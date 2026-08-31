// Package xds translates a node's NetDesired into go-control-plane
// snapshots and serves them to that node's Envoy over ADS (ADR 0007).
package xds

import (
	"fmt"
	"slices"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbaccfgv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	rbacnetv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/rbac/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const (
	rbacFilterName     = "envoy.filters.network.rbac"
	tcpProxyFilterName = "envoy.filters.network.tcp_proxy"

	// anyService is the wildcard in CompiledPolicy from/to fields.
	anyService = "*"
)

// BuildSnapshot turns net into a validated xDS snapshot for node. Distinct
// version strings per change are the caller's job (pass the desired-state
// revision); Envoy ignores a snapshot whose version it already ACKed.
func BuildSnapshot(node, version string, net *klitev1.NetDesired) (*cachev3.Snapshot, error) {
	listeners, err := buildListeners(net)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	snap, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  buildClusters(net),
		resourcev3.EndpointType: buildEndpoints(net),
		resourcev3.ListenerType: listeners,
	})
	if err != nil {
		return nil, fmt.Errorf("node %s: new snapshot: %w", node, err)
	}
	if err := snap.Consistent(); err != nil {
		return nil, fmt.Errorf("node %s: inconsistent snapshot: %w", node, err)
	}
	return snap, nil
}

func buildListeners(net *klitev1.NetDesired) ([]types.Resource, error) {
	out := make([]types.Resource, 0, len(net.GetServices()))
	for _, svc := range net.GetServices() {
		lst, err := buildListener(svc, net)
		if err != nil {
			return nil, err
		}
		out = append(out, lst)
	}
	return out, nil
}

func buildListener(svc *klitev1.ServiceVIP, net *klitev1.NetDesired) (*listenerv3.Listener, error) {
	filters, err := rbacFilters(svc.GetService(), net)
	if err != nil {
		return nil, err
	}
	tcpProxy, err := anypb.New(&tcpproxyv3.TcpProxy{
		StatPrefix:       svc.GetService(),
		ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: svc.GetService()},
		// The 1h default silently kills idle connections (research/envoy-xds.md).
		IdleTimeout: durationpb.New(0),
	})
	if err != nil {
		return nil, fmt.Errorf("listener svc/%s: %w", svc.GetService(), err)
	}
	filters = append(filters, &listenerv3.Filter{
		Name:       tcpProxyFilterName,
		ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: tcpProxy},
	})
	return &listenerv3.Listener{
		Name: "svc/" + svc.GetService(),
		// Freebind lets Envoy bind the VIP before it exists on any interface.
		Freebind:     wrapperspb.Bool(true),
		Address:      socketAddress(svc.GetVip(), uint32(svc.GetPort())),
		FilterChains: []*listenerv3.FilterChain{{Filters: filters}},
	}, nil
}

// rbacFilters compiles the istio-lite policy table (ADR 0009) for the
// listener of destination service: a DENY filter when any deny policy
// yields principals, then an ALLOW filter whenever an allow policy targets
// the service (allowlist mode), even if no principals remain — an empty
// ALLOW filter admits nobody, which is the correct flip semantics.
func rbacFilters(service string, net *klitev1.NetDesired) ([]*listenerv3.Filter, error) {
	deny := map[string]*rbaccfgv3.Policy{}
	allow := map[string]*rbaccfgv3.Policy{}
	allowlist := false
	for i, pol := range net.GetPolicies() {
		if !policyTargets(pol, service) {
			continue
		}
		switch pol.GetAction() {
		case klitev1.PolicyAction_POLICY_ACTION_DENY:
			addPolicy(deny, i, pol, net.GetIpIdentity())
		case klitev1.PolicyAction_POLICY_ACTION_ALLOW:
			allowlist = true
			addPolicy(allow, i, pol, net.GetIpIdentity())
		}
	}
	var filters []*listenerv3.Filter
	if len(deny) > 0 {
		f, err := rbacFilter(service+"_deny", rbaccfgv3.RBAC_DENY, deny)
		if err != nil {
			return nil, err
		}
		filters = append(filters, f)
	}
	if allowlist {
		f, err := rbacFilter(service+"_allow", rbaccfgv3.RBAC_ALLOW, allow)
		if err != nil {
			return nil, err
		}
		filters = append(filters, f)
	}
	return filters, nil
}

func policyTargets(pol *klitev1.CompiledPolicy, service string) bool {
	if pol.GetTo() == service {
		return true
	}
	return pol.GetTo() == anyService && !slices.Contains(pol.GetExcept(), service)
}

// addPolicy translates one CompiledPolicy into an Envoy RBAC policy. A from
// service with zero current instances yields zero principals; the policy is
// dropped because Envoy rejects principal-less policies, and matching
// nothing is the correct semantics anyway.
func addPolicy(policies map[string]*rbaccfgv3.Policy, index int, pol *klitev1.CompiledPolicy, ipIdentity map[string]string) {
	principals := principalsFor(pol.GetFrom(), ipIdentity)
	if len(principals) == 0 {
		return
	}
	key := fmt.Sprintf("%03d-%s", index, pol.GetPolicyName())
	policies[key] = &rbaccfgv3.Policy{
		Permissions: []*rbaccfgv3.Permission{{Rule: &rbaccfgv3.Permission_Any{Any: true}}},
		Principals:  principals,
	}
}

func principalsFor(from string, ipIdentity map[string]string) []*rbaccfgv3.Principal {
	if from == anyService {
		return []*rbaccfgv3.Principal{{Identifier: &rbaccfgv3.Principal_Any{Any: true}}}
	}
	var ips []string
	for ip, svc := range ipIdentity {
		if svc == from {
			ips = append(ips, ip)
		}
	}
	slices.Sort(ips)
	principals := make([]*rbaccfgv3.Principal, 0, len(ips))
	for _, ip := range ips {
		principals = append(principals, &rbaccfgv3.Principal{
			Identifier: &rbaccfgv3.Principal_DirectRemoteIp{
				DirectRemoteIp: &corev3.CidrRange{
					AddressPrefix: ip,
					PrefixLen:     wrapperspb.UInt32(32),
				},
			},
		})
	}
	return principals
}

func rbacFilter(statPrefix string, action rbaccfgv3.RBAC_Action, policies map[string]*rbaccfgv3.Policy) (*listenerv3.Filter, error) {
	cfg, err := anypb.New(&rbacnetv3.RBAC{
		StatPrefix: statPrefix,
		Rules:      &rbaccfgv3.RBAC{Action: action, Policies: policies},
	})
	if err != nil {
		return nil, fmt.Errorf("rbac filter %s: %w", statPrefix, err)
	}
	return &listenerv3.Filter{
		Name:       rbacFilterName,
		ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: cfg},
	}, nil
}

func buildClusters(net *klitev1.NetDesired) []types.Resource {
	out := make([]types.Resource, 0, len(net.GetServices()))
	for _, svc := range net.GetServices() {
		out = append(out, buildCluster(svc.GetService()))
	}
	return out
}

func buildCluster(service string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 service,
		ConnectTimeout:       durationpb.New(time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_EDS},
		LbPolicy:             clusterv3.Cluster_ROUND_ROBIN,
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: &corev3.ConfigSource{
				ResourceApiVersion:    corev3.ApiVersion_V3,
				ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
			},
		},
		// Mandatory: the 50% default panic threshold ignores health the
		// moment one endpoint of two drains (ADR 0010, research/envoy-xds.md).
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &typev3.Percent{Value: 0},
		},
	}
}

// buildEndpoints emits one ClusterLoadAssignment per endpoint group plus an
// empty one for each service with no group, so a zero-instance service still
// passes the snapshot consistency check.
func buildEndpoints(net *klitev1.NetDesired) []types.Resource {
	out := make([]types.Resource, 0, len(net.GetServices()))
	seen := make(map[string]bool, len(net.GetServices()))
	for _, group := range net.GetEndpoints() {
		seen[group.GetService()] = true
		out = append(out, buildLoadAssignment(group))
	}
	for _, svc := range net.GetServices() {
		if seen[svc.GetService()] {
			continue
		}
		seen[svc.GetService()] = true
		out = append(out, &endpointv3.ClusterLoadAssignment{ClusterName: svc.GetService()})
	}
	return out
}

func buildLoadAssignment(group *klitev1.EndpointGroup) *endpointv3.ClusterLoadAssignment {
	lbs := make([]*endpointv3.LbEndpoint, 0, len(group.GetEndpoints()))
	for _, ep := range group.GetEndpoints() {
		lbs = append(lbs, &endpointv3.LbEndpoint{
			HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
				Endpoint: &endpointv3.Endpoint{
					Address: socketAddress(ep.GetIp(), uint32(ep.GetPort())),
				},
			},
			HealthStatus: healthStatus(ep.GetHealth()),
		})
	}
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: group.GetService(),
		Endpoints:   []*endpointv3.LocalityLbEndpoints{{LbEndpoints: lbs}},
	}
}

// healthStatus maps endpoint health explicitly: the Envoy default UNKNOWN
// counts as healthy, so READY is pinned to HEALTHY rather than left unset.
func healthStatus(h klitev1.EndpointHealth) corev3.HealthStatus {
	if h == klitev1.EndpointHealth_ENDPOINT_HEALTH_DRAINING {
		return corev3.HealthStatus_DRAINING
	}
	return corev3.HealthStatus_HEALTHY
}

func socketAddress(host string, port uint32) *corev3.Address {
	return &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol:      corev3.SocketAddress_TCP,
				Address:       host,
				PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
			},
		},
	}
}

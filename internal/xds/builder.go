// Package xds translates a node's NetDesired into go-control-plane
// snapshots and serves them to that node's Envoy over ADS (ADR 0007).
package xds

import (
	"fmt"
	"net/netip"
	"slices"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbaccfgv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	rbacnetv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/rbac/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	rawbufferv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/raw_buffer/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const (
	rbacFilterName     = "envoy.filters.network.rbac"
	tcpProxyFilterName = "envoy.filters.network.tcp_proxy"

	// anyService is the wildcard in CompiledPolicy from/to fields.
	anyService = "*"

	// tlsMountDir is where the infra pod bind-mounts the node identity
	// (the agent's tlsMount). Ingress and upstream TLS read the same trio
	// Envoy already dials xDS with (ADR 0013, ADR 0024).
	tlsMountDir  = "/etc/klite/tls"
	nodeCertFile = tlsMountDir + "/node.crt"
	nodeKeyFile  = tlsMountDir + "/node.key"
	caCertFile   = tlsMountDir + "/ca.crt"

	tlsSocketName = "envoy.transport_sockets.tls"
	rawSocketName = "envoy.transport_sockets.raw_buffer"

	// transportMatchMetadataKey is Envoy's fixed filter-metadata namespace
	// for endpoint transport selection, and ingressMatchKey/Value is our one
	// entry in it. Remote endpoints carry it, local ones don't, and the
	// cluster's match list turns that into mTLS-or-raw. The list is static
	// so CDS never churns when endpoints move — churn would drain the
	// cluster's connections and break hitless rollouts.
	transportMatchMetadataKey = "envoy.transport_socket_match"
	ingressMatchKey           = "klite"
	ingressMatchValue         = "ingress-mtls"
)

// BuildSnapshot turns net into a validated xDS snapshot for node. Distinct
// version strings per change are the caller's job (pass the desired-state
// revision), since Envoy ignores a snapshot whose version it already ACKed.
// node decides sidedness (ADR 0024): endpoints on it get ingress listeners
// here, endpoints elsewhere render as mTLS dials to their ingress ports.
func BuildSnapshot(node, version string, net *klitev1.NetDesired) (*cachev3.Snapshot, error) {
	if err := validateNet(net); err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	listeners, err := buildListeners(net)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	ingressListeners, ingressClusters, err := buildIngress(net)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	clusters, err := buildClusters(net)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	snap, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  append(clusters, ingressClusters...),
		resourcev3.EndpointType: buildEndpoints(node, net),
		resourcev3.ListenerType: append(listeners, ingressListeners...),
	})
	if err != nil {
		return nil, fmt.Errorf("node %s: new snapshot: %w", node, err)
	}
	if err := snap.Consistent(); err != nil {
		return nil, fmt.Errorf("node %s: inconsistent snapshot: %w", node, err)
	}
	return snap, nil
}

// validateNet screens the wire input before any resource is built. Bad IPs
// and ports pass snapshot.Consistent() (it checks name cross-references, not
// values) and blow up later as an Envoy NACK, which nothing used to log. The
// node just sat on its last ACKed config. Rejecting here keeps the previous
// snapshot serving and puts the reason in klited's log. Duplicate names are
// rejected too, because snapshot indexing is last-wins and would drop the
// earlier resource.
func validateNet(net *klitev1.NetDesired) error {
	seenSvc := make(map[string]bool, len(net.GetServices()))
	for _, svc := range net.GetServices() {
		name := svc.GetService()
		if name == "" {
			return fmt.Errorf("service with empty name (vip %q)", svc.GetVip())
		}
		if seenSvc[name] {
			return fmt.Errorf("service %q listed twice", name)
		}
		seenSvc[name] = true
		if _, err := netip.ParseAddr(svc.GetVip()); err != nil {
			return fmt.Errorf("service %s: vip: %w", name, err)
		}
		if p := svc.GetPort(); p < 1 || p > 65535 {
			return fmt.Errorf("service %s: port %d outside 1-65535", name, p)
		}
	}
	if err := validateEndpointGroups(net); err != nil {
		return err
	}
	if err := validateIngressListeners(net); err != nil {
		return err
	}
	for ip := range net.GetIpIdentity() {
		if _, err := netip.ParseAddr(ip); err != nil {
			return fmt.Errorf("ip_identity key %q: %w", ip, err)
		}
	}
	return nil
}

// validateEndpointGroups screens each group: base ips and ports, duplicated
// group names, and the cross-node riders, where a machine address must be a
// literal IP because it lands in EDS.
func validateEndpointGroups(net *klitev1.NetDesired) error {
	seenGroup := make(map[string]bool, len(net.GetEndpoints()))
	for _, group := range net.GetEndpoints() {
		svc := group.GetService()
		if seenGroup[svc] {
			return fmt.Errorf("endpoint group %q listed twice", svc)
		}
		seenGroup[svc] = true
		for _, ep := range group.GetEndpoints() {
			if _, err := netip.ParseAddr(ep.GetIp()); err != nil {
				return fmt.Errorf("endpoint for %s: ip: %w", svc, err)
			}
			if p := ep.GetPort(); p < 1 || p > 65535 {
				return fmt.Errorf("endpoint for %s: port %d outside 1-65535", svc, p)
			}
			if addr := ep.GetMachineAddress(); addr != "" {
				if _, err := netip.ParseAddr(addr); err != nil {
					return fmt.Errorf("endpoint for %s: machine address: %w", svc, err)
				}
			}
			if p := ep.GetIngressPort(); p < 0 || p > 65535 {
				return fmt.Errorf("endpoint for %s: ingress port %d outside 0-65535", svc, p)
			}
		}
	}
	return nil
}

// validateIngressListeners rejects listeners that would NACK (bad addresses,
// bad ports) or silently drop each other: two entries on one port become two
// listeners that share a name or an address, and snapshot indexing is
// last-wins. Allocator repair fixes the store within a pass, and the node
// serves its previous snapshot until then.
func validateIngressListeners(net *klitev1.NetDesired) error {
	seen := map[int32]bool{}
	for _, ing := range net.GetIngressListeners() {
		if ing.GetService() == "" {
			return fmt.Errorf("ingress listener on port %d without a service", ing.GetPort())
		}
		if p := ing.GetPort(); p < 1 || p > 65535 {
			return fmt.Errorf("ingress %s: port %d outside 1-65535", ing.GetService(), p)
		}
		if p := ing.GetTargetPort(); p < 1 || p > 65535 {
			return fmt.Errorf("ingress %s: target port %d outside 1-65535", ing.GetService(), p)
		}
		if _, err := netip.ParseAddr(ing.GetPodIp()); err != nil {
			return fmt.Errorf("ingress %s: pod ip: %w", ing.GetService(), err)
		}
		if seen[ing.GetPort()] {
			return fmt.Errorf("ingress port %d claimed by two local endpoints", ing.GetPort())
		}
		seen[ing.GetPort()] = true
	}
	return nil
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
// destination service's listener: a DENY filter when any deny policy yields
// principals, then an ALLOW filter whenever an allow policy targets the
// service (allowlist mode). The ALLOW filter stays even when no principals
// remain, because an empty ALLOW filter admits nobody, which is the correct
// flip semantics.
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
// service with zero current instances yields zero principals. The policy is
// then dropped because Envoy rejects principal-less policies, and matching
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
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue // validateNet already rejected configs carrying these
		}
		// The prefix length must follow the address family: /32 over a v6
		// address would match a whole /32 of v6 space, not one host.
		principals = append(principals, &rbaccfgv3.Principal{
			Identifier: &rbaccfgv3.Principal_DirectRemoteIp{
				DirectRemoteIp: &corev3.CidrRange{
					AddressPrefix: ip,
					PrefixLen:     wrapperspb.UInt32(uint32(addr.BitLen())),
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

func buildClusters(net *klitev1.NetDesired) ([]types.Resource, error) {
	matches, err := transportSocketMatches()
	if err != nil {
		return nil, err
	}
	out := make([]types.Resource, 0, len(net.GetServices()))
	for _, svc := range net.GetServices() {
		out = append(out, buildCluster(svc.GetService(), matches))
	}
	return out, nil
}

func buildCluster(service string, matches []*clusterv3.Cluster_TransportSocketMatch) *clusterv3.Cluster {
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
		// The default 50% panic threshold ignores health the moment one
		// endpoint of two drains, so zero is mandatory (ADR 0010,
		// research/envoy-xds.md).
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &typev3.Percent{Value: 0},
		},
		TransportSocketMatches: matches,
	}
}

// transportSocketMatches lets one EDS cluster mix plain local endpoints with
// mTLS remote ones (ADR 0024). Endpoints tagged ingress-mtls dial with the
// node identity. The trailing empty match pins everything else to a raw
// socket explicitly rather than leaning on fallback-to-default semantics.
func transportSocketMatches() ([]*clusterv3.Cluster_TransportSocketMatch, error) {
	mtls, err := upstreamMTLSSocket()
	if err != nil {
		return nil, err
	}
	raw, err := anypb.New(&rawbufferv3.RawBuffer{})
	if err != nil {
		return nil, fmt.Errorf("raw buffer socket: %w", err)
	}
	return []*clusterv3.Cluster_TransportSocketMatch{
		{
			Name: ingressMatchValue,
			Match: &structpb.Struct{Fields: map[string]*structpb.Value{
				ingressMatchKey: structpb.NewStringValue(ingressMatchValue),
			}},
			TransportSocket: mtls,
		},
		{
			Name:  "raw",
			Match: &structpb.Struct{},
			TransportSocket: &corev3.TransportSocket{
				Name:       rawSocketName,
				ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: raw},
			},
		},
	}, nil
}

// commonNodeTLS is both directions' shared identity: present the node cert,
// trust only peers chaining to the cluster CA, and speak TLS 1.3 alone,
// matching klited's floor and the xDS bootstrap's pins (ADR 0013).
func commonNodeTLS() *tlsv3.CommonTlsContext {
	return &tlsv3.CommonTlsContext{
		TlsParams: &tlsv3.TlsParameters{
			TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
			TlsMaximumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
		},
		TlsCertificates: []*tlsv3.TlsCertificate{{
			CertificateChain: fileDataSource(nodeCertFile),
			PrivateKey:       fileDataSource(nodeKeyFile),
		}},
		ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
			ValidationContext: &tlsv3.CertificateValidationContext{TrustedCa: fileDataSource(caCertFile)},
		},
	}
}

// upstreamMTLSSocket dials a remote node's ingress listener. No SNI and no
// SAN pinning: node certs carry only the klite:node:<name> CN, and identity
// is deliberately node-level in v1 (ADR 0024 records the gap).
func upstreamMTLSSocket() (*corev3.TransportSocket, error) {
	cfg, err := anypb.New(&tlsv3.UpstreamTlsContext{CommonTlsContext: commonNodeTLS()})
	if err != nil {
		return nil, fmt.Errorf("upstream tls socket: %w", err)
	}
	return &corev3.TransportSocket{
		Name:       tlsSocketName,
		ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: cfg},
	}, nil
}

func fileDataSource(path string) *corev3.DataSource {
	return &corev3.DataSource{Specifier: &corev3.DataSource_Filename{Filename: path}}
}

// buildEndpoints emits one ClusterLoadAssignment per endpoint group plus an
// empty one for each service with no group, so a zero-instance service still
// passes the snapshot consistency check.
func buildEndpoints(node string, net *klitev1.NetDesired) []types.Resource {
	out := make([]types.Resource, 0, len(net.GetServices()))
	seen := make(map[string]bool, len(net.GetServices()))
	for _, group := range net.GetEndpoints() {
		seen[group.GetService()] = true
		out = append(out, buildLoadAssignment(node, group))
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

// buildLoadAssignment renders one group for the consuming node. Endpoints on
// the node itself stay raw pod addresses, while endpoints elsewhere become
// mTLS dials to machineAddress:ingressPort, the only cross-node path
// (ADR 0024).
// A remote endpoint whose allocation or machine address hasn't landed yet is
// left out: no ingress rider means no reachable path, and the flat-bridge
// shortcut is gone. Health carries over unchanged either way, so DRAINING
// stays a source-side EDS state across the ingress hop (ADR 0010).
func buildLoadAssignment(node string, group *klitev1.EndpointGroup) *endpointv3.ClusterLoadAssignment {
	lbs := make([]*endpointv3.LbEndpoint, 0, len(group.GetEndpoints()))
	for _, ep := range group.GetEndpoints() {
		lb := &endpointv3.LbEndpoint{HealthStatus: healthStatus(ep.GetHealth())}
		switch {
		case ep.GetNode() == "" || ep.GetNode() == node:
			lb.HostIdentifier = lbAddress(ep.GetIp(), uint32(ep.GetPort()))
		case ep.GetIngressPort() > 0 && ep.GetMachineAddress() != "":
			lb.HostIdentifier = lbAddress(ep.GetMachineAddress(), uint32(ep.GetIngressPort()))
			lb.Metadata = ingressMatchMetadata()
		default:
			continue // remote and not yet dialable
		}
		lbs = append(lbs, lb)
	}
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: group.GetService(),
		Endpoints:   []*endpointv3.LocalityLbEndpoints{{LbEndpoints: lbs}},
	}
}

func lbAddress(host string, port uint32) *endpointv3.LbEndpoint_Endpoint {
	return &endpointv3.LbEndpoint_Endpoint{
		Endpoint: &endpointv3.Endpoint{Address: socketAddress(host, port)},
	}
}

// ingressMatchMetadata tags a remote endpoint for the cluster's ingress-mtls
// transport socket match.
func ingressMatchMetadata() *corev3.Metadata {
	return &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
		transportMatchMetadataKey: {Fields: map[string]*structpb.Value{
			ingressMatchKey: structpb.NewStringValue(ingressMatchValue),
		}},
	}}
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

// buildIngress emits the destination half of ADR 0024 from the node's
// allocation-driven listener list: one listener on 0.0.0.0:<ingressPort>
// inside the donor's published slice. Each terminates TLS with the node
// identity, requires a client cert that chains to the cluster CA, and
// tcp-proxies to a one-endpoint static cluster at the local instance. The
// list stands from instance birth through draining, so consumers never
// route at a port whose listener hasn't committed yet. No RBAC here: policy
// runs where the connection originates, on the source node's VIP listener,
// and admission at this layer is deliberately node-level. These also skip
// freebind, unlike the VIP listeners, because 0.0.0.0 always exists.
func buildIngress(net *klitev1.NetDesired) (listeners, clusters []types.Resource, err error) {
	for _, ing := range net.GetIngressListeners() {
		lst, cl, err := buildIngressPair(ing)
		if err != nil {
			return nil, nil, err
		}
		listeners = append(listeners, lst)
		clusters = append(clusters, cl)
	}
	return listeners, clusters, nil
}

func buildIngressPair(ing *klitev1.IngressListener) (*listenerv3.Listener, *clusterv3.Cluster, error) {
	// The port names both resources: it's unique on the node (the allocator
	// hands each endpoint its own), while instance names never reach this
	// layer.
	name := fmt.Sprintf("ingress/%s/%d", ing.GetService(), ing.GetPort())
	tcpProxy, err := anypb.New(&tcpproxyv3.TcpProxy{
		StatPrefix:       fmt.Sprintf("ingress_%d", ing.GetPort()),
		ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: name},
		// Same deliberate zero as the VIP listeners: the 1h default
		// silently kills idle connections.
		IdleTimeout: durationpb.New(0),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listener %s: %w", name, err)
	}
	downstream, err := anypb.New(&tlsv3.DownstreamTlsContext{
		CommonTlsContext: commonNodeTLS(),
		// The whole point: a plaintext dial or a foreign-CA cert dies in
		// the handshake, before any byte reaches the instance.
		RequireClientCertificate: wrapperspb.Bool(true),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listener %s: %w", name, err)
	}
	lst := &listenerv3.Listener{
		Name:    name,
		Address: socketAddress("0.0.0.0", uint32(ing.GetPort())),
		FilterChains: []*listenerv3.FilterChain{{
			TransportSocket: &corev3.TransportSocket{
				Name:       tlsSocketName,
				ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: downstream},
			},
			Filters: []*listenerv3.Filter{{
				Name:       tcpProxyFilterName,
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: tcpProxy},
			}},
		}},
	}
	cl := &clusterv3.Cluster{
		Name:                 name,
		ConnectTimeout:       durationpb.New(time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: lbAddress(ing.GetPodIp(), uint32(ing.GetTargetPort())),
				}},
			}},
		},
	}
	return lst, cl, nil
}

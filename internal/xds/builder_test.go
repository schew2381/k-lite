package xds_test

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbaccfgv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	rbacnetv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/rbac/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/xds"
)

const (
	rbacName = "envoy.filters.network.rbac"
	tcpName  = "envoy.filters.network.tcp_proxy"
)

// testNet is a two-service node: services a and b have VIPs and endpoints,
// and ip_identity also knows one instance of source-only service c.
func testNet(policies ...*klitev1.CompiledPolicy) *klitev1.NetDesired {
	return &klitev1.NetDesired{
		Services: []*klitev1.ServiceVIP{
			{Service: "a", Vip: "10.44.64.1", Port: 80, TargetPort: 8080},
			{Service: "b", Vip: "10.44.64.2", Port: 5432, TargetPort: 5432},
		},
		Endpoints: []*klitev1.EndpointGroup{
			{Service: "a", Endpoints: []*klitev1.Endpoint{
				{Ip: "10.88.0.1", Port: 8080, Node: "node-1", Health: klitev1.EndpointHealth_ENDPOINT_HEALTH_READY},
				{Ip: "10.88.0.2", Port: 8080, Node: "node-1", Health: klitev1.EndpointHealth_ENDPOINT_HEALTH_DRAINING},
			}},
			{Service: "b", Endpoints: []*klitev1.Endpoint{
				{Ip: "10.88.0.3", Port: 5432, Node: "node-1", Health: klitev1.EndpointHealth_ENDPOINT_HEALTH_READY},
			}},
		},
		IpIdentity: map[string]string{
			"10.88.0.1": "a",
			"10.88.0.2": "a",
			"10.88.0.3": "b",
			"10.88.0.9": "c",
		},
		Policies: policies,
	}
}

func deny(from, to string, except ...string) *klitev1.CompiledPolicy {
	return &klitev1.CompiledPolicy{
		Action: klitev1.PolicyAction_POLICY_ACTION_DENY,
		From:   from, To: to, Except: except, PolicyName: "deny-" + from,
	}
}

func allow(from, to string, except ...string) *klitev1.CompiledPolicy {
	return &klitev1.CompiledPolicy{
		Action: klitev1.PolicyAction_POLICY_ACTION_ALLOW,
		From:   from, To: to, Except: except, PolicyName: "allow-" + from,
	}
}

func getListener(t *testing.T, snap *cachev3.Snapshot, name string) *listenerv3.Listener {
	t.Helper()
	res, ok := snap.GetResources(resourcev3.ListenerType)[name]
	if !ok {
		t.Fatalf("listener %q not in snapshot", name)
	}
	lst, ok := res.(*listenerv3.Listener)
	if !ok {
		t.Fatalf("resource %q is %T, want *listenerv3.Listener", name, res)
	}
	return lst
}

type rbacView struct {
	action     rbaccfgv3.RBAC_Action
	principals [][]string // per policy in key order, "*" for any, else "ip/len"
}

// decodeFilters flattens the single filter chain into filter names in order,
// decoded rbac filters, and the tcp_proxy target cluster.
func decodeFilters(t *testing.T, lst *listenerv3.Listener) ([]string, []rbacView, string) {
	t.Helper()
	chains := lst.GetFilterChains()
	if len(chains) != 1 {
		t.Fatalf("listener %s: want 1 filter chain, got %d", lst.GetName(), len(chains))
	}
	var (
		names      []string
		rbacs      []rbacView
		tcpCluster string
	)
	for _, f := range chains[0].GetFilters() {
		names = append(names, f.GetName())
		switch f.GetName() {
		case rbacName:
			cfg := new(rbacnetv3.RBAC)
			if err := f.GetTypedConfig().UnmarshalTo(cfg); err != nil {
				t.Fatalf("decode rbac: %v", err)
			}
			rbacs = append(rbacs, rbacView{
				action:     cfg.GetRules().GetAction(),
				principals: principalStrings(cfg.GetRules().GetPolicies()),
			})
		case tcpName:
			cfg := new(tcpproxyv3.TcpProxy)
			if err := f.GetTypedConfig().UnmarshalTo(cfg); err != nil {
				t.Fatalf("decode tcp_proxy: %v", err)
			}
			tcpCluster = cfg.GetCluster()
		default:
			t.Fatalf("unexpected filter %q", f.GetName())
		}
	}
	return names, rbacs, tcpCluster
}

func principalStrings(policies map[string]*rbaccfgv3.Policy) [][]string {
	out := make([][]string, 0, len(policies))
	for _, key := range slices.Sorted(maps.Keys(policies)) {
		var ps []string
		for _, pr := range policies[key].GetPrincipals() {
			if pr.GetAny() {
				ps = append(ps, "*")
				continue
			}
			cidr := pr.GetDirectRemoteIp()
			ps = append(ps, fmt.Sprintf("%s/%d", cidr.GetAddressPrefix(), cidr.GetPrefixLen().GetValue()))
		}
		out = append(out, ps)
	}
	return out
}

type endpointWant struct {
	addr   string
	port   uint32
	health corev3.HealthStatus
}

func TestBuildSnapshotTwoServices(t *testing.T) {
	t.Parallel()
	snap, err := xds.BuildSnapshot("node-1", "rev-7", testNet())
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	for _, typ := range []string{resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.EndpointType} {
		if got := snap.GetVersion(typ); got != "rev-7" {
			t.Errorf("version for %s = %q, want rev-7", typ, got)
		}
		if got := len(snap.GetResources(typ)); got != 2 {
			t.Errorf("resource count for %s = %d, want 2", typ, got)
		}
	}

	tests := []struct {
		service   string
		vip       string
		port      uint32
		endpoints []endpointWant
	}{
		{
			service: "a", vip: "10.44.64.1", port: 80,
			endpoints: []endpointWant{
				{addr: "10.88.0.1", port: 8080, health: corev3.HealthStatus_HEALTHY},
				{addr: "10.88.0.2", port: 8080, health: corev3.HealthStatus_DRAINING},
			},
		},
		{
			service: "b", vip: "10.44.64.2", port: 5432,
			endpoints: []endpointWant{
				{addr: "10.88.0.3", port: 5432, health: corev3.HealthStatus_HEALTHY},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			t.Parallel()
			assertServiceListener(t, snap, tt.service, tt.vip, tt.port)
			assertServiceCluster(t, snap, tt.service)
			assertServiceEndpoints(t, snap, tt.service, tt.endpoints)
		})
	}
}

func assertServiceListener(t *testing.T, snap *cachev3.Snapshot, service, vip string, port uint32) {
	t.Helper()
	lst := getListener(t, snap, "svc/"+service)
	if !lst.GetFreebind().GetValue() {
		t.Error("freebind not set")
	}
	sock := lst.GetAddress().GetSocketAddress()
	if sock.GetAddress() != vip || sock.GetPortValue() != port {
		t.Errorf("listener address = %s:%d, want %s:%d",
			sock.GetAddress(), sock.GetPortValue(), vip, port)
	}
	names, rbacs, tcpCluster := decodeFilters(t, lst)
	if !slices.Equal(names, []string{tcpName}) {
		t.Errorf("filters = %v, want only tcp_proxy", names)
	}
	if len(rbacs) != 0 {
		t.Errorf("got %d rbac filters, want none", len(rbacs))
	}
	if tcpCluster != service {
		t.Errorf("tcp_proxy cluster = %q, want %q", tcpCluster, service)
	}
}

func assertServiceCluster(t *testing.T, snap *cachev3.Snapshot, service string) {
	t.Helper()
	res := snap.GetResources(resourcev3.ClusterType)[service]
	if res == nil {
		t.Fatalf("cluster %q missing", service)
	}
	cl := res.(*clusterv3.Cluster)
	if cl.GetType() != clusterv3.Cluster_EDS {
		t.Errorf("cluster type = %v, want EDS", cl.GetType())
	}
	if got := cl.GetConnectTimeout().AsDuration(); got != time.Second {
		t.Errorf("connect timeout = %v, want 1s", got)
	}
	if cl.GetLbPolicy() != clusterv3.Cluster_ROUND_ROBIN {
		t.Errorf("lb policy = %v, want ROUND_ROBIN", cl.GetLbPolicy())
	}
	panicThreshold := cl.GetCommonLbConfig().GetHealthyPanicThreshold()
	if panicThreshold == nil {
		t.Fatal("healthy_panic_threshold not set: the 50% default breaks draining")
	}
	if panicThreshold.GetValue() != 0 {
		t.Errorf("healthy_panic_threshold = %v, want 0", panicThreshold.GetValue())
	}
	if cl.GetEdsClusterConfig().GetEdsConfig().GetAds() == nil {
		t.Error("eds config does not use ADS")
	}
}

func assertServiceEndpoints(t *testing.T, snap *cachev3.Snapshot, service string, want []endpointWant) {
	t.Helper()
	res := snap.GetResources(resourcev3.EndpointType)[service]
	if res == nil {
		t.Fatalf("load assignment %q missing", service)
	}
	cla := res.(*endpointv3.ClusterLoadAssignment)
	var got []endpointWant
	for _, loc := range cla.GetEndpoints() {
		for _, lb := range loc.GetLbEndpoints() {
			sock := lb.GetEndpoint().GetAddress().GetSocketAddress()
			got = append(got, endpointWant{
				addr:   sock.GetAddress(),
				port:   sock.GetPortValue(),
				health: lb.GetHealthStatus(),
			})
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("endpoints = %+v, want %+v", got, want)
	}
}

func TestBuildSnapshotZeroInstanceService(t *testing.T) {
	t.Parallel()
	net := testNet()
	net.Endpoints = net.Endpoints[:1] // b keeps its VIP but has no instances
	snap, err := xds.BuildSnapshot("node-1", "1", net)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	res := snap.GetResources(resourcev3.EndpointType)["b"]
	if res == nil {
		t.Fatal("empty load assignment for b missing")
	}
	cla := res.(*endpointv3.ClusterLoadAssignment)
	for _, loc := range cla.GetEndpoints() {
		if n := len(loc.GetLbEndpoints()); n != 0 {
			t.Errorf("got %d endpoints for b, want 0", n)
		}
	}
}

// A departed node decays to an empty NetDesired, which must still build a
// valid, empty snapshot. The endpoints engine keeps pushing it.
func TestBuildSnapshotEmptyNet(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		net  *klitev1.NetDesired
	}{
		{name: "empty message", net: &klitev1.NetDesired{}},
		{name: "nil message", net: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snap, err := xds.BuildSnapshot("node-gone", "9", tt.net)
			if err != nil {
				t.Fatalf("BuildSnapshot: %v", err)
			}
			for _, typ := range []string{resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.EndpointType} {
				if got := len(snap.GetResources(typ)); got != 0 {
					t.Errorf("resource count for %s = %d, want 0", typ, got)
				}
			}
		})
	}
}

// Garbage that Consistent() can't see (bad IPs, bad ports, colliding names)
// must fail BuildSnapshot rather than ride out to Envoy as a NACK.
func TestBuildSnapshotRejectsGarbage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(net *klitev1.NetDesired)
		wantErr string
	}{
		{
			name: "empty service name",
			mutate: func(net *klitev1.NetDesired) {
				net.Services[0].Service = ""
			},
			wantErr: "empty name",
		},
		{
			name: "duplicate service",
			mutate: func(net *klitev1.NetDesired) {
				net.Services = append(net.Services, &klitev1.ServiceVIP{
					Service: "a", Vip: "10.44.64.3", Port: 81,
				})
			},
			wantErr: "listed twice",
		},
		{
			name: "vip not an ip",
			mutate: func(net *klitev1.NetDesired) {
				net.Services[0].Vip = "nope"
			},
			wantErr: "vip",
		},
		{
			name: "listener port zero",
			mutate: func(net *klitev1.NetDesired) {
				net.Services[0].Port = 0
			},
			wantErr: "port 0",
		},
		{
			name: "listener port negative",
			mutate: func(net *klitev1.NetDesired) {
				net.Services[0].Port = -80
			},
			wantErr: "outside 1-65535",
		},
		{
			name: "listener port too big",
			mutate: func(net *klitev1.NetDesired) {
				net.Services[0].Port = 70000
			},
			wantErr: "outside 1-65535",
		},
		{
			name: "duplicate endpoint group",
			mutate: func(net *klitev1.NetDesired) {
				net.Endpoints = append(net.Endpoints, &klitev1.EndpointGroup{Service: "a"})
			},
			wantErr: "listed twice",
		},
		{
			name: "endpoint ip garbage",
			mutate: func(net *klitev1.NetDesired) {
				net.Endpoints[0].Endpoints[0].Ip = "10.88.0.999"
			},
			wantErr: "ip",
		},
		{
			name: "endpoint port out of range",
			mutate: func(net *klitev1.NetDesired) {
				net.Endpoints[0].Endpoints[0].Port = 65536
			},
			wantErr: "outside 1-65535",
		},
		{
			name: "ip_identity key garbage",
			mutate: func(net *klitev1.NetDesired) {
				net.IpIdentity["not-an-ip"] = "a"
			},
			wantErr: "ip_identity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			net := testNet()
			tc.mutate(net)
			_, err := xds.BuildSnapshot("node-1", "1", net)
			if err == nil {
				t.Fatal("BuildSnapshot accepted garbage")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "node-1") {
				t.Errorf("error = %q, want it to name the node", err)
			}
		})
	}
}

// A v6 source gets a /128 principal. A /32 over v6 would match a whole /32
// of address space instead of one host.
func TestRBACPrincipalV6(t *testing.T) {
	t.Parallel()
	net := testNet(deny("v6svc", "b"))
	net.IpIdentity["fd00::9"] = "v6svc"
	snap, err := xds.BuildSnapshot("node-1", "1", net)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	_, rbacs, _ := decodeFilters(t, getListener(t, snap, "svc/b"))
	if len(rbacs) != 1 {
		t.Fatalf("got %d rbac filters, want 1", len(rbacs))
	}
	want := [][]string{{"fd00::9/128"}}
	if !slices.EqualFunc(rbacs[0].principals, want, slices.Equal) {
		t.Errorf("principals = %v, want %v", rbacs[0].principals, want)
	}
}

func TestBuildSnapshotInconsistent(t *testing.T) {
	t.Parallel()
	net := testNet()
	net.Endpoints = append(net.Endpoints, &klitev1.EndpointGroup{
		Service: "ghost",
		Endpoints: []*klitev1.Endpoint{
			{Ip: "10.88.0.13", Port: 80, Health: klitev1.EndpointHealth_ENDPOINT_HEALTH_READY},
		},
	})
	_, err := xds.BuildSnapshot("node-1", "1", net)
	if err == nil {
		t.Fatal("BuildSnapshot accepted endpoints for a nonexistent cluster")
	}
	if !strings.Contains(err.Error(), "inconsistent") {
		t.Errorf("error = %q, want it to mention inconsistency", err)
	}
}

func TestRBACTranslation(t *testing.T) {
	t.Parallel()
	aIPs := []string{"10.88.0.1/32", "10.88.0.2/32"}
	cases := []struct {
		name            string
		policies        []*klitev1.CompiledPolicy
		service         string
		wantFilters     []string
		wantDeny        [][]string // nil means no deny filter
		wantAllowFilter bool
		wantAllow       [][]string
	}{
		{
			name:        "no policies means no rbac filters",
			service:     "b",
			wantFilters: []string{tcpName},
		},
		{
			name:        "deny pair blocks source ips on destination listener",
			policies:    []*klitev1.CompiledPolicy{deny("a", "b")},
			service:     "b",
			wantFilters: []string{rbacName, tcpName},
			wantDeny:    [][]string{aIPs},
		},
		{
			name:        "deny pair leaves other listeners alone",
			policies:    []*klitev1.CompiledPolicy{deny("a", "b")},
			service:     "a",
			wantFilters: []string{tcpName},
		},
		{
			name:        "deny to star skips excepted service",
			policies:    []*klitev1.CompiledPolicy{deny("c", "*", "a")},
			service:     "a",
			wantFilters: []string{tcpName},
		},
		{
			name:        "deny to star hits non-excepted service",
			policies:    []*klitev1.CompiledPolicy{deny("c", "*", "a")},
			service:     "b",
			wantFilters: []string{rbacName, tcpName},
			wantDeny:    [][]string{{"10.88.0.9/32"}},
		},
		{
			name:        "deny from star matches any principal",
			policies:    []*klitev1.CompiledPolicy{deny("*", "b")},
			service:     "b",
			wantFilters: []string{rbacName, tcpName},
			wantDeny:    [][]string{{"*"}},
		},
		{
			name:            "allow flips listener to allowlist locking out others",
			policies:        []*klitev1.CompiledPolicy{allow("a", "b")},
			service:         "b",
			wantFilters:     []string{rbacName, tcpName},
			wantAllowFilter: true,
			wantAllow:       [][]string{aIPs}, // c's 10.88.0.9 absent, so c -> b is denied
		},
		{
			name:        "deny from unknown service matches nothing",
			policies:    []*klitev1.CompiledPolicy{deny("ghost", "b")},
			service:     "b",
			wantFilters: []string{tcpName},
		},
		{
			name:            "allow from unknown service admits nobody",
			policies:        []*klitev1.CompiledPolicy{allow("ghost", "b")},
			service:         "b",
			wantFilters:     []string{rbacName, tcpName},
			wantAllowFilter: true,
			wantAllow:       [][]string{},
		},
		{
			name:            "deny filter precedes allow filter",
			policies:        []*klitev1.CompiledPolicy{allow("a", "b"), deny("c", "b")},
			service:         "b",
			wantFilters:     []string{rbacName, rbacName, tcpName},
			wantDeny:        [][]string{{"10.88.0.9/32"}},
			wantAllowFilter: true,
			wantAllow:       [][]string{aIPs},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap, err := xds.BuildSnapshot("node-1", "1", testNet(tc.policies...))
			if err != nil {
				t.Fatalf("BuildSnapshot: %v", err)
			}
			lst := getListener(t, snap, "svc/"+tc.service)
			names, rbacs, tcpCluster := decodeFilters(t, lst)
			if !slices.Equal(names, tc.wantFilters) {
				t.Errorf("filter order = %v, want %v", names, tc.wantFilters)
			}
			if tcpCluster != tc.service {
				t.Errorf("tcp_proxy cluster = %q, want %q", tcpCluster, tc.service)
			}

			var want []rbacView
			if tc.wantDeny != nil {
				want = append(want, rbacView{action: rbaccfgv3.RBAC_DENY, principals: tc.wantDeny})
			}
			if tc.wantAllowFilter {
				want = append(want, rbacView{action: rbaccfgv3.RBAC_ALLOW, principals: tc.wantAllow})
			}
			if len(rbacs) != len(want) {
				t.Fatalf("got %d rbac filters, want %d", len(rbacs), len(want))
			}
			for i, w := range want {
				if rbacs[i].action != w.action {
					t.Errorf("rbac[%d] action = %v, want %v", i, rbacs[i].action, w.action)
				}
				if !slices.EqualFunc(rbacs[i].principals, w.principals, slices.Equal) {
					t.Errorf("rbac[%d] principals = %v, want %v", i, rbacs[i].principals, w.principals)
				}
			}
		})
	}
}

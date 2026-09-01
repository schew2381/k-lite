package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

func TestNetContainerSpecHonorsBasesAndClusterLabel(t *testing.T) {
	t.Parallel()
	a := New(&Config{Node: "node-1"})
	a.net = &klitev1.NetBootstrap{
		KliteNetIp: "10.44.0.63", NodeIndex: 3,
		ClusterId: "abc123", NetAdminPortBase: 21000, EnvoyAdminPortBase: 21500,
	}
	spec := a.netContainerSpec(a.net)
	if got := spec.Ports["9090/tcp"]; got != "127.0.0.1:21003" {
		t.Fatalf("net admin port base ignored: %s", got)
	}
	if got := spec.Ports["9901/tcp"]; got != "127.0.0.1:21503" {
		t.Fatalf("envoy admin port base ignored: %s", got)
	}
	if got := spec.Labels[runtime.LabelCluster]; got != "abc123" {
		t.Fatalf("cluster label missing: %q", got)
	}

	a.net = &klitev1.NetBootstrap{KliteNetIp: "10.44.0.13", NodeIndex: 3}
	def := a.netContainerSpec(a.net)
	if got := def.Ports["9090/tcp"]; got != "127.0.0.1:19003" {
		t.Fatalf("default net admin port broken: %s", got)
	}
	if _, ok := def.Labels[runtime.LabelCluster]; ok {
		t.Fatal("empty cluster id must not stamp a label")
	}
}

// The donor publishes its whole ingress slice on every interface at
// creation (ADR 0024), and adding the slice moves the config hash so
// pre-M9 donors recreate exactly once.
func TestNetContainerSpecPublishesIngressRange(t *testing.T) {
	t.Parallel()
	a := New(&Config{Node: "node-2"})
	bare := &klitev1.NetBootstrap{KliteNetIp: "10.44.0.12", NodeIndex: 2}
	withRange := &klitev1.NetBootstrap{
		KliteNetIp: "10.44.0.12", NodeIndex: 2,
		IngressPortBase: 20000, IngressPortsPerNode: 32,
	}
	a.net = withRange
	spec := a.netContainerSpec(withRange)
	// Node index 2 owns [20032, 20064): 2 admin ports + 32 ingress ports.
	if len(spec.Ports) != 34 {
		t.Fatalf("published ports = %d, want 34", len(spec.Ports))
	}
	for _, p := range []string{"20032/tcp", "20063/tcp"} {
		want := "0.0.0.0:" + p[:5]
		if got := spec.Ports[p]; got != want {
			t.Fatalf("port %s bound to %q, want %q (all interfaces)", p, got, want)
		}
	}
	if _, ok := spec.Ports["20064/tcp"]; ok {
		t.Fatal("published past the node's slice")
	}

	a.net = bare
	old := a.netContainerSpec(bare)
	if _, ok := old.Ports["20032/tcp"]; ok {
		t.Fatal("a pre-M9 bootstrap must publish no ingress ports")
	}
	if old.Labels[runtime.LabelConfigHash] == spec.Labels[runtime.LabelConfigHash] {
		t.Fatal("config hash must move when the range appears, forcing one donor recreate")
	}
}

func TestEvictNetSquattersGuardsForeignClusters(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := New(&Config{Node: "node-1", Runtime: rt})
	rt.infra = []runtime.InfraInfo{
		{ID: "same", Name: "klite.old-node.net", Node: "old-node", Cluster: "ours", IP: "10.44.0.11"},
		{ID: "legacy", Name: "klite.older.net", Node: "older", Cluster: "", IP: "10.44.0.11"},
		{ID: "other-ip", Name: "klite.node-2.net", Node: "node-2", Cluster: "ours", IP: "10.44.0.12"},
	}

	if err := a.evictNetSquatters(context.Background(), "10.44.0.11", "ours"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"same", "legacy"} {
		if !slices.Contains(rt.removes, id) {
			t.Fatalf("stale donor %s not evicted (removed: %v)", id, rt.removes)
		}
	}
	if slices.Contains(rt.removes, "other-ip") {
		t.Fatal("donor on a different address was evicted")
	}

	// A donor from another cluster on our address is a configuration error,
	// never a cleanup target.
	rt2 := newFakeRuntime()
	a2 := New(&Config{Node: "node-1", Runtime: rt2})
	rt2.infra = []runtime.InfraInfo{
		{ID: "foreign", Name: "klite.node-1.net.other", Node: "node-1", Cluster: "theirs", IP: "10.44.0.11"},
	}
	err := a2.evictNetSquatters(context.Background(), "10.44.0.11", "ours")
	if err == nil || !strings.Contains(err.Error(), "another cluster") {
		t.Fatalf("foreign squatter not refused loudly: %v", err)
	}
	if len(rt2.removes) != 0 {
		t.Fatalf("foreign squatter was removed: %v", rt2.removes)
	}
}

func TestRenderEnvoyBootstrapTLSAndFailover(t *testing.T) {
	t.Parallel()
	a := New(&Config{
		Node:        "node-1",
		ServerAddrs: []string{"127.0.0.1:7443", "192.168.1.20:7445"},
		TLSDir:      "/home/u/.klite/agent/node-1/tls",
	})
	got := a.renderEnvoyBootstrap()

	for _, want := range []string{
		"id: node-1",
		"type: STRICT_DNS",
		// Loopback klited maps to the host gateway; routable ones keep
		// their address, so a WAN agent's Envoy dials the same place.
		"address: host.docker.internal",
		"port_value: 7443",
		"address: 192.168.1.20",
		"port_value: 7445",
		"envoy.transport_sockets.tls",
		"tls_minimum_protocol_version: TLSv1_3",
		// Envoy's upstream default caps at 1.2, which klited refuses.
		"tls_maximum_protocol_version: TLSv1_3",
		"filename: /etc/klite/tls/node.crt",
		"filename: /etc/klite/tls/ca.crt",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("bootstrap missing %q:\n%s", want, got)
		}
	}

	plain := New(&Config{Node: "node-1", ServerAddrs: []string{"127.0.0.1:7443"}})
	if strings.Contains(plain.renderEnvoyBootstrap(), "transport_socket") {
		t.Fatal("TLS stanza rendered without an identity dir")
	}
}

// ingressPortRange mirrors the server-side allocator's slice math, and the
// degenerate inputs (pre-M9 servers, unassigned indexes, a slice past the
// port ceiling) all collapse to an empty range instead of a bad publish.
func TestIngressPortRangeBounds(t *testing.T) {
	t.Parallel()
	nb := func(base, per, idx int32) *klitev1.NetBootstrap {
		return &klitev1.NetBootstrap{IngressPortBase: base, IngressPortsPerNode: per, NodeIndex: idx}
	}
	tests := []struct {
		name   string
		nb     *klitev1.NetBootstrap
		lo, hi int
	}{
		{"first node", nb(20000, 32, 1), 20000, 20032},
		{"third node", nb(20000, 32, 3), 20064, 20096},
		{"pre-M9 server sends no base", nb(0, 32, 1), 0, 0},
		{"no per-node size", nb(20000, 0, 1), 0, 0},
		{"unassigned index", nb(20000, 32, 0), 0, 0},
		{"slice ending exactly at the ceiling", nb(65472, 32, 2), 65504, 65536},
		{"slice past the ceiling", nb(65505, 32, 2), 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lo, hi := ingressPortRange(tt.nb)
			if lo != tt.lo || hi != tt.hi {
				t.Fatalf("ingressPortRange = [%d, %d), want [%d, %d)", lo, hi, tt.lo, tt.hi)
			}
		})
	}
}

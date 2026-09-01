package controller

import (
	"slices"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func svc(name string, port, targetPort int32, selector map[string]string) *klitev1.Service {
	return &klitev1.Service{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.ServiceSpec{Selector: selector, Port: port, TargetPort: targetPort},
	}
}

func inst(name, node, ip string, phase klitev1.InstancePhase, labels map[string]string, probePort int32) *klitev1.Instance {
	c := &klitev1.Container{Name: "web"}
	if probePort > 0 {
		c.ReadinessProbe = &klitev1.ReadinessProbe{TcpPort: probePort}
	}
	return &klitev1.Instance{
		Meta:   &klitev1.Meta{Name: name, Labels: labels},
		Spec:   &klitev1.InstanceSpec{Workload: "b", Node: node, Container: c},
		Status: &klitev1.InstanceStatus{Phase: phase, InstanceIp: ip},
	}
}

// fixtureInputs is the shared buildAll scenario: service b on two nodes with
// instances in every phase that matters.
func fixtureInputs() *inputs {
	bLabels := map[string]string{"app": "b"}
	return &inputs{
		services: []*klitev1.Service{svc("b", 8080, 80, map[string]string{"app": "b"})},
		instances: []*klitev1.Instance{
			inst("b-ready", "node-1", "10.44.128.10", klitev1.InstancePhase_INSTANCE_PHASE_READY, bLabels, 80),
			inst("b-running", "node-2", "10.44.128.11", klitev1.InstancePhase_INSTANCE_PHASE_RUNNING, bLabels, 80),
			inst("b-draining", "node-2", "10.44.128.12", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING, bLabels, 80),
			inst("b-noip", "node-1", "", klitev1.InstancePhase_INSTANCE_PHASE_READY, bLabels, 80),
			inst("x-other", "node-1", "10.44.128.13", klitev1.InstancePhase_INSTANCE_PHASE_READY, map[string]string{"app": "x"}, 0),
		},
		nodes: []string{"node-1", "node-2"},
		vips: map[string]string{
			AllocationName("b", "node-1"): "10.44.64.1",
			AllocationName("b", "node-2"): "10.44.64.2",
		},
	}
}

func TestBuildAllServicesAndInstances(t *testing.T) {
	t.Parallel()
	out := buildAll(fixtureInputs())
	n1 := out["node-1"].Net
	if len(n1.GetServices()) != 1 || n1.GetServices()[0].GetVip() != "10.44.64.1" {
		t.Fatalf("node-1 services = %v, want b at 10.44.64.1", n1.GetServices())
	}
	if got := n1.GetServices()[0]; got.GetPort() != 8080 || got.GetTargetPort() != 80 {
		t.Fatalf("ports = %d/%d, want 8080/80", got.GetPort(), got.GetTargetPort())
	}
	if got := out["node-1"].Instances; len(got) != 3 {
		t.Fatalf("node-1 instances = %d, want 3", len(got))
	}
}

// Endpoints span nodes: READY and DRAINING make it in, RUNNING and ip-less
// do not.
func TestBuildAllEndpoints(t *testing.T) {
	t.Parallel()
	n1 := buildAll(fixtureInputs())["node-1"].Net
	if len(n1.GetEndpoints()) != 1 {
		t.Fatalf("endpoint groups = %v, want one for b", n1.GetEndpoints())
	}
	eps := n1.GetEndpoints()[0].GetEndpoints()
	if len(eps) != 2 || eps[0].GetIp() != "10.44.128.10" || eps[1].GetIp() != "10.44.128.12" {
		t.Fatalf("endpoints = %v, want ready+draining only", eps)
	}
	if eps[1].GetHealth() != klitev1.EndpointHealth_ENDPOINT_HEALTH_DRAINING {
		t.Fatalf("draining endpoint health = %v", eps[1].GetHealth())
	}
	if eps[0].GetPort() != 80 || eps[0].GetNode() != "node-1" {
		t.Fatalf("endpoint = %+v, want targetPort 80 on node-1", eps[0])
	}
}

// Probe targets are node-local: b-noip has no address and x-other neither a
// probe nor a selecting service, so node-1 probes only b-ready.
func TestBuildAllProbeTargets(t *testing.T) {
	t.Parallel()
	out := buildAll(fixtureInputs())
	if pts := out["node-1"].Net.GetProbeTargets(); len(pts) != 1 || pts[0].GetInstance() != "b-ready" || pts[0].GetPort() != 80 {
		t.Fatalf("node-1 probe targets = %v", pts)
	}
	if pts := out["node-2"].Net.GetProbeTargets(); len(pts) != 2 {
		t.Fatalf("node-2 probe targets = %v", pts)
	}
}

// Identity covers every addressed selected instance regardless of phase.
func TestBuildAllIPIdentity(t *testing.T) {
	t.Parallel()
	id := buildAll(fixtureInputs())["node-1"].Net.GetIpIdentity()
	if id["10.44.128.11"] != "b" || id["10.44.128.10"] != "b" {
		t.Fatalf("ip identity = %v", id)
	}
	if _, ok := id["10.44.128.13"]; ok {
		t.Fatal("unselected instance must carry no identity")
	}
}

// A departed node decays to an empty snapshot for one pass, so its watchers
// hear the empty state, then drops out entirely and reports as removed for
// the xDS cache eviction.
func TestStoreDropsDepartedNodeAfterDecay(t *testing.T) {
	t.Parallel()
	e := NewEndpoints(nil, nil)
	built := func(nodes ...string) map[string]*NodeSnapshot {
		out := map[string]*NodeSnapshot{}
		for _, n := range nodes {
			out[n] = &NodeSnapshot{Instances: []*klitev1.Instance{
				inst("b-aa", n, "10.44.128.10", klitev1.InstancePhase_INSTANCE_PHASE_READY, nil, 0),
			}, Net: &klitev1.NetDesired{}}
		}
		return out
	}

	assertPass := func(step string, gotChanged, gotRemoved, wantChanged, wantRemoved []string) {
		t.Helper()
		if !slices.Equal(gotChanged, wantChanged) || !slices.Equal(gotRemoved, wantRemoved) {
			t.Fatalf("%s: changed=%v removed=%v, want %v / %v", step, gotChanged, gotRemoved, wantChanged, wantRemoved)
		}
	}

	changed, removed := e.store(built("node-1", "node-2"))
	assertPass("initial store", changed, removed, []string{"node-1", "node-2"}, nil)

	// node-2 leaves the inputs: first pass decays it to empty.
	changed, removed = e.store(built("node-1"))
	assertPass("decay pass", changed, removed, []string{"node-2"}, nil)
	if snap, ok := e.Snapshot("node-2"); !ok || len(snap.Instances) != 0 || snap.Revision == 0 {
		t.Fatalf("decay pass must keep an empty snapshot at a real revision, got %+v (%v)", snap, ok)
	}

	// Second pass without it drops the entry and reports the removal.
	changed, removed = e.store(built("node-1"))
	assertPass("drop pass", changed, removed, nil, []string{"node-2"})
	if snap, ok := e.Snapshot("node-2"); !ok || snap.Revision != 0 {
		t.Fatalf("dropped node must read as never-seen (empty at revision 0), got %+v (%v)", snap, ok)
	}
	// And it stays gone on further passes.
	changed, removed = e.store(built("node-1"))
	assertPass("steady state", changed, removed, nil, nil)
}

func TestBuildAllSkipsServiceWithoutVIP(t *testing.T) {
	t.Parallel()
	in := &inputs{
		services: []*klitev1.Service{svc("b", 8080, 80, map[string]string{"app": "b"})},
		nodes:    []string{"node-1"},
		vips:     map[string]string{},
	}
	if got := buildAll(in)["node-1"].Net.GetServices(); len(got) != 0 {
		t.Fatalf("services = %v, want none until the VIP lands", got)
	}
}

func TestProbeTargetFallsBackToTargetPort(t *testing.T) {
	t.Parallel()
	in := &inputs{
		services: []*klitev1.Service{svc("b", 8080, 9000, map[string]string{"app": "b"})},
		instances: []*klitev1.Instance{
			inst("b-aa", "node-1", "10.44.128.10", klitev1.InstancePhase_INSTANCE_PHASE_RUNNING, map[string]string{"app": "b"}, 0),
		},
		nodes: []string{"node-1"},
		vips:  map[string]string{AllocationName("b", "node-1"): "10.44.64.1"},
	}
	pts := buildAll(in)["node-1"].Net.GetProbeTargets()
	if len(pts) != 1 || pts[0].GetPort() != 9000 {
		t.Fatalf("probe targets = %v, want fallback port 9000", pts)
	}
}

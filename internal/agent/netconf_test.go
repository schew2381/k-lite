package agent

import (
	"context"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func reconcileOK(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProbeVerdictDrivesReadyPhase(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h1"))

	reconcileOK(t, a)
	if st := stateOf(t, a, "b-aa"); st.phase != klitev1.InstancePhase_INSTANCE_PHASE_RUNNING {
		t.Fatalf("phase before probe = %v, want RUNNING", st.phase)
	}

	a.updateProbes([]*klitev1.ProbeState{{Instance: "b-aa", Ready: true}})
	reconcileOK(t, a)
	if st := stateOf(t, a, "b-aa"); st.phase != klitev1.InstancePhase_INSTANCE_PHASE_READY {
		t.Fatalf("phase after ready probe = %v, want READY", st.phase)
	}

	a.updateProbes([]*klitev1.ProbeState{{Instance: "b-aa", Ready: false}})
	reconcileOK(t, a)
	if st := stateOf(t, a, "b-aa"); st.phase != klitev1.InstancePhase_INSTANCE_PHASE_RUNNING {
		t.Fatalf("phase after failing probe = %v, want RUNNING", st.phase)
	}
}

func TestNoProbeMeansImmediatelyReady(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	inst := desiredInstance("a-aa", "uid-1", "h1")
	inst.Spec.Container.ReadinessProbe = nil
	a := testAgent(t, rt, inst)

	reconcileOK(t, a)
	if st := stateOf(t, a, "a-aa"); st.phase != klitev1.InstancePhase_INSTANCE_PHASE_READY {
		t.Fatalf("phase = %v, want READY without a probe", st.phase)
	}
}

func TestNeedNetPushDetectsDriftAndRestart(t *testing.T) {
	t.Parallel()
	a := New(&Config{Node: "node-1"})
	net := &klitev1.NetDesired{Services: []*klitev1.ServiceVIP{{Service: "b", Vip: "10.44.64.1", Port: 8080}}}
	healthOK := &klitev1.HealthResponse{DnsReady: true, VipsBound: 1}

	if !a.needNetPush(net, healthOK) {
		t.Fatal("first pass must push")
	}
	a.markNetApplied(net)
	if a.needNetPush(net, healthOK) {
		t.Fatal("unchanged config with matching vip count must not push")
	}
	// A restarted klite-net remembers nothing: zero VIPs bound.
	if !a.needNetPush(net, &klitev1.HealthResponse{DnsReady: true, VipsBound: 0}) {
		t.Fatal("vip count mismatch must push")
	}
	changed := &klitev1.NetDesired{Services: []*klitev1.ServiceVIP{{Service: "b", Vip: "10.44.64.2", Port: 8080}}}
	if !a.needNetPush(changed, healthOK) {
		t.Fatal("changed config must push")
	}
}

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

func lockdownAgent(rt *fakeRuntime) *Agent {
	a := New(&Config{Node: "node-1", Runtime: rt})
	a.net = &klitev1.NetBootstrap{KliteNetIp: "10.44.0.11", NodeIndex: 1, ClusterId: "abc123"}
	return a
}

func runningDonor(id string) *runtime.InfraStatus {
	return &runtime.InfraStatus{ID: id, Running: true}
}

// The lockdown helper runs once per donor: NET_ADMIN inside the donor's
// netns, dropping the admin ports for workload and VIP sources (ADR 0029).
func TestEnsureLockdownAppliesOncePerDonor(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := lockdownAgent(rt)
	ctx := context.Background()

	a.ensureLockdown(ctx, a.net, runningDonor("donor-1"))
	if rt.oneShotCount() != 1 {
		t.Fatalf("one-shots = %d, want 1", rt.oneShotCount())
	}
	spec := rt.oneShots[0]
	if spec.JoinNetns != "klite.node-1.net" {
		t.Errorf("JoinNetns = %q, the rules must land in the donor's netns", spec.JoinNetns)
	}
	if !slices.Contains(spec.CapAdd, "NET_ADMIN") {
		t.Errorf("CapAdd = %v, iptables needs NET_ADMIN", spec.CapAdd)
	}
	script := strings.Join(spec.Cmd, " ")
	for _, want := range []string{"iptables", "9090", "9901", "10.44.64.0/18", "10.44.128.0/17"} {
		if !strings.Contains(script, want) {
			t.Errorf("lockdown command missing %q", want)
		}
	}
	if spec.Labels[runtime.LabelRole] != runtime.RoleHelper || spec.Labels[runtime.LabelCluster] != "abc123" {
		t.Errorf("labels = %v, want the helper role and the cluster id", spec.Labels)
	}
	if spec.Labels[runtime.LabelConfigHash] == "" {
		t.Error("config hash label missing")
	}

	// The same donor never gets a second pass.
	a.ensureLockdown(ctx, a.net, runningDonor("donor-1"))
	if rt.oneShotCount() != 1 {
		t.Fatalf("one-shots = %d, a locked donor must not re-run the helper", rt.oneShotCount())
	}

	// A recreated donor gets its rules right away, not after the retry delay.
	a.ensureLockdown(ctx, a.net, runningDonor("donor-2"))
	if rt.oneShotCount() != 2 {
		t.Fatalf("one-shots = %d, a fresh donor must be locked immediately", rt.oneShotCount())
	}
}

func TestEnsureLockdownSkipsWithoutRunningDonor(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := lockdownAgent(rt)
	ctx := context.Background()

	a.ensureLockdown(ctx, a.net, nil)
	a.ensureLockdown(ctx, a.net, &runtime.InfraStatus{ID: "donor-1", Running: false})
	if rt.oneShotCount() != 0 {
		t.Fatalf("one-shots = %d, want none without a running donor", rt.oneShotCount())
	}
}

// A failed pass leaves the donor unlocked and arms the per-donor retry
// timer, so the reconcile loop doesn't spawn a helper every two seconds.
func TestEnsureLockdownRetriesAfterFailure(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.oneShotErr = errors.New("apk mirror down")
	a := lockdownAgent(rt)
	ctx := context.Background()
	base := time.Now()
	a.now = func() time.Time { return base }

	donor := runningDonor("donor-1")
	a.ensureLockdown(ctx, a.net, donor)
	if a.lockedDonor != "" {
		t.Fatal("a failed pass must not mark the donor locked")
	}

	// Within the delay nothing retries, even after the error clears.
	rt.mu.Lock()
	rt.oneShotErr = nil
	rt.mu.Unlock()
	a.ensureLockdown(ctx, a.net, donor)
	if rt.oneShotCount() != 0 {
		t.Fatal("retried inside the pacing window")
	}

	a.now = func() time.Time { return base.Add(lockRetryDelay + time.Second) }
	a.ensureLockdown(ctx, a.net, donor)
	if rt.oneShotCount() != 1 || a.lockedDonor != "donor-1" {
		t.Fatalf("one-shots = %d, lockedDonor = %q; want the delayed retry to lock the donor", rt.oneShotCount(), a.lockedDonor)
	}
}

// An unavailable helper image fails the pass the same way a failed run does.
func TestEnsureLockdownImagePullFailureArmsRetry(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.imageErr = errors.New("registry unreachable")
	a := lockdownAgent(rt)

	a.ensureLockdown(context.Background(), a.net, runningDonor("donor-1"))
	if rt.oneShotCount() != 0 {
		t.Fatal("the helper must not run without its image")
	}
	if a.lockedDonor != "" {
		t.Fatal("a failed image pull must not mark the donor locked")
	}
}

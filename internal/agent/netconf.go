package agent

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const (
	netSyncPeriod  = 2 * time.Second
	netCallTimeout = 3 * time.Second
)

// netLoop drives the infra pod and the klite-net daemon behind it: converge
// the containers, push the latest NetDesired whenever it (or klite-net's
// state) drifts, and fold probe results into instance phases. klite-net keeps
// nothing across restarts, so drift detection leans on its reported state,
// not on what we remember sending.
func (a *Agent) netLoop(ctx context.Context) {
	conn, err := a.dialNetAdmin()
	if err != nil {
		slog.Error("klite-net admin dial failed, net loop dead", "err", err)
		return
	}
	defer conn.Close()
	admin := klitev1.NewKliteNetServiceClient(conn)
	ticker := time.NewTicker(netSyncPeriod)
	defer ticker.Stop()
	for {
		if err := a.ensureInfraPod(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("infra pod not converged", "err", err)
			a.setNetHealthy(false)
		} else {
			a.syncNet(ctx, admin)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.kickNet:
		case <-ticker.C:
		}
	}
}

// dialNetAdmin opens the (lazy) client connection to the donor's published
// admin port. Plain TCP on loopback, like every pre-M8 hop.
func (a *Agent) dialNetAdmin() (*grpc.ClientConn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", a.netAdminPort())
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// syncNet is one push-and-poll pass against klite-net's admin API.
func (a *Agent) syncNet(ctx context.Context, admin klitev1.KliteNetServiceClient) {
	desired, ok := a.currentNet()
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, netCallTimeout)
	defer cancel()
	health, err := admin.Health(cctx, &klitev1.HealthRequest{})
	if err != nil {
		if ctx.Err() == nil {
			a.setNetHealthy(false)
		}
		return
	}
	if a.needNetPush(desired, health) {
		if _, err := admin.ApplyConfig(cctx, &klitev1.ApplyConfigRequest{Net: desired}); err != nil {
			slog.Warn("klite-net config push failed", "err", err)
			a.setNetHealthy(false)
			return
		}
		a.markNetApplied(desired)
		slog.Info("klite-net config pushed",
			"services", len(desired.GetServices()), "probeTargets", len(desired.GetProbeTargets()))
	}
	a.setNetHealthy(health.GetDnsReady())
	probes, err := admin.Probes(cctx, &klitev1.ProbesRequest{})
	if err != nil {
		return
	}
	a.updateProbes(probes.GetProbes())
}

// needNetPush wants a push when the desired config changed since the last
// successful apply, or when klite-net's bound-VIP count disagrees with it —
// the tell of a restarted (and therefore empty) daemon.
func (a *Agent) needNetPush(desired *klitev1.NetDesired, health *klitev1.HealthResponse) bool {
	a.mu.Lock()
	last := a.appliedNet
	a.mu.Unlock()
	if last == nil || !proto.Equal(last, desired) {
		return true
	}
	return int(health.GetVipsBound()) != uniqueVIPs(desired)
}

func uniqueVIPs(net *klitev1.NetDesired) int {
	seen := map[string]bool{}
	for _, svc := range net.GetServices() {
		seen[svc.GetVip()] = true
	}
	return len(seen)
}

func (a *Agent) markNetApplied(net *klitev1.NetDesired) {
	a.mu.Lock()
	a.appliedNet = net
	a.mu.Unlock()
}

// currentNet returns the latest snapshot's NetDesired, never nil once a
// snapshot arrived: an empty desired state must still clear klite-net.
func (a *Agent) currentNet() (*klitev1.NetDesired, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.haveSnapshot {
		return nil, false
	}
	if a.desiredNet == nil {
		return &klitev1.NetDesired{}, true
	}
	return a.desiredNet, true
}

func (a *Agent) setNetHealthy(healthy bool) {
	a.mu.Lock()
	changed := a.netHealthy != healthy
	a.netHealthy = healthy
	a.mu.Unlock()
	if changed {
		kick(a.kickReport)
	}
}

// updateProbes swaps in the latest probe verdicts and nudges the reconciler
// when any instance's readiness flipped, since phases derive from them.
func (a *Agent) updateProbes(probes []*klitev1.ProbeState) {
	next := make(map[string]bool, len(probes))
	for _, p := range probes {
		next[p.GetInstance()] = p.GetReady()
	}
	a.mu.Lock()
	changed := !maps.Equal(a.probeReady, next)
	a.probeReady = next
	a.mu.Unlock()
	if changed {
		kick(a.kickReconcile)
	}
}

// kliteNetStatus is the heartbeat's infra-health rider.
func (a *Agent) kliteNetStatus() *klitev1.KliteNetStatus {
	a.mu.Lock()
	nb, healthy := a.net, a.netHealthy
	a.mu.Unlock()
	if nb == nil {
		return nil
	}
	base := int(nb.GetNetAdminPortBase())
	if base == 0 {
		base = defaultNetAdminPortBase
	}
	return &klitev1.KliteNetStatus{
		Healthy:   healthy,
		AdminPort: int32(base + int(nb.GetNodeIndex())),
	}
}

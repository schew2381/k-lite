package agent

import (
	"context"
	"log/slog"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

// lockdown.go closes the M8 security finding: the infra netns exposes
// klite-net's admin gRPC (:9090) and Envoy's admin (:9901) on the donor's
// klite0 address, where every workload container could reach them. A
// run-once helper joins the donor's netns (the donor image is FROM scratch
// and carries no iptables) and drops admin-port traffic from the VIP and
// workload source ranges. The host path stays open: docker-proxy connects
// from the bridge gateway, outside both ranges.

const (
	lockdownImage = "alpine:3.20"

	// lockRetryDelay spaces retries after a failed pass (the helper needs
	// one apk fetch), keeping the reconcile loop from spawning a container
	// every two seconds against a broken network.
	lockRetryDelay = time.Minute
)

// lockdownScript drops new connections to the admin ports from workload
// (10.44.128.0/17) and VIP (10.44.64.0/18) sources. -C before -I keeps it
// idempotent across agent restarts.
const lockdownScript = `set -e
apk add -q --no-cache iptables >/dev/null
for src in 10.44.64.0/18 10.44.128.0/17; do
  for port in 9090 9901; do
    iptables -w -C INPUT -p tcp -s "$src" --dport "$port" -j DROP 2>/dev/null || \
    iptables -w -I INPUT -p tcp -s "$src" --dport "$port" -j DROP
  done
done
iptables -w -S INPUT
`

// ensureLockdown applies the admin-port rules to the donor's netns exactly
// once per donor container. Failure never blocks the infra pod — DNS and the
// data plane matter more than the hardening pass — but it is loud, and it
// retries on a timer.
func (a *Agent) ensureLockdown(ctx context.Context, nb *klitev1.NetBootstrap, donor *runtime.InfraStatus) {
	if donor == nil || !donor.Running || a.lockedDonor == donor.ID {
		return
	}
	if a.now().Sub(a.lockAttempt) < lockRetryDelay {
		return
	}
	a.lockAttempt = a.now()
	spec := &runtime.InfraContainer{
		Name:      "klite." + a.node + ".lockdown",
		Image:     lockdownImage,
		Cmd:       []string{"/bin/sh", "-c", lockdownScript},
		JoinNetns: a.netContainerName(),
		CapAdd:    []string{"NET_ADMIN"},
		Labels:    a.infraLabels(nb, runtime.RoleHelper),
	}
	spec.Labels[runtime.LabelConfigHash] = configHash(spec, donor.ID)
	if err := a.rt.EnsureImage(ctx, spec.Image); err != nil {
		slog.Error("admin lockdown image unavailable — workload containers can reach the admin ports", "err", err)
		return
	}
	if err := a.rt.RunOneShot(ctx, spec); err != nil {
		slog.Error("admin lockdown failed — workload containers can reach the admin ports", "donor", donor.ID, "err", err)
		return
	}
	a.lockedDonor = donor.ID
	slog.Info("admin ports locked down in the infra netns", "node", a.node, "ports", "9090,9901")
}

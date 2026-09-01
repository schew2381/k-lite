package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/schew2381/k-lite/internal/controller"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// drainPollInterval paces the progress stream's store polls. Tests shrink it.
var drainPollInterval = 400 * time.Millisecond

// Drain cordons the node and streams the surge-first choreography (ADR 0010)
// as the leader's controllers perform it: replacements surge elsewhere, old
// instances drain, then disappear. With force, draining instances are deleted
// immediately instead of waiting out their timeout. The RPC only writes
// intent (cordon, drain phase, force deletes). Breaking the stream never
// strands a drain, since the controllers finish it at normal pace.
func (s *Cluster) Drain(req *klitev1.DrainRequest, stream grpc.ServerStreamingServer[klitev1.DrainProgress]) error {
	node := req.GetNode()
	if node == "" {
		return status.Error(codes.InvalidArgument, "node name is required")
	}
	ctx := stream.Context()
	send := func(msg string) error {
		return stream.Send(&klitev1.DrainProgress{Message: msg})
	}
	if err := s.markNodeDraining(ctx, node); err != nil {
		return err
	}
	slog.Info("drain started", "node", node, "force", req.GetForce())
	if err := send("cordoned " + node); err != nil {
		return err
	}

	n := newNarrator(node)
	for {
		insts, _, err := s.store.List(ctx, object.KindInstance)
		if err != nil {
			return storeStatus(err)
		}
		lines, remaining := n.observe(insts)
		if req.GetForce() {
			lines = append(lines, s.forceDelete(ctx, n)...)
		}
		for _, l := range lines {
			if err := send(l); err != nil {
				return err
			}
		}
		if remaining == 0 {
			return stream.Send(&klitev1.DrainProgress{Message: "done: " + node + " drained", Done: true})
		}
		// Heartbeats from a node that bounced through NOT_READY can undo
		// the drain phase, so re-assert while instances remain.
		if err := s.markNodeDraining(ctx, node); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPollInterval):
		}
	}
}

// markNodeDraining cordons the node and sets its phase DRAINING, CAS-retried.
func (s *Cluster) markNodeDraining(ctx context.Context, name string) error {
	for range casRetries {
		obj, rev, err := s.store.Get(ctx, object.KindNode, name)
		if err != nil {
			return storeStatus(err)
		}
		node := obj.GetNode()
		if node.Status == nil {
			node.Status = &klitev1.NodeStatus{}
		}
		if node.Status.GetUnschedulable() && node.Status.GetPhase() == klitev1.NodePhase_NODE_PHASE_DRAINING {
			return nil
		}
		node.Status.Unschedulable = true
		node.Status.Phase = klitev1.NodePhase_NODE_PHASE_DRAINING
		if _, err := s.store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return storeStatus(err)
		}
		return nil
	}
	return status.Error(codes.Aborted, "kept losing revision races, try again")
}

// forceDelete removes the draining instances on the node right away: their
// replacements are READY (the controller only marks DRAINING after that), so
// skipping the timeout only cuts in-flight connections. Each delete is
// pinned to the revision the narrator observed. An instance rewritten or
// recreated since that poll survives the pass and gets re-observed on the
// next one, where the controller's timeout still covers it.
func (s *Cluster) forceDelete(ctx context.Context, n *narrator) []string {
	var lines []string
	for name, v := range n.prev {
		if v.node != n.node || v.phase != klitev1.InstancePhase_INSTANCE_PHASE_DRAINING {
			continue
		}
		if err := s.store.DeleteIfRevision(ctx, object.KindInstance, name, v.rev); err != nil {
			continue
		}
		delete(n.prev, name)
		lines = append(lines, "force-removed "+name)
	}
	return lines
}

// instView is what the narrator remembers about one instance between polls.
type instView struct {
	workload string
	node     string
	phase    klitev1.InstancePhase
	message  string
	drainSec int32
	rev      int64 // mod revision at the observing poll, pinning force deletes
}

// narrator diffs successive instance listings into nomad-style progress
// lines. It tracks the workloads that had instances on the draining node and
// narrates their surges wherever they land.
type narrator struct {
	node      string
	first     bool
	prev      map[string]instView
	workloads map[string]bool
	reported  map[string]bool // fallback lines already sent, by instance name
}

func newNarrator(node string) *narrator {
	return &narrator{node: node, first: true, prev: map[string]instView{}, workloads: map[string]bool{}, reported: map[string]bool{}}
}

func (n *narrator) observe(objs []*klitev1.Object) (lines []string, remaining int) {
	cur := make(map[string]instView, len(objs))
	for _, o := range objs {
		inst := o.GetInstance()
		v := instView{
			workload: inst.GetSpec().GetWorkload(),
			node:     inst.GetSpec().GetNode(),
			phase:    inst.GetStatus().GetPhase(),
			message:  inst.GetStatus().GetMessage(),
			drainSec: inst.GetSpec().GetDrain().GetDrainTimeoutSeconds(),
			rev:      inst.GetMeta().GetResourceVersion(),
		}
		cur[inst.GetMeta().GetName()] = v
		if v.node == n.node {
			remaining++
			n.workloads[v.workload] = true
		}
	}
	if n.first {
		n.first = false
		n.prev = cur
		if remaining > 0 {
			lines = append(lines, fmt.Sprintf("draining %d instance(s) on %s", remaining, n.node))
		}
		return lines, remaining
	}
	lines = n.diff(cur)
	n.prev = cur
	return lines, remaining
}

func (n *narrator) diff(cur map[string]instView) []string {
	var lines []string
	for name, v := range cur {
		if line := n.lineFor(name, v); line != "" {
			lines = append(lines, line)
		}
	}
	for name, old := range n.prev {
		if _, still := cur[name]; !still && old.node == n.node {
			lines = append(lines, "drained "+name)
		}
	}
	return lines
}

// lineFor renders one instance's transition since the last poll, or "".
func (n *narrator) lineFor(name string, v instView) string {
	old, seen := n.prev[name]
	switch {
	case v.node == n.node && v.phase == klitev1.InstancePhase_INSTANCE_PHASE_DRAINING && (!seen || old.phase != v.phase):
		sec := v.drainSec
		if sec <= 0 {
			sec = object.DefaultDrainTimeoutSeconds
		}
		return fmt.Sprintf("draining %s (%ds)%s", name, sec, drainNote(v.message))
	case v.node != n.node && v.node != "" && n.workloads[v.workload] && (!seen || old.node == ""):
		return fmt.Sprintf("surged %s to %s", name, v.node)
	case v.node == "" && n.workloads[v.workload] && blockedMessage(v.message) && !n.reported[name]:
		n.reported[name] = true
		return fmt.Sprintf("surge %s pending: %s; falling back to drain-first (brief dip)", name, v.message)
	}
	return ""
}

// drainNote annotates the drain line with the controller's reason when it
// isn't the plain rollout/drain case.
func drainNote(msg string) string {
	if strings.Contains(msg, "fallback") {
		return " (" + msg + ")"
	}
	return ""
}

func blockedMessage(msg string) bool {
	return msg == controller.MsgNoCapacity || strings.HasPrefix(msg, "pinned node ")
}

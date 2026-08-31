package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// Register hands out this cluster network layout (ADR 0008). Infra pods sit
// at 10.44.0.<10+index>, below the VIP range at 10.44.64.0/18 and the dynamic
// container range at 10.44.128.0/17.
const (
	netSubnet    = "10.44.0.0/16"
	netVIPRange  = "10.44.64.0/18"
	infraIPBase  = 10
	snapshotWait = time.Second
)

// Agent serves AgentService, covering node registration, desired-state
// streams, and status ingestion. StreamCommands and PushCommandOutput stay
// Unimplemented until M3.
type Agent struct {
	klitev1.UnimplementedAgentServiceServer
	store        store.Store
	clusterToken string

	// indexMu serializes node-index assignment on this server, which is
	// enough while one klited handles registration. Multiple servers need
	// an etcd-side reservation, which belongs to M8's join flow.
	indexMu sync.Mutex
}

// NewAgent returns the AgentService backed by the store. Agents must present
// clusterToken at Register.
func NewAgent(st store.Store, clusterToken string) *Agent {
	return &Agent{store: st, clusterToken: clusterToken}
}

// Register admits a node that presents the cluster token and already exists
// in the store, because the per-node YAML is the membership declaration
// (ADR 0003). The response carries the node's stable index and derived
// network addresses.
func (a *Agent) Register(ctx context.Context, req *klitev1.RegisterRequest) (*klitev1.RegisterResponse, error) {
	if req.GetClusterToken() != a.clusterToken {
		return nil, status.Error(codes.Unauthenticated, "bad cluster token")
	}
	if req.GetNode() == "" {
		return nil, status.Error(codes.InvalidArgument, "node name is required")
	}
	a.indexMu.Lock()
	defer a.indexMu.Unlock()
	for range casRetries {
		obj, rev, err := a.store.Get(ctx, object.KindNode, req.GetNode())
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.PermissionDenied,
				"node %q is not declared in the cluster: apply the node YAML first", req.GetNode())
		}
		if err != nil {
			return nil, storeStatus(err)
		}
		node := obj.GetNode()
		if node.Status == nil {
			node.Status = &klitev1.NodeStatus{}
		}
		idx := node.GetStatus().GetNodeIndex()
		if idx == 0 {
			if idx, err = a.freeNodeIndex(ctx); err != nil {
				return nil, storeStatus(err)
			}
			node.Status.NodeIndex = idx
		}
		node.Status.LastHeartbeatUnix = time.Now().Unix()
		node.Status.Phase = klitev1.NodePhase_NODE_PHASE_READY
		if _, err := a.store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return nil, storeStatus(err)
		}
		slog.Info("node registered", "node", req.GetNode(), "index", idx)
		return &klitev1.RegisterResponse{Net: netBootstrap(idx)}, nil
	}
	return nil, status.Error(codes.Aborted, "kept losing revision races, try again")
}

// freeNodeIndex returns the smallest index no node holds, starting at 1. An
// index sticks to its node for life, so the derived addresses stay stable.
func (a *Agent) freeNodeIndex(ctx context.Context) (int32, error) {
	objs, _, err := a.store.List(ctx, object.KindNode)
	if err != nil {
		return 0, err
	}
	used := make(map[int32]bool, len(objs))
	for _, o := range objs {
		used[o.GetNode().GetStatus().GetNodeIndex()] = true
	}
	for i := int32(1); ; i++ {
		if !used[i] {
			return i, nil
		}
	}
}

func netBootstrap(idx int32) *klitev1.NetBootstrap {
	return &klitev1.NetBootstrap{
		Subnet:     netSubnet,
		KliteNetIp: fmt.Sprintf("10.44.0.%d", infraIPBase+idx),
		VipRange:   netVIPRange,
		NodeIndex:  idx,
	}
}

// WatchDesired streams full per-node snapshots, one on connect and then a
// fresh one whenever a store event touches the node's instances, the
// snapshot-then-update shape from swarmkit's dispatcher. A dead or compacted
// etcd watch triggers re-list-and-send, never a stream exit. NetDesired stays
// empty until M4.
func (a *Agent) WatchDesired(req *klitev1.WatchDesiredRequest, stream grpc.ServerStreamingServer[klitev1.DesiredState]) error {
	node := req.GetNode()
	if node == "" {
		return status.Error(codes.InvalidArgument, "node name is required")
	}
	ctx := stream.Context()
	var events <-chan store.Event
	for ctx.Err() == nil {
		rev, err := a.sendSnapshot(ctx, stream, node)
		if err != nil {
			return err
		}
		if events == nil {
			if events, err = a.store.Watch(ctx, []string{object.KindInstance}, rev+1); err != nil {
				slog.Warn("instance watch failed, retrying", "node", node, "err", err)
				sleep(ctx, snapshotWait)
				continue
			}
		}
		if !waitTouch(ctx, events, node) {
			// The watch died or was compacted away. Drop it and resync.
			events = nil
			sleep(ctx, snapshotWait)
		}
	}
	return ctx.Err()
}

// sendSnapshot lists the node's instances and pushes them as one DesiredState.
// etcd hiccups retry in place, and only a dead client stream propagates out.
func (a *Agent) sendSnapshot(ctx context.Context, stream grpc.ServerStreamingServer[klitev1.DesiredState], node string) (int64, error) {
	for {
		objs, rev, err := a.store.List(ctx, object.KindInstance)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			slog.Warn("list instances for snapshot failed", "node", node, "err", err)
			if !sleep(ctx, snapshotWait) {
				return 0, ctx.Err()
			}
			continue
		}
		var instances []*klitev1.Instance
		for _, o := range objs {
			if inst := o.GetInstance(); inst.GetSpec().GetNode() == node {
				instances = append(instances, inst)
			}
		}
		if err := stream.Send(&klitev1.DesiredState{Revision: rev, Instances: instances}); err != nil {
			return 0, err
		}
		return rev, nil
	}
}

// waitTouch blocks until an event touches the node's instances (true) or the
// watch dies (false).
func waitTouch(ctx context.Context, events <-chan store.Event, node string) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-events:
			if !ok || ev.Err != nil {
				return false
			}
			if touchesNode(&ev, node) {
				return true
			}
		}
	}
}

// touchesNode reports whether an instance event concerns the node. A delete
// whose prior value was compacted away carries no spec, so it counts for
// every node: a redundant snapshot beats a missed removal.
func touchesNode(ev *store.Event, node string) bool {
	inst := ev.Object.GetInstance()
	if inst == nil {
		return false
	}
	if inst.GetSpec().GetNode() == node {
		return true
	}
	return inst.GetSpec().GetNode() == "" && ev.Type == klitev1.EventType_EVENT_TYPE_DELETED
}

// ReportStatus writes the agent's instance statuses and stamps the node's
// heartbeat. It doubles as the liveness signal the node controller watches.
func (a *Agent) ReportStatus(ctx context.Context, req *klitev1.ReportStatusRequest) (*klitev1.ReportStatusResponse, error) {
	if req.GetNode() == "" {
		return nil, status.Error(codes.InvalidArgument, "node name is required")
	}
	// A failed instance write never fails the heartbeat: the instance may
	// simply be gone, and the next snapshot stands the agent down.
	for _, u := range req.GetInstances() {
		if err := a.applyInstanceStatus(ctx, u); err != nil {
			slog.Warn("instance status update failed", "instance", u.GetName(), "err", err)
		}
	}
	if err := a.stampNode(ctx, req.GetNode()); err != nil {
		return nil, storeStatus(err)
	}
	return &klitev1.ReportStatusResponse{}, nil
}

// applyInstanceStatus CAS-writes one instance status, skipping vanished
// instances, stale UIDs, and no-op updates.
func (a *Agent) applyInstanceStatus(ctx context.Context, u *klitev1.InstanceStatusUpdate) error {
	for range casRetries {
		obj, rev, err := a.store.Get(ctx, object.KindInstance, u.GetName())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		inst := obj.GetInstance()
		if inst.GetMeta().GetUid() != u.GetUid() {
			return nil
		}
		if proto.Equal(inst.GetStatus(), u.GetStatus()) {
			return nil
		}
		inst.Status = u.GetStatus()
		if _, err := a.store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("instance %s status: %w", u.GetName(), store.ErrConflict)
}

// stampNode records the heartbeat and marks the node READY. The node
// controller reverses that once heartbeats stop.
func (a *Agent) stampNode(ctx context.Context, name string) error {
	for range casRetries {
		obj, rev, err := a.store.Get(ctx, object.KindNode, name)
		if err != nil {
			return err
		}
		node := obj.GetNode()
		if node.Status == nil {
			node.Status = &klitev1.NodeStatus{}
		}
		now := time.Now().Unix()
		if node.Status.LastHeartbeatUnix == now && node.Status.Phase == klitev1.NodePhase_NODE_PHASE_READY {
			return nil
		}
		node.Status.LastHeartbeatUnix = now
		node.Status.Phase = klitev1.NodePhase_NODE_PHASE_READY
		if _, err := a.store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("node %s heartbeat: %w", name, store.ErrConflict)
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

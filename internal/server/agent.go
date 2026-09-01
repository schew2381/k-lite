package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/schew2381/k-lite/internal/ca"
	"github.com/schew2381/k-lite/internal/controller"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// Register hands out this cluster network layout (ADR 0008). Infra pods sit
// at 10.44.0.<10+index>, below the VIP range at 10.44.64.0/18 and the dynamic
// container range at 10.44.128.0/17.
const (
	netSubnet          = "10.44.0.0/16"
	netVIPRange        = "10.44.64.0/18"
	defaultInfraIPBase = 10
)

// AgentConfig wires the AgentService.
type AgentConfig struct {
	Store store.Store
	// ClusterToken is the join secret agents present at Register.
	ClusterToken string
	// Hub carries agent command streams and is shared with the Cluster
	// service, whose Logs RPC feeds off it.
	Hub *CommandHub
	// Net computes the per-node snapshots WatchDesired streams; nil is
	// allowed in tests that never call WatchDesired.
	Net *controller.Endpoints
	// CA signs join CSRs into node certs. nil skips issuance, for tests
	// that only exercise registration bookkeeping.
	CA *ca.CA
	// ClusterID is this cluster's stable random identity, minted at first
	// boot and handed to agents so infra containers can be labeled.
	ClusterID string
	// Port bases for the per-node loopback admin ports (0 means default).
	// A deliberate second cluster on the same machine overrides these.
	NetAdminPortBase   int32
	EnvoyAdminPortBase int32
	// InfraIPBase shifts donor addresses (10.44.0.<base+index>), the third
	// per-cluster knob a coexisting cluster must move.
	InfraIPBase int32
}

// Agent serves AgentService, covering node registration, desired-state
// streams, status ingestion, and the command channel (commands.go).
type Agent struct {
	klitev1.UnimplementedAgentServiceServer
	cfg AgentConfig

	// indexMu serializes node-index assignment on this server, which is
	// enough while one klited handles registration. Multiple servers need
	// an etcd-side reservation, which belongs to M8's join flow.
	indexMu sync.Mutex
}

// NewAgent returns the AgentService backed by cfg.Store.
func NewAgent(cfg *AgentConfig) *Agent {
	return &Agent{cfg: *cfg}
}

// Register admits a node that presents the cluster token and already exists
// in the store, because the per-node YAML is the membership declaration
// (ADR 0003). A node that already holds a certificate for the requested name
// may re-register on that identity alone. The response carries the node's
// stable index, derived network addresses, and — when the request bears a
// CSR — the signed node cert plus the cluster CA (ADR 0013).
func (a *Agent) Register(ctx context.Context, req *klitev1.RegisterRequest) (*klitev1.RegisterResponse, error) {
	certPEM, err := a.admit(ctx, req)
	if err != nil {
		return nil, err
	}
	a.indexMu.Lock()
	defer a.indexMu.Unlock()
	for range casRetries {
		obj, rev, err := a.cfg.Store.Get(ctx, object.KindNode, req.GetNode())
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
		if _, err := a.cfg.Store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return nil, storeStatus(err)
		}
		slog.Info("node registered", "node", req.GetNode(), "index", idx, "certIssued", len(certPEM) > 0)
		resp := &klitev1.RegisterResponse{Net: a.netBootstrap(idx)}
		if len(certPEM) > 0 {
			resp.CertPem = certPEM
			resp.CaPem = a.cfg.CA.CertPEM
		}
		return resp, nil
	}
	return nil, status.Error(codes.Aborted, "kept losing revision races, try again")
}

// admit authenticates a Register call — an existing certificate for the
// requested name, or the cluster token — and signs the request's CSR, if any,
// into the node cert the response will carry.
func (a *Agent) admit(ctx context.Context, req *klitev1.RegisterRequest) ([]byte, error) {
	p := callerPrincipal(ctx)
	certified := p.kind == principalNode && p.node == req.GetNode()
	if !certified && subtle.ConstantTimeCompare([]byte(req.GetClusterToken()), []byte(a.cfg.ClusterToken)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "bad cluster token")
	}
	if req.GetNode() == "" {
		return nil, status.Error(codes.InvalidArgument, "node name is required")
	}
	if len(req.GetCsrPem()) == 0 {
		return nil, nil
	}
	if a.cfg.CA == nil {
		return nil, status.Error(codes.Unimplemented, "this server has no CA to sign the csr")
	}
	certPEM, err := a.cfg.CA.SignNodeCSR(req.GetCsrPem(), req.GetNode())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "sign csr: %v", err)
	}
	return certPEM, nil
}

// freeNodeIndex returns the smallest index no node holds, starting at 1. An
// index sticks to its node for life, so the derived addresses stay stable.
func (a *Agent) freeNodeIndex(ctx context.Context) (int32, error) {
	objs, _, err := a.cfg.Store.List(ctx, object.KindNode)
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

func (a *Agent) netBootstrap(idx int32) *klitev1.NetBootstrap {
	base := a.cfg.InfraIPBase
	if base == 0 {
		base = defaultInfraIPBase
	}
	return &klitev1.NetBootstrap{
		Subnet:             netSubnet,
		KliteNetIp:         fmt.Sprintf("10.44.0.%d", base+idx),
		VipRange:           netVIPRange,
		NodeIndex:          idx,
		ClusterId:          a.cfg.ClusterID,
		NetAdminPortBase:   a.cfg.NetAdminPortBase,
		EnvoyAdminPortBase: a.cfg.EnvoyAdminPortBase,
	}
}

// WatchDesired streams full per-node snapshots off the endpoints engine, one
// on connect and then a fresh one whenever the node's content changes, the
// snapshot-then-update shape from swarmkit's dispatcher. The engine absorbs
// store hiccups, so the only exit is a dead client stream.
func (a *Agent) WatchDesired(req *klitev1.WatchDesiredRequest, stream grpc.ServerStreamingServer[klitev1.DesiredState]) error {
	node := req.GetNode()
	if node == "" {
		return status.Error(codes.InvalidArgument, "node name is required")
	}
	ctx := stream.Context()
	if err := requireNodeMatch(ctx, node); err != nil {
		return err
	}
	kicks, cancel := a.cfg.Net.Subscribe()
	defer cancel()
	lastSent := int64(-1)
	for {
		if snap, ok := a.cfg.Net.Snapshot(node); ok && snap.Revision != lastSent {
			ds := &klitev1.DesiredState{Revision: snap.Revision, Instances: snap.Instances, Net: snap.Net}
			if err := stream.Send(ds); err != nil {
				return err
			}
			lastSent = snap.Revision
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-kicks:
		}
	}
}

// ReportStatus writes the agent's instance statuses and stamps the node's
// heartbeat. It doubles as the liveness signal the node controller watches.
func (a *Agent) ReportStatus(ctx context.Context, req *klitev1.ReportStatusRequest) (*klitev1.ReportStatusResponse, error) {
	if req.GetNode() == "" {
		return nil, status.Error(codes.InvalidArgument, "node name is required")
	}
	if err := requireNodeMatch(ctx, req.GetNode()); err != nil {
		return nil, err
	}
	if kn := req.GetKliteNet(); kn != nil && !kn.GetHealthy() {
		slog.Warn("klite-net unhealthy", "node", req.GetNode(), "adminPort", kn.GetAdminPort())
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
	reported := u.GetStatus().GetPhase()
	for range casRetries {
		obj, rev, err := a.cfg.Store.Get(ctx, object.KindInstance, u.GetName())
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
		// A controller-set DRAINING phase outlives agent reports that still
		// say the instance serves; the drain only moves forward, to FAILED
		// or deletion (ADR 0010).
		u.GetStatus().Phase = reported
		if inst.GetStatus().GetPhase() == klitev1.InstancePhase_INSTANCE_PHASE_DRAINING && servingPhase(reported) {
			u.GetStatus().Phase = klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
		}
		if proto.Equal(inst.GetStatus(), u.GetStatus()) {
			return nil
		}
		inst.Status = u.GetStatus()
		if _, err := a.cfg.Store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("instance %s status: %w", u.GetName(), store.ErrConflict)
}

// servingPhase reports whether a phase could put the endpoint back into
// service, which a drain must never allow.
func servingPhase(p klitev1.InstancePhase) bool {
	return p == klitev1.InstancePhase_INSTANCE_PHASE_READY ||
		p == klitev1.InstancePhase_INSTANCE_PHASE_RUNNING ||
		p == klitev1.InstancePhase_INSTANCE_PHASE_PENDING
}

// stampNode records the heartbeat and marks the node READY. The node
// controller reverses that once heartbeats stop. A DRAINING phase survives
// heartbeats: the node controller flips it back once the drain completes.
func (a *Agent) stampNode(ctx context.Context, name string) error {
	for range casRetries {
		obj, rev, err := a.cfg.Store.Get(ctx, object.KindNode, name)
		if err != nil {
			return err
		}
		node := obj.GetNode()
		if node.Status == nil {
			node.Status = &klitev1.NodeStatus{}
		}
		phase := klitev1.NodePhase_NODE_PHASE_READY
		if node.Status.GetPhase() == klitev1.NodePhase_NODE_PHASE_DRAINING {
			phase = klitev1.NodePhase_NODE_PHASE_DRAINING
		}
		now := time.Now().Unix()
		if node.Status.LastHeartbeatUnix == now && node.Status.Phase == phase {
			return nil
		}
		node.Status.LastHeartbeatUnix = now
		node.Status.Phase = phase
		if _, err := a.cfg.Store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("node %s heartbeat: %w", name, store.ErrConflict)
}

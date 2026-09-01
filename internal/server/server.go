// Package server implements the gRPC services klited exposes.
package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/schew2381/k-lite/internal/ca"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// Writes retry this many revision races before giving up. Conflicts need a
// concurrent writer to the same object, so one retry usually settles it.
const casRetries = 5

// TokenConfig is what NodeToken needs to mint join tokens: the CA PEM whose
// hash the token pins, and the cluster secret it carries (ADR 0013).
type TokenConfig struct {
	CAPEM         []byte
	ClusterSecret string
}

// Cluster serves ClusterService straight off the store, keeping klited
// stateless (ADR 0005). The one exception is hub, which holds live agent
// streams for log relaying and evaporates with the process.
type Cluster struct {
	klitev1.UnimplementedClusterServiceServer
	store  store.Store
	hub    *CommandHub
	tokens *TokenConfig
}

// NewCluster returns the ClusterService. tokens may be nil in tests, which
// leaves NodeToken unimplemented.
func NewCluster(st store.Store, hub *CommandHub, tokens *TokenConfig) *Cluster {
	return &Cluster{store: st, hub: hub, tokens: tokens}
}

// NodeToken mints a join token for `klite node token`. The token embeds the
// CA hash, so agents can pin the server on their first, unverified dial.
func (s *Cluster) NodeToken(context.Context, *klitev1.NodeTokenRequest) (*klitev1.NodeTokenResponse, error) {
	if s.tokens == nil {
		return nil, status.Error(codes.Unimplemented, "this server mints no join tokens (started without a CA)")
	}
	return &klitev1.NodeTokenResponse{Token: ca.MintToken(s.tokens.CAPEM, s.tokens.ClusterSecret)}, nil
}

// Uncordon clears a node's unschedulable flag, set earlier by a drain. A node
// whose deletion is pending stays cordoned: the delete choreography owns it
// (ADR 0010, 0023).
func (s *Cluster) Uncordon(ctx context.Context, req *klitev1.UncordonRequest) (*klitev1.UncordonResponse, error) {
	name := req.GetNode()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "node name is required")
	}
	for range casRetries {
		obj, rev, err := s.store.Get(ctx, object.KindNode, name)
		if err != nil {
			return nil, storeStatus(err)
		}
		if object.MetaOf(obj).GetLabels()[object.LabelPendingDelete] == "true" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"node %q is pending delete; re-apply its YAML to cancel the delete instead", name)
		}
		node := obj.GetNode()
		if node.Status == nil {
			node.Status = &klitev1.NodeStatus{}
		}
		if !node.Status.GetUnschedulable() && node.Status.GetPhase() != klitev1.NodePhase_NODE_PHASE_DRAINING {
			return &klitev1.UncordonResponse{}, nil
		}
		node.Status.Unschedulable = false
		if node.Status.GetPhase() == klitev1.NodePhase_NODE_PHASE_DRAINING {
			node.Status.Phase = klitev1.NodePhase_NODE_PHASE_READY
		}
		if _, err := s.store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return nil, storeStatus(err)
		}
		slog.Info("node uncordoned", "node", name)
		return &klitev1.UncordonResponse{}, nil
	}
	return nil, status.Error(codes.Aborted, "kept losing revision races, try again")
}

func (s *Cluster) Apply(ctx context.Context, req *klitev1.ApplyRequest) (*klitev1.ApplyResponse, error) {
	objs, err := object.Decode(req.GetYaml())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode yaml: %v", err)
	}
	if len(objs) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no objects in request")
	}
	resp := &klitev1.ApplyResponse{}
	for _, o := range objs {
		res := s.applyOne(ctx, o)
		if res.Error != "" {
			slog.Warn("apply failed", "kind", res.Kind, "name", res.Name, "err", res.Error)
		} else {
			slog.Info("apply", "kind", res.Kind, "name", res.Name, "action", res.Action)
		}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

func (s *Cluster) applyOne(ctx context.Context, o *klitev1.Object) *klitev1.ApplyResult {
	kind := object.KindOf(o)
	res := &klitev1.ApplyResult{Kind: kind, Name: object.MetaOf(o).GetName()}
	if kind == object.KindInstance || kind == object.KindVIPAllocation {
		res.Error = strings.ToLower(object.Plural(kind)) + " are server-materialized and read-only"
		return res
	}
	sanitize(o)
	object.Default(o)
	if err := object.Validate(o); err != nil {
		res.Error = err.Error()
		return res
	}

	for range casRetries {
		existing, rev, err := s.store.Get(ctx, kind, res.Name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if _, err := s.store.Put(ctx, o, store.RevCreate); err != nil {
				if errors.Is(err, store.ErrAlreadyExists) {
					continue
				}
				res.Error = err.Error()
				return res
			}
			res.Action = "created"
			return res
		case err != nil:
			res.Error = err.Error()
			return res
		}

		merged := mergeSpec(existing, o)
		if proto.Equal(merged, existing) {
			res.Action = "unchanged"
			return res
		}
		if _, err := s.store.Put(ctx, merged, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			res.Error = err.Error()
			return res
		}
		res.Action = "updated"
		return res
	}
	res.Error = "kept losing revision races, try again"
	return res
}

// sanitize strips the server-owned fields a client may echo back from a get:
// meta identity comes from the store and status from controllers.
func sanitize(o *klitev1.Object) {
	m := object.MetaOf(o)
	m.Uid = ""
	m.ResourceVersion = 0
	m.CreatedUnix = 0
	switch k := o.GetKind().(type) {
	case *klitev1.Object_Workload:
		k.Workload.Status = nil
	case *klitev1.Object_Node:
		k.Node.Status = nil
	case *klitev1.Object_Instance:
		k.Instance.Status = nil
	}
}

// mergeSpec lays the incoming spec and labels over the stored object, keeping
// meta identity and status, so equality against existing detects a no-op apply.
func mergeSpec(existing, incoming *klitev1.Object) *klitev1.Object {
	merged := proto.CloneOf(existing)
	object.MetaOf(merged).Labels = object.MetaOf(incoming).GetLabels()
	switch k := merged.GetKind().(type) {
	case *klitev1.Object_Workload:
		k.Workload.Spec = incoming.GetWorkload().GetSpec()
	case *klitev1.Object_Service:
		k.Service.Spec = incoming.GetService().GetSpec()
	case *klitev1.Object_Node:
		k.Node.Spec = incoming.GetNode().GetSpec()
	case *klitev1.Object_NetworkPolicy:
		k.NetworkPolicy.Spec = incoming.GetNetworkPolicy().GetSpec()
	case *klitev1.Object_Instance:
		k.Instance.Spec = incoming.GetInstance().GetSpec()
	}
	return merged
}

func (s *Cluster) List(ctx context.Context, req *klitev1.ListRequest) (*klitev1.ListResponse, error) {
	kind, err := object.Canonical(req.GetKind())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if name := req.GetName(); name != "" {
		obj, _, err := s.store.Get(ctx, kind, name)
		if err != nil {
			return nil, storeStatus(err)
		}
		return &klitev1.ListResponse{Objects: []*klitev1.Object{obj}}, nil
	}
	objs, _, err := s.store.List(ctx, kind)
	if err != nil {
		return nil, storeStatus(err)
	}
	return &klitev1.ListResponse{Objects: objs}, nil
}

func (s *Cluster) Delete(ctx context.Context, req *klitev1.DeleteRequest) (*klitev1.DeleteResponse, error) {
	if len(req.GetYaml()) > 0 {
		objs, err := object.Decode(req.GetYaml())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "decode yaml: %v", err)
		}
		resp := &klitev1.DeleteResponse{}
		for _, o := range objs {
			res := &klitev1.ApplyResult{Kind: object.KindOf(o), Name: object.MetaOf(o).GetName()}
			res.Action, err = s.deleteOne(ctx, res.Kind, res.Name)
			if err != nil {
				res.Error = err.Error()
				slog.Warn("delete failed", "kind", res.Kind, "name", res.Name, "err", res.Error)
			} else {
				slog.Info("delete", "kind", res.Kind, "name", res.Name, "action", res.Action)
			}
			resp.Results = append(resp.Results, res)
		}
		return resp, nil
	}

	kind, err := object.Canonical(req.GetKind())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	action, err := s.deleteOne(ctx, kind, req.GetName())
	if err != nil {
		return nil, storeStatus(err)
	}
	slog.Info("delete", "kind", kind, "name", req.GetName(), "action", action)
	return &klitev1.DeleteResponse{
		Results: []*klitev1.ApplyResult{{Kind: kind, Name: req.GetName(), Action: action}},
	}, nil
}

// deleteOne deletes an object, except nodes: those are marked pending-delete
// and drained first, and the node controller removes the record once the
// last instance has left (ADR 0010).
func (s *Cluster) deleteOne(ctx context.Context, kind, name string) (string, error) {
	if kind == object.KindNode {
		return s.markNodeForDelete(ctx, name)
	}
	if err := s.store.Delete(ctx, kind, name); err != nil {
		return "", err
	}
	return "deleted", nil
}

// markNodeForDelete cordons the node, flags it pending-delete, and starts its
// drain. Idempotent: repeating the delete reports the drain in progress.
func (s *Cluster) markNodeForDelete(ctx context.Context, name string) (string, error) {
	for range casRetries {
		obj, rev, err := s.store.Get(ctx, object.KindNode, name)
		if err != nil {
			return "", err
		}
		node := obj.GetNode()
		meta := object.MetaOf(obj)
		if meta.GetLabels()[object.LabelPendingDelete] == "true" {
			return "draining (delete pending)", nil
		}
		if meta.Labels == nil {
			meta.Labels = map[string]string{}
		}
		meta.Labels[object.LabelPendingDelete] = "true"
		if node.Status == nil {
			node.Status = &klitev1.NodeStatus{}
		}
		node.Status.Unschedulable = true
		node.Status.Phase = klitev1.NodePhase_NODE_PHASE_DRAINING
		if _, err := s.store.Put(ctx, obj, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return "", err
		}
		return "draining (delete pending)", nil
	}
	return "", status.Error(codes.Aborted, "kept losing revision races, try again")
}

func (s *Cluster) Watch(req *klitev1.WatchRequest, stream grpc.ServerStreamingServer[klitev1.WatchEvent]) error {
	ctx := stream.Context()
	kinds := req.GetKinds()
	for _, k := range kinds {
		if _, err := object.Canonical(k); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	events, err := s.store.Watch(ctx, kinds, req.GetFromRevision())
	if err != nil {
		return storeStatus(err)
	}
	for ev := range events {
		if ev.Err != nil {
			return status.Errorf(codes.Unavailable, "watch ended: %v", ev.Err)
		}
		out := &klitev1.WatchEvent{Type: ev.Type, Object: ev.Object, Revision: ev.Revision}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Cluster) Scale(ctx context.Context, req *klitev1.ScaleRequest) (*klitev1.ScaleResponse, error) {
	if req.GetWorkload() == "" {
		return nil, status.Error(codes.InvalidArgument, "workload name is required")
	}
	if req.GetReplicas() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "replicas must be >= 0, got %d", req.GetReplicas())
	}
	for range casRetries {
		obj, rev, err := s.store.Get(ctx, object.KindWorkload, req.GetWorkload())
		if err != nil {
			return nil, storeStatus(err)
		}
		w := obj.GetWorkload()
		if w.GetSpec().GetReplicas() == req.GetReplicas() {
			return &klitev1.ScaleResponse{}, nil
		}
		updated := proto.CloneOf(obj)
		if updated.GetWorkload().Spec == nil {
			updated.GetWorkload().Spec = &klitev1.WorkloadSpec{}
		}
		updated.GetWorkload().Spec.Replicas = req.GetReplicas()
		if _, err := s.store.Put(ctx, updated, rev); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return nil, storeStatus(err)
		}
		slog.Info("scale", "workload", req.GetWorkload(), "replicas", req.GetReplicas())
		return &klitev1.ScaleResponse{}, nil
	}
	return nil, status.Error(codes.Aborted, "kept losing revision races, try again")
}

func storeStatus(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, store.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

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

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// Writes retry this many revision races before giving up. Conflicts need a
// concurrent writer to the same object, so one retry usually settles it.
const casRetries = 5

// Cluster serves ClusterService straight off the store, keeping klited
// stateless (ADR 0005). The one exception is hub, which holds live agent
// streams for log relaying and evaporates with the process.
type Cluster struct {
	klitev1.UnimplementedClusterServiceServer
	store store.Store
	hub   *CommandHub
}

func NewCluster(st store.Store, hub *CommandHub) *Cluster {
	return &Cluster{store: st, hub: hub}
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
			if err := s.store.Delete(ctx, res.Kind, res.Name); err != nil {
				res.Error = err.Error()
				slog.Warn("delete failed", "kind", res.Kind, "name", res.Name, "err", res.Error)
			} else {
				res.Action = "deleted"
				slog.Info("delete", "kind", res.Kind, "name", res.Name)
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
	if err := s.store.Delete(ctx, kind, req.GetName()); err != nil {
		return nil, storeStatus(err)
	}
	slog.Info("delete", "kind", kind, "name", req.GetName())
	return &klitev1.DeleteResponse{
		Results: []*klitev1.ApplyResult{{Kind: kind, Name: req.GetName(), Action: "deleted"}},
	}, nil
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

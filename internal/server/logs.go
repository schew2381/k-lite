package server

import (
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

// Logs resolves the instance to its node, relays a LogsCommand over that
// node's command stream, and forwards the agent's chunks to the client until
// eof. Every exit path sends the agent a StopCommand, which covers the client
// hanging up mid-follow. After a clean eof the agent has already forgotten
// the command id and drops the stop.
func (s *Cluster) Logs(req *klitev1.LogsRequest, stream grpc.ServerStreamingServer[klitev1.LogChunk]) error {
	if req.GetInstance() == "" {
		return status.Error(codes.InvalidArgument, "instance name is required")
	}
	ctx := stream.Context()
	obj, _, err := s.store.Get(ctx, object.KindInstance, req.GetInstance())
	if err != nil {
		return storeStatus(err)
	}
	node := obj.GetInstance().GetSpec().GetNode()
	if node == "" {
		return status.Errorf(codes.FailedPrecondition, "instance %s is not scheduled to a node yet", req.GetInstance())
	}

	id := uuid.NewString()
	w := s.hub.addWaiter(id)
	defer s.hub.removeWaiter(id)
	ok := s.hub.send(node, &klitev1.Command{Id: id, Cmd: &klitev1.Command_Logs{Logs: &klitev1.LogsCommand{
		Instance: req.GetInstance(),
		Follow:   req.GetFollow(),
		Tail:     req.GetTail(),
	}}})
	if !ok {
		// With several klited replicas the agent's stream may be parked on a
		// sibling, so the CLI treats this code as "try the next endpoint".
		// M7 moves that routing server-side.
		return status.Errorf(codes.FailedPrecondition, "agent for node %s not connected to this server", node)
	}
	defer s.hub.send(node, &klitev1.Command{Id: id, Cmd: &klitev1.Command_Stop{Stop: &klitev1.StopCommand{CommandId: id}}})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out := <-w.ch:
			if len(out.GetData()) > 0 {
				if err := stream.Send(&klitev1.LogChunk{Data: out.GetData()}); err != nil {
					return err
				}
			}
			if out.GetEof() {
				return nil
			}
		}
	}
}

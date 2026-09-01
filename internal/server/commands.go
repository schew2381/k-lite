package server

import (
	"errors"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// CommandHub matches Logs RPCs with the agent command streams parked on this
// server. Each node holds at most one stream (newest wins), and each running
// command has one waiter keyed by command id. The hub is per-process on
// purpose: an agent's streams all ride one connection, so its output can only
// ever arrive at the klited that issued the command (research/grpc-go.md).
type CommandHub struct {
	mu      sync.Mutex
	streams map[string]*commandStream // by node
	waiters map[string]*outputWaiter  // by command id
}

// commandStream is the server end of one agent's StreamCommands call.
type commandStream struct {
	ch       chan *klitev1.Command
	replaced chan struct{} // closed when a newer stream for the node attaches
}

// outputWaiter is one Logs RPC waiting for a command's output.
type outputWaiter struct {
	node string // the node the command was sent to, and only its pushes may land here
	ch   chan *klitev1.CommandOutput
	gone chan struct{} // closed when the waiter stops reading
}

func NewCommandHub() *CommandHub {
	return &CommandHub{
		streams: map[string]*commandStream{},
		waiters: map[string]*outputWaiter{},
	}
}

// attach registers the node's command stream, displacing any predecessor. A
// reconnecting agent always has the freshest connection, so the old entry is
// dead weight by definition.
func (h *CommandHub) attach(node string) *commandStream {
	cs := &commandStream{ch: make(chan *klitev1.Command, 16), replaced: make(chan struct{})}
	h.mu.Lock()
	old := h.streams[node]
	h.streams[node] = cs
	h.mu.Unlock()
	if old != nil {
		close(old.replaced)
	}
	return cs
}

// detach forgets the stream unless a newer one already took the slot, then
// fails the waiters of any still-queued commands, whose agent left before
// ever running them.
func (h *CommandHub) detach(node string, cs *commandStream) {
	h.mu.Lock()
	if h.streams[node] == cs {
		delete(h.streams, node)
	}
	h.mu.Unlock()
	for {
		select {
		case cmd := <-cs.ch:
			if cmd.GetLogs() == nil {
				continue
			}
			h.route(nil, &klitev1.CommandOutput{
				CommandId: cmd.GetId(),
				Data:      []byte("the node's agent disconnected before running the command\n"),
				Eof:       true,
			})
		default:
			return
		}
	}
}

// send queues a command for the node's agent, reporting false when this
// server holds no live stream for the node. Enqueueing under the lock pairs
// with detach, so a command lands either on a registered stream or nowhere.
func (h *CommandHub) send(node string, cmd *klitev1.Command) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.streams[node]
	if cs == nil {
		return false
	}
	select {
	case cs.ch <- cmd:
		return true
	default:
		// A full queue means the agent stopped draining, so treat it as gone.
		return false
	}
}

// addWaiter parks a channel for the command's output, bound to the node the
// command targets. Callers must pair it with removeWaiter.
func (h *CommandHub) addWaiter(id, node string) *outputWaiter {
	w := &outputWaiter{node: node, ch: make(chan *klitev1.CommandOutput, 16), gone: make(chan struct{})}
	h.mu.Lock()
	h.waiters[id] = w
	h.mu.Unlock()
	return w
}

// waiterNode reports which node a live command's output must come from.
func (h *CommandHub) waiterNode(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.waiters[id]
	if w == nil {
		return "", false
	}
	return w.node, true
}

func (h *CommandHub) removeWaiter(id string) {
	h.mu.Lock()
	w := h.waiters[id]
	delete(h.waiters, id)
	h.mu.Unlock()
	if w != nil {
		close(w.gone)
	}
}

// route hands one output message to its waiter. It blocks when the waiter's
// buffer is full, so a slow log reader backpressures the agent through the
// push stream's own flow control. False means the waiter is gone and the
// pusher should stand down.
func (h *CommandHub) route(stop <-chan struct{}, out *klitev1.CommandOutput) bool {
	h.mu.Lock()
	w := h.waiters[out.GetCommandId()]
	h.mu.Unlock()
	if w == nil {
		return false
	}
	select {
	case w.ch <- out:
		return true
	case <-w.gone:
		return false
	case <-stop:
		return false
	}
}

// StreamCommands parks the agent's command stream in the hub and forwards
// queued commands until the agent hangs up or a newer stream takes over.
func (a *Agent) StreamCommands(req *klitev1.StreamCommandsRequest, stream grpc.ServerStreamingServer[klitev1.Command]) error {
	node := req.GetNode()
	if node == "" {
		return status.Error(codes.InvalidArgument, "node name is required")
	}
	if err := requireNodeMatch(stream.Context(), node); err != nil {
		return err
	}
	cs := a.cfg.Hub.attach(node)
	defer a.cfg.Hub.detach(node, cs)
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cs.replaced:
			return status.Error(codes.Aborted, "a newer command stream took over for this node")
		case cmd := <-cs.ch:
			if err := stream.Send(cmd); err != nil {
				return err
			}
		}
	}
}

// PushCommandOutput drains one command's output into the waiting Logs RPC. A
// vanished waiter ends the stream with an error, so the agent stops pumping
// even if its StopCommand got lost in a reconnect. A stream that dies before
// its eof message gets a synthetic one, so the waiter ends instead of hanging
// on an agent that vanished mid-command. Every message must come from the
// node the command targeted: command ids are unguessable, but nothing else
// stops one certified node from injecting into another node's log stream.
func (a *Agent) PushCommandOutput(stream grpc.ClientStreamingServer[klitev1.CommandOutput, klitev1.PushCommandOutputResponse]) error {
	ctx := stream.Context()
	id := ""
	sawEOF := false
	for {
		out, err := stream.Recv()
		if err != nil {
			if id != "" && !sawEOF {
				a.cfg.Hub.route(ctx.Done(), &klitev1.CommandOutput{
					CommandId: id,
					Data:      []byte("log stream from agent broke: " + err.Error() + "\n"),
					Eof:       true,
				})
			}
			if errors.Is(err, io.EOF) {
				return stream.SendAndClose(&klitev1.PushCommandOutputResponse{})
			}
			return err
		}
		if p := callerPrincipal(ctx); p.kind == principalNode {
			if node, ok := a.cfg.Hub.waiterNode(out.GetCommandId()); ok && node != p.node {
				return status.Errorf(codes.PermissionDenied,
					"command %s was sent to node %q, not %q", out.GetCommandId(), node, p.node)
			}
		}
		id = out.GetCommandId()
		sawEOF = out.GetEof()
		if !a.cfg.Hub.route(ctx.Done(), out) {
			return status.Errorf(codes.NotFound, "command %s has no waiter on this server", out.GetCommandId())
		}
	}
}

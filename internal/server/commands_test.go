package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func logsCmd(id string) *klitev1.Command {
	return &klitev1.Command{Id: id, Cmd: &klitev1.Command_Logs{Logs: &klitev1.LogsCommand{Instance: "a-x"}}}
}

func output(id, data string) *klitev1.CommandOutput {
	return &klitev1.CommandOutput{CommandId: id, Data: []byte(data)}
}

func TestHubSendWithoutStream(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	if h.send("node-1", logsCmd("c1")) {
		t.Fatal("send must fail when the node has no stream")
	}
}

func TestHubSendReachesAttachedStream(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	cs := h.attach("node-1")
	if !h.send("node-1", logsCmd("c1")) {
		t.Fatal("send must succeed with a stream attached")
	}
	select {
	case cmd := <-cs.ch:
		if cmd.GetId() != "c1" {
			t.Fatalf("delivered command %q, want c1", cmd.GetId())
		}
	default:
		t.Fatal("command never reached the stream channel")
	}
}

func TestHubNewestStreamWins(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	first := h.attach("node-1")
	second := h.attach("node-1")

	select {
	case <-first.replaced:
	default:
		t.Fatal("attaching a second stream must mark the first replaced")
	}
	if !h.send("node-1", logsCmd("c1")) {
		t.Fatal("send must reach the second stream")
	}
	select {
	case <-second.ch:
	default:
		t.Fatal("the command must land on the newest stream")
	}

	// The first handler's deferred detach runs after the replacement and
	// must not evict the newcomer.
	h.detach("node-1", first)
	if !h.send("node-1", logsCmd("c2")) {
		t.Fatal("a stale detach must leave the newest stream registered")
	}
	h.detach("node-1", second)
	if h.send("node-1", logsCmd("c3")) {
		t.Fatal("send must fail after the live stream detaches")
	}
}

func TestHubDetachFailsQueuedCommands(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	cs := h.attach("node-1")
	w := h.addWaiter("c1")
	if !h.send("node-1", logsCmd("c1")) {
		t.Fatal("send must succeed while the stream is attached")
	}

	h.detach("node-1", cs)
	got := drainWaiter(w)
	if len(got) != 1 || !got[0].GetEof() {
		t.Fatalf("waiter received %v, want one synthetic eof for the undelivered command", got)
	}
	if !strings.Contains(string(got[0].GetData()), "disconnected") {
		t.Errorf("synthetic eof data = %q, want a disconnect explanation", got[0].GetData())
	}
}

func TestHubSendFullQueueFails(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	cs := h.attach("node-1")
	for i := 0; h.send("node-1", logsCmd("cN")); i++ {
		if i > cap(cs.ch) {
			t.Fatal("send never reported a full queue")
		}
	}
}

func TestHubRouteToWaiter(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	w := h.addWaiter("c1")

	if !h.route(nil, output("c1", "hello")) {
		t.Fatal("route must deliver to a registered waiter")
	}
	select {
	case out := <-w.ch:
		if string(out.GetData()) != "hello" {
			t.Fatalf("waiter received %q, want hello", out.GetData())
		}
	default:
		t.Fatal("output never reached the waiter channel")
	}

	if h.route(nil, output("c2", "stray")) {
		t.Fatal("route must reject an unknown command id")
	}
	h.removeWaiter("c1")
	if h.route(nil, output("c1", "late")) {
		t.Fatal("route must reject a removed waiter")
	}
}

func TestHubRouteUnblocksWhenWaiterLeaves(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	w := h.addWaiter("c1")
	for range cap(w.ch) {
		if !h.route(nil, output("c1", "fill")) {
			t.Fatal("filling the waiter buffer must succeed")
		}
	}

	routed := make(chan bool)
	go func() { routed <- h.route(nil, output("c1", "blocked")) }()
	select {
	case <-routed:
		t.Fatal("route must block on a full waiter buffer")
	case <-time.After(50 * time.Millisecond):
	}

	h.removeWaiter("c1")
	select {
	case ok := <-routed:
		if ok {
			t.Fatal("route must report false once the waiter is gone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("route stayed blocked after the waiter left")
	}
}

// fakePushServer feeds PushCommandOutput a fixed message sequence, then err
// or a clean io.EOF.
type fakePushServer struct {
	grpc.ServerStream
	msgs   []*klitev1.CommandOutput
	err    error
	closed bool
}

func (s *fakePushServer) Recv() (*klitev1.CommandOutput, error) {
	if len(s.msgs) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

func (s *fakePushServer) SendAndClose(*klitev1.PushCommandOutputResponse) error {
	s.closed = true
	return nil
}

func (s *fakePushServer) Context() context.Context { return context.Background() }

func drainWaiter(w *outputWaiter) []*klitev1.CommandOutput {
	var out []*klitev1.CommandOutput
	for {
		select {
		case m := <-w.ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

func TestPushCommandOutputRoutesToWaiter(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	w := h.addWaiter("c1")
	a := NewAgent(nil, "tok", h, nil)
	stream := &fakePushServer{msgs: []*klitev1.CommandOutput{
		output("c1", "hello"),
		{CommandId: "c1", Eof: true},
	}}

	if err := a.PushCommandOutput(stream); err != nil {
		t.Fatal(err)
	}
	if !stream.closed {
		t.Error("a drained stream must be acked with SendAndClose")
	}
	got := drainWaiter(w)
	if len(got) != 2 || string(got[0].GetData()) != "hello" || !got[1].GetEof() {
		t.Errorf("waiter received %v, want the data then the eof", got)
	}
}

func TestPushCommandOutputSynthesizesEOFOnBrokenStream(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	w := h.addWaiter("c1")
	a := NewAgent(nil, "tok", h, nil)
	stream := &fakePushServer{
		msgs: []*klitev1.CommandOutput{output("c1", "hello")},
		err:  errors.New("agent vanished"),
	}

	if err := a.PushCommandOutput(stream); err == nil {
		t.Fatal("a broken stream must surface its error")
	}
	got := drainWaiter(w)
	if len(got) != 2 {
		t.Fatalf("waiter received %d messages, want data plus synthetic eof", len(got))
	}
	last := got[1]
	if !last.GetEof() || !strings.Contains(string(last.GetData()), "agent vanished") {
		t.Errorf("synthetic final message = %v, want eof naming the break", last)
	}
}

func TestPushCommandOutputRejectsUnknownCommand(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	a := NewAgent(nil, "tok", h, nil)
	stream := &fakePushServer{msgs: []*klitev1.CommandOutput{output("ghost", "x")}}

	if err := a.PushCommandOutput(stream); err == nil {
		t.Fatal("output with no waiter must end the stream with an error")
	}
}

func TestHubRouteUnblocksOnStop(t *testing.T) {
	t.Parallel()
	h := NewCommandHub()
	w := h.addWaiter("c1")
	for range cap(w.ch) {
		h.route(nil, output("c1", "fill"))
	}

	stop := make(chan struct{})
	routed := make(chan bool)
	go func() { routed <- h.route(stop, output("c1", "blocked")) }()
	close(stop)
	select {
	case ok := <-routed:
		if ok {
			t.Fatal("route must report false when the pusher stops")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("route ignored the stop channel")
	}
}

package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// fakePush records one PushCommandOutput stream. Data is cloned because the
// pump reuses its read buffer between sends, which real gRPC tolerates by
// marshaling inside Send.
type fakePush struct {
	grpc.ClientStream
	mu     sync.Mutex
	sent   []*klitev1.CommandOutput
	closed bool
}

func (p *fakePush) Send(out *klitev1.CommandOutput) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, proto.CloneOf(out))
	return nil
}

func (p *fakePush) CloseAndRecv() (*klitev1.PushCommandOutputResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return &klitev1.PushCommandOutputResponse{}, nil
}

func (p *fakePush) snapshot() []*klitev1.CommandOutput {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*klitev1.CommandOutput, len(p.sent))
	copy(out, p.sent)
	return out
}

// fakeAgentClient serves PushCommandOutput only. The embedded nil interface
// panics on anything else, which no command-path test should touch.
type fakeAgentClient struct {
	klitev1.AgentServiceClient
	mu     sync.Mutex
	pushes []*fakePush
}

func (c *fakeAgentClient) PushCommandOutput(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[klitev1.CommandOutput, klitev1.PushCommandOutputResponse], error) {
	p := &fakePush{}
	c.mu.Lock()
	c.pushes = append(c.pushes, p)
	c.mu.Unlock()
	return p, nil
}

func (c *fakeAgentClient) pushCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pushes)
}

func commandAgent(t *testing.T, rt *fakeRuntime) (*Agent, *fakeAgentClient) {
	t.Helper()
	fc := &fakeAgentClient{}
	a := New(&Config{Node: "node-1", Token: "dev-token", Runtime: rt, Client: fc})
	a.mu.Lock()
	a.states["a-xx"] = &instState{uid: "uid-1", containerID: "ctr-1"}
	a.mu.Unlock()
	return a, fc
}

func logsCommand(id string, follow bool, tail int32) *klitev1.Command {
	return &klitev1.Command{Id: id, Cmd: &klitev1.Command_Logs{Logs: &klitev1.LogsCommand{
		Instance: "a-xx", Follow: follow, Tail: tail,
	}}}
}

func stopCommand(id string) *klitev1.Command {
	return &klitev1.Command{Id: id, Cmd: &klitev1.Command_Stop{Stop: &klitev1.StopCommand{CommandId: id}}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runningCommands(a *Agent) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.commands)
}

func TestLogsCommandStreamsUntilEOF(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	var gotID string
	var gotFollow bool
	var gotTail int32
	rt.logsFn = func(_ context.Context, id string, follow bool, tail int32) (io.ReadCloser, error) {
		gotID, gotFollow, gotTail = id, follow, tail
		return io.NopCloser(strings.NewReader("line1\nline2\n")), nil
	}
	a, fc := commandAgent(t, rt)

	a.dispatch(context.Background(), logsCommand("c1", false, 3))
	a.cmdWG.Wait()

	if gotID != "ctr-1" || gotFollow || gotTail != 3 {
		t.Errorf("Logs called with (%q, %t, %d), want (ctr-1, false, 3)", gotID, gotFollow, gotTail)
	}
	if fc.pushCount() != 1 {
		t.Fatalf("push streams = %d, want 1", fc.pushCount())
	}
	sent := fc.pushes[0].snapshot()
	if len(sent) < 2 {
		t.Fatalf("sent %d messages, want data plus eof", len(sent))
	}
	var data strings.Builder
	for _, m := range sent[:len(sent)-1] {
		if m.GetCommandId() != "c1" || m.GetEof() {
			t.Errorf("mid-stream message = %v, want tagged c1 without eof", m)
		}
		data.Write(m.GetData())
	}
	if data.String() != "line1\nline2\n" {
		t.Errorf("streamed %q, want the reader's content", data.String())
	}
	last := sent[len(sent)-1]
	if !last.GetEof() || len(last.GetData()) != 0 {
		t.Errorf("final message = %v, want bare eof", last)
	}
	if !fc.pushes[0].closed {
		t.Error("push stream was never closed")
	}
	if runningCommands(a) != 0 {
		t.Error("command registry must be empty after eof")
	}
}

func TestLogsCommandStopEndsFollow(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.logsFn = func(ctx context.Context, _ string, _ bool, _ int32) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("hello\n"))
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, nil
	}
	a, fc := commandAgent(t, rt)

	a.dispatch(context.Background(), logsCommand("c1", true, 0))
	waitFor(t, "first chunk", func() bool {
		return fc.pushCount() == 1 && len(fc.pushes[0].snapshot()) > 0
	})

	// A duplicate delivery of a running command must not spawn a second pump.
	a.dispatch(context.Background(), logsCommand("c1", true, 0))

	a.dispatch(context.Background(), stopCommand("c1"))
	a.cmdWG.Wait()

	if fc.pushCount() != 1 {
		t.Fatalf("push streams = %d, want 1 (duplicate id must be dropped)", fc.pushCount())
	}
	sent := fc.pushes[0].snapshot()
	last := sent[len(sent)-1]
	if !last.GetEof() || len(last.GetData()) != 0 {
		t.Errorf("final message = %v, want bare eof: a stop is not a failure", last)
	}
	if runningCommands(a) != 0 {
		t.Error("command registry must be empty after stop")
	}
}

func TestLogsCommandReportsReaderError(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.logsFn = func(context.Context, string, bool, int32) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("x\n"))
			_ = pw.CloseWithError(errors.New("daemon hiccup"))
		}()
		return pr, nil
	}
	a, fc := commandAgent(t, rt)

	a.dispatch(context.Background(), logsCommand("c1", true, 0))
	a.cmdWG.Wait()

	sent := fc.pushes[0].snapshot()
	last := sent[len(sent)-1]
	if !last.GetEof() || !strings.Contains(string(last.GetData()), "daemon hiccup") {
		t.Errorf("final message = %v, want eof carrying the reader error", last)
	}
}

func TestLogsCommandUnknownInstance(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a, fc := commandAgent(t, rt)

	a.dispatch(context.Background(), &klitev1.Command{Id: "c1", Cmd: &klitev1.Command_Logs{
		Logs: &klitev1.LogsCommand{Instance: "ghost"},
	}})
	a.cmdWG.Wait()

	sent := fc.pushes[0].snapshot()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want a lone eof", len(sent))
	}
	if !sent[0].GetEof() || !strings.Contains(string(sent[0].GetData()), "no container") {
		t.Errorf("final message = %v, want eof explaining the missing container", sent[0])
	}
}

func TestConcurrentLogsCommands(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.logsFn = func(ctx context.Context, _ string, _ bool, _ int32) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("tick\n"))
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, nil
	}
	a, fc := commandAgent(t, rt)

	a.dispatch(context.Background(), logsCommand("c1", true, 0))
	a.dispatch(context.Background(), logsCommand("c2", true, 0))
	waitFor(t, "both streams to produce output", func() bool {
		if fc.pushCount() != 2 {
			return false
		}
		return len(fc.pushes[0].snapshot()) > 0 && len(fc.pushes[1].snapshot()) > 0
	})
	if runningCommands(a) != 2 {
		t.Errorf("running commands = %d, want 2", runningCommands(a))
	}

	a.dispatch(context.Background(), stopCommand("c1"))
	a.dispatch(context.Background(), stopCommand("c2"))
	a.cmdWG.Wait()

	seen := map[string]bool{}
	for _, p := range fc.pushes {
		sent := p.snapshot()
		last := sent[len(sent)-1]
		if !last.GetEof() {
			t.Errorf("stream for %s never sent eof", last.GetCommandId())
		}
		seen[sent[0].GetCommandId()] = true
	}
	if !seen["c1"] || !seen["c2"] {
		t.Errorf("streams seen = %v, want both c1 and c2", seen)
	}
}

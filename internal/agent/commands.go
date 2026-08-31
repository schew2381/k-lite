package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Command output travels in messages this size, well under the 4MB gRPC cap
// (research/grpc-go.md).
const logChunkSize = 32 * 1024

// commandLoop keeps a StreamCommands stream open with the same reconnect
// backoff as watchLoop. The agent dials, per ADR 0004, so this stream is how
// the server reaches down to a node at all.
func (a *Agent) commandLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := a.now()
		err := a.commandsOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("command stream broke, reconnecting", "err", err, "backoff", backoff)
		if !sleep(ctx, backoff) {
			return
		}
		if a.now().Sub(started) > time.Minute {
			backoff = time.Second
		} else {
			backoff = min(backoff*2, retryBackoffMax)
		}
	}
}

func (a *Agent) commandsOnce(ctx context.Context) error {
	stream, err := a.client.StreamCommands(ctx, &klitev1.StreamCommandsRequest{Node: a.node})
	if err != nil {
		return err
	}
	for {
		cmd, err := stream.Recv()
		if err != nil {
			return err
		}
		a.dispatch(ctx, cmd)
	}
}

// dispatch starts logs commands and cancels stopped ones. Each logs command
// runs in its own goroutine under its own cancelable context, so several can
// stream at once and none of them blocks command intake.
func (a *Agent) dispatch(ctx context.Context, cmd *klitev1.Command) {
	switch c := cmd.GetCmd().(type) {
	case *klitev1.Command_Logs:
		a.startLogs(ctx, cmd.GetId(), c.Logs)
	case *klitev1.Command_Stop:
		a.stopCommand(c.Stop.GetCommandId())
	default:
		slog.Warn("unknown command ignored", "id", cmd.GetId())
	}
}

// startLogs registers the command and hands it to a goroutine. A duplicate id
// is dropped, since the first delivery already owns it.
func (a *Agent) startLogs(ctx context.Context, id string, lc *klitev1.LogsCommand) {
	cmdCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if _, dup := a.commands[id]; dup {
		a.mu.Unlock()
		cancel()
		return
	}
	a.commands[id] = cancel
	a.mu.Unlock()
	a.cmdWG.Add(1)
	go func() {
		defer a.cmdWG.Done()
		defer a.stopCommand(id) // releases cmdCtx once the pump ends on its own
		a.runLogs(ctx, cmdCtx, id, lc)
	}()
}

// stopCommand cancels a running command. An unknown id either finished
// already or belonged to a previous agent life, so dropping it is fine.
func (a *Agent) stopCommand(id string) {
	a.mu.Lock()
	cancel := a.commands[id]
	delete(a.commands, id)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// runLogs pumps one instance's container logs into a PushCommandOutput
// stream. The final message always carries eof. When the pump broke rather
// than drained, the failure text rides along as the last chunk so the user
// sees why their logs stopped. The push stream hangs off ctx, not cmdCtx, so
// a StopCommand can't kill it before that final message goes out.
func (a *Agent) runLogs(ctx, cmdCtx context.Context, id string, lc *klitev1.LogsCommand) {
	push, err := a.client.PushCommandOutput(ctx)
	if err != nil {
		slog.Warn("command output stream failed", "command", id, "err", err)
		return
	}
	failure := a.pumpLogs(cmdCtx, push, id, lc)
	final := &klitev1.CommandOutput{CommandId: id, Eof: true}
	if failure != nil && cmdCtx.Err() == nil {
		final.Data = []byte(failure.Error() + "\n")
	}
	if err := push.Send(final); err != nil {
		slog.Warn("command eof send failed", "command", id, "err", err)
	}
	if _, err := push.CloseAndRecv(); err != nil && ctx.Err() == nil {
		slog.Warn("command output close failed", "command", id, "err", err)
	}
	slog.Info("logs command finished", "command", id, "instance", lc.GetInstance())
}

// pumpLogs opens the container's log reader and forwards chunks until the
// reader drains (nil) or breaks (the error to report). Cancelling cmdCtx
// closes the reader from under the pump, which is how StopCommand lands.
func (a *Agent) pumpLogs(cmdCtx context.Context, push klitev1.AgentService_PushCommandOutputClient, id string, lc *klitev1.LogsCommand) error {
	ctr := a.containerFor(lc.GetInstance())
	if ctr == "" {
		return fmt.Errorf("instance %s has no container on node %s", lc.GetInstance(), a.node)
	}
	rc, err := a.rt.Logs(cmdCtx, ctr, lc.GetFollow(), lc.GetTail())
	if err != nil {
		return err
	}
	defer rc.Close()
	buf := make([]byte, logChunkSize)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if serr := push.Send(&klitev1.CommandOutput{CommandId: id, Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		switch {
		case errors.Is(rerr, io.EOF):
			return nil
		case rerr != nil:
			return rerr
		}
	}
}

// containerFor looks up the instance's current container id in the
// reconciler's state.
func (a *Agent) containerFor(instance string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.states[instance]; ok {
		return st.containerID
	}
	return ""
}

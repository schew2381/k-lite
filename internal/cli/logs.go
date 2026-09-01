package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func newLogsCmd(cfg *connCfg) *cobra.Command {
	var follow bool
	var tail int32
	cmd := &cobra.Command{
		Use:   "logs <instance>",
		Short: "Print an instance's container logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd.Context(), cmd.OutOrStdout(), cfg, &klitev1.LogsRequest{
				Instance: args[0],
				Follow:   follow,
				Tail:     tail,
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new lines until interrupted")
	cmd.Flags().Int32Var(&tail, "tail", 0, "print only the last N lines (0 means all)")
	return cmd
}

// runLogs walks the endpoints one at a time. A klited only serves logs for
// agents streaming into it, so FailedPrecondition (wrong replica) and
// Unavailable (dead replica) both mean "ask the next one". M7 moves this
// routing server-side so any replica can answer.
func runLogs(ctx context.Context, w io.Writer, cfg *connCfg, req *klitev1.LogsRequest) error {
	var lastErr error
	for _, ep := range endpoints(cfg.server) {
		done, err := logsFrom(ctx, w, cfg, ep, req)
		if done {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// logsFrom streams from one endpoint until eof or interrupt. done=false means
// this endpoint can't serve the stream and the caller should try another.
func logsFrom(ctx context.Context, w io.Writer, cfg *connCfg, ep string, req *klitev1.LogsRequest) (done bool, err error) {
	conn, client, err := dialOne(cfg, ep)
	if err != nil {
		return true, err
	}
	defer conn.Close()
	stream, err := client.Logs(ctx, req)
	if err != nil {
		return !retryElsewhere(err), rpcErr(err)
	}
	received := false
	for {
		chunk, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return true, nil
		case status.Code(err) == codes.Canceled && ctx.Err() != nil:
			return true, nil // Ctrl-C on a follow is a clean exit
		case err != nil:
			// Once data flowed, this server was the right one and the
			// failure is real. Retrying would replay lines from the top.
			if !received && retryElsewhere(err) {
				return false, rpcErr(err)
			}
			return true, rpcErr(err)
		}
		received = true
		if _, err := w.Write(chunk.GetData()); err != nil {
			return true, fmt.Errorf("write output: %w", err)
		}
	}
}

// retryElsewhere reports whether another endpoint might succeed where this
// one refused.
func retryElsewhere(err error) bool {
	code := status.Code(err)
	return code == codes.FailedPrecondition || code == codes.Unavailable
}

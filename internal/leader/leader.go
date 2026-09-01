// Package leader elects one active controller set per cluster through etcd.
package leader

import (
	"context"
	"log/slog"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

const (
	electionPrefix = "/klite/leader"
	// Session TTL bounds takeover after a SIGKILL. Clean shutdowns resign and hand over at once.
	sessionTTL      = 5 // seconds
	campaignBackoff = 2 * time.Second
)

// RunWhenLeader campaigns until it wins, runs fn with a context that's canceled on
// leadership loss, then campaigns again. standby (optional) runs before each campaign.
// The loop only returns when ctx ends, and never gives up when etcd is unreachable.
func RunWhenLeader(ctx context.Context, cli *clientv3.Client, id string, standby func(), fn func(context.Context)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if standby != nil {
			standby()
		}
		sess, err := concurrency.NewSession(cli, concurrency.WithTTL(sessionTTL), concurrency.WithContext(ctx))
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("leader session failed, backing off", "err", err)
			}
			if !sleep(ctx, campaignBackoff) {
				return ctx.Err()
			}
			continue
		}
		election := concurrency.NewElection(sess, electionPrefix)
		if err := election.Campaign(ctx, id); err != nil {
			sess.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Back off here too: a session can keep succeeding while the
			// campaign txn keeps failing, and without a pause that loop
			// hammers etcd flat out.
			slog.Warn("leader campaign failed, backing off", "err", err)
			if !sleep(ctx, campaignBackoff) {
				return ctx.Err()
			}
			continue
		}

		leadCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			fn(leadCtx)
		}()
		select {
		case <-sess.Done(): // lease expired or quorum lost, stop actuating now
		case <-ctx.Done():
		case <-done:
		}
		cancel()
		<-done

		// Resign so a standby takes over immediately instead of waiting out
		// the TTL. Use a fresh context: on shutdown ctx is already canceled,
		// and the resign still has to go out.
		resignCtx, resignCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = election.Resign(resignCtx)
		resignCancel()
		sess.Close()
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

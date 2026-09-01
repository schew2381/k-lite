package leader_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/schew2381/k-lite/internal/leader"
)

// With etcd unreachable the loop must keep retrying, run standby while it
// waits, never start fn, and return as soon as ctx ends.
func TestRunWhenLeaderReturnsOnCancelWithoutEtcd(t *testing.T) {
	t.Parallel()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"127.0.0.1:1"}, // nothing listens here
		DialTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(t.Context())
	var standbys, fnRuns atomic.Int32
	result := make(chan error, 1)
	go func() {
		result <- leader.RunWhenLeader(ctx, cli, "test", func() {
			standbys.Add(1)
		}, func(context.Context) {
			fnRuns.Add(1)
		})
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunWhenLeader = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunWhenLeader did not return after cancel")
	}
	if standbys.Load() == 0 {
		t.Error("standby never ran")
	}
	if fnRuns.Load() != 0 {
		t.Errorf("fn ran %d times without etcd", fnRuns.Load())
	}
}

//go:build integration

package leader_test

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/schew2381/k-lite/internal/leader"
)

const etcdEndpoint = "127.0.0.1:2379"

func newClient(t *testing.T) *clientv3.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", etcdEndpoint, 500*time.Millisecond)
	if err != nil {
		t.Skipf("etcd not reachable on %s, run hack/etcd-up.sh first", etcdEndpoint)
	}
	conn.Close()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdEndpoint}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// run starts RunWhenLeader with an fn that signals each election and blocks
// until leadership ends.
func run(ctx context.Context, cli *clientv3.Client, id string, elected chan<- string) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- leader.RunWhenLeader(ctx, cli, id, nil, func(leadCtx context.Context) {
			select {
			case elected <- id:
			case <-leadCtx.Done():
				return
			}
			<-leadCtx.Done()
		})
	}()
	return result
}

func TestLeadershipCancelStopsFn(t *testing.T) {
	cli := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	elected := make(chan string, 1)
	result := run(ctx, cli, "solo", elected)

	select {
	case <-elected:
	case <-time.After(10 * time.Second):
		t.Fatal("never elected")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Error("RunWhenLeader returned nil, want ctx error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunWhenLeader did not return after cancel")
	}
}

// A clean shutdown resigns, so the standby takes over well inside the
// session TTL.
func TestHandoverOnShutdown(t *testing.T) {
	cli := newClient(t)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	elected := make(chan string, 2)
	resultA := run(ctxA, cli, "a", elected)
	select {
	case id := <-elected:
		if id != "a" {
			t.Fatalf("first leader = %s, want a", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a never elected")
	}

	resultB := run(ctxB, cli, "b", elected)
	cancelA()
	start := time.Now()
	select {
	case id := <-elected:
		if id != "b" {
			t.Fatalf("second leader = %s, want b", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("b never took over")
	}
	// Resign-based handover beats lease expiry. Allow slack for CI, but a
	// takeover near the 5s TTL means the resign path didn't fire.
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("handover took %v, want well under the session TTL", took)
	}
	<-resultA
	cancelB()
	<-resultB
}

// Losing the session (lease revoked out from under the leader) cancels fn
// and the loop campaigns again on a fresh session.
func TestSessionLossCancelsFnAndRecampaigns(t *testing.T) {
	cli := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cycles atomic.Int32
	result := make(chan error, 1)
	started := make(chan struct{}, 4)
	go func() {
		result <- leader.RunWhenLeader(ctx, cli, "flaky", nil, func(leadCtx context.Context) {
			cycles.Add(1)
			started <- struct{}{}
			<-leadCtx.Done()
		})
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("never elected")
	}

	// Revoke the lease behind the election key to simulate session loss.
	resp, err := cli.Get(context.Background(), "/klite/leader", clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	revoked := false
	for _, kv := range resp.Kvs {
		if strings.HasPrefix(string(kv.Key), "/klite/leader") && kv.Lease != 0 {
			if _, err := cli.Revoke(context.Background(), clientv3.LeaseID(kv.Lease)); err != nil {
				t.Fatal(err)
			}
			revoked = true
		}
	}
	if !revoked {
		t.Fatal("no leased election key found to revoke")
	}

	select {
	case <-started: // second cycle proves fn was canceled and re-elected
	case <-time.After(15 * time.Second):
		t.Fatalf("no re-election after session loss, cycles = %d", cycles.Load())
	}
	cancel()
	<-result
}

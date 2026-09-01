package netd

import (
	"fmt"
	"sync"
	"testing"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// fakeClock hands the queryLog a controllable now().
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.UnixMilli(1_700_000_000_000)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func testQueryLog() (*queryLog, *fakeClock) {
	l := newQueryLog()
	c := newFakeClock()
	l.now = c.now
	return l, c
}

func TestQueryLogRecordAndSince(t *testing.T) {
	l, clk := testQueryLog()
	t0 := clk.now().UnixMilli()
	l.record("10.44.128.5", "b")
	clk.advance(time.Second)
	l.record("10.44.128.6", "c")

	got := l.since(0)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	want := []*klitev1.RecentQuery{
		{SourceIp: "10.44.128.5", Service: "b", UnixMs: t0},
		{SourceIp: "10.44.128.6", Service: "c", UnixMs: t0 + 1000},
	}
	for i, w := range want {
		g := got[i]
		if g.GetSourceIp() != w.GetSourceIp() || g.GetService() != w.GetService() || g.GetUnixMs() != w.GetUnixMs() {
			t.Errorf("entry %d = %v, want %v", i, g, w)
		}
	}
}

func TestQueryLogSinceIsInclusive(t *testing.T) {
	l, clk := testQueryLog()
	l.record("10.44.128.5", "b") // t0
	clk.advance(time.Second)
	cursor := clk.now().UnixMilli()
	l.record("10.44.128.5", "c") // exactly at the cursor
	clk.advance(time.Second)
	l.record("10.44.128.5", "d")

	got := l.since(cursor)
	if len(got) != 2 || got[0].GetService() != "c" || got[1].GetService() != "d" {
		t.Fatalf("since(cursor) = %v, want [c d]", got)
	}
}

func TestQueryLogRetention(t *testing.T) {
	l, clk := testQueryLog()
	l.record("10.44.128.5", "b")
	clk.advance(queryRetention + time.Millisecond)
	if got := l.since(0); len(got) != 0 {
		t.Fatalf("aged-out entries still returned: %v", got)
	}
	l.record("10.44.128.6", "c")
	got := l.since(0)
	if len(got) != 1 || got[0].GetService() != "c" {
		t.Fatalf("after aging = %v, want just c", got)
	}
}

func TestQueryLogWraparound(t *testing.T) {
	l, _ := testQueryLog()
	const extra = 5
	for i := range queryCapacity + extra {
		l.record(fmt.Sprintf("10.0.%d.%d", i/256%256, i%256), fmt.Sprintf("svc-%d", i))
	}
	got := l.since(0)
	if len(got) != queryCapacity {
		t.Fatalf("entries = %d, want the full ring %d", len(got), queryCapacity)
	}
	// The oldest surviving entry is the first one not overwritten.
	if got[0].GetService() != fmt.Sprintf("svc-%d", extra) {
		t.Errorf("oldest = %s, want svc-%d", got[0].GetService(), extra)
	}
	if last := got[len(got)-1].GetService(); last != fmt.Sprintf("svc-%d", queryCapacity+extra-1) {
		t.Errorf("newest = %s, want svc-%d", last, queryCapacity+extra-1)
	}
}

// Recorders and readers race freely, and -race plus the shape checks catch
// torn state. Every observed entry must be internally consistent (its
// service matches its source IP by construction).
func TestQueryLogConcurrency(t *testing.T) {
	l, _ := testQueryLog()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				n := w*2000 + i
				l.record(fmt.Sprintf("10.0.0.%d", n%256), fmt.Sprintf("svc-%d", n%256))
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		for _, e := range l.since(0) {
			wantIP := "10.0.0." + e.GetService()[len("svc-"):]
			if e.GetSourceIp() != wantIP {
				t.Fatalf("torn entry: %s with %s", e.GetService(), e.GetSourceIp())
			}
		}
		select {
		case <-done:
			if got := l.since(0); len(got) != queryCapacity {
				t.Fatalf("entries = %d, want %d after 16000 records", len(got), queryCapacity)
			}
			return
		default:
		}
	}
}

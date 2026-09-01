package netd

import (
	"sync"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const (
	// queryRetention is how far back RecentQueries reaches. Entries age out
	// on read. Nothing sweeps in the background, since the ring's fixed size
	// already bounds memory.
	queryRetention = 30 * time.Second
	// queryCapacity is the ring size. At the demo's pace (a few queries a
	// second per node) 4096 slots hold minutes of history, far past the
	// retention window. Under a query flood the ring overwrites its oldest
	// entries instead of growing.
	queryCapacity = 4096
)

// queryLog is a fixed ring of the in-zone A answers this node's kdns served:
// which container IP asked for which service, and when. The DNS handler runs
// one goroutine per query, so record keeps its critical section to a slot
// write and two index bumps. Everything else (aging, filtering, proto
// conversion) happens on the read side, which only the admin RPC touches.
type queryLog struct {
	now func() time.Time // test hook, time.Now outside tests

	mu   sync.Mutex
	buf  []queryEntry // fixed at queryCapacity
	next int          // slot the next record lands in
	size int          // filled slots, tops out at len(buf)
}

type queryEntry struct {
	sourceIP string
	service  string
	unixMS   int64
}

func newQueryLog() *queryLog {
	return &queryLog{now: time.Now, buf: make([]queryEntry, queryCapacity)}
}

// record appends one served answer, overwriting the oldest entry when the
// ring is full. The timestamp is read under the lock so ring order and time
// order agree.
func (l *queryLog) record(sourceIP, service string) {
	l.mu.Lock()
	l.buf[l.next] = queryEntry{sourceIP: sourceIP, service: service, unixMS: l.now().UnixMilli()}
	l.next = (l.next + 1) % len(l.buf)
	l.size = min(l.size+1, len(l.buf))
	l.mu.Unlock()
}

// since returns the retained entries with unix_ms at or after sinceUnixMS,
// oldest first. A poller feeds its last seen timestamp back in and misses
// nothing, re-reading at most the entries sharing that millisecond. Entries
// older than queryRetention are dropped whatever the cursor says.
//
// The lock covers one allocation and the filtered copy; proto conversion
// happens after unlock, so a slow read never stalls the DNS handler's
// record calls behind 4096 message allocations.
func (l *queryLog) since(sinceUnixMS int64) []*klitev1.RecentQuery {
	floor := max(sinceUnixMS, l.now().Add(-queryRetention).UnixMilli())
	l.mu.Lock()
	kept := make([]queryEntry, 0, l.size)
	for i := range l.size {
		e := l.buf[(l.next-l.size+i+len(l.buf))%len(l.buf)]
		if e.unixMS >= floor {
			kept = append(kept, e)
		}
	}
	l.mu.Unlock()
	out := make([]*klitev1.RecentQuery, len(kept))
	for i, e := range kept {
		out[i] = &klitev1.RecentQuery{SourceIp: e.sourceIP, Service: e.service, UnixMs: e.unixMS}
	}
	return out
}

package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// trafficFake lists one Ready node with index 1.
type trafficFake struct{ *fakeClient }

func (trafficFake) List(context.Context, *klitev1.ListRequest, ...grpc.CallOption) (*klitev1.ListResponse, error) {
	return &klitev1.ListResponse{Objects: []*klitev1.Object{{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
		Meta:   &klitev1.Meta{Name: "node-1"},
		Status: &klitev1.NodeStatus{Phase: klitev1.NodePhase_NODE_PHASE_READY, NodeIndex: 1},
	}}}}}, nil
}

// fakeAdmin serves /clusters with a mutable rq_total for service b plus an
// ingress-side cluster the feed must not count, and /stats with mutable
// per-phase rbac denied counters.
func fakeAdmin(t *testing.T, total, denied *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/stats") {
			// Real filters emit bare per-phase prefixes (pinned against the
			// reseeded cluster): <service>_deny / <service>_allow.
			fmt.Fprintf(w, "b_deny.rbac.allowed: 12\nb_deny.rbac.denied: %d\nd_allow.rbac.denied: 0\n", denied.Load())
			return
		}
		fmt.Fprintf(w, `{"cluster_statuses":[
			{"name":"b","host_statuses":[{"address":{"socket_address":{"address":"10.44.128.6","port_value":80}},"stats":[{"name":"rq_total","value":"%d"},{"name":"cx_total","value":"1"}]}]},
			{"name":"ingress/b/20037","host_statuses":[{"address":{"socket_address":{"address":"10.44.128.15","port_value":80}},"stats":[{"name":"rq_total","value":"999"}]}]}
		]}`, total.Load())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTrafficFeedStreamsDeltas(t *testing.T) {
	var total, denied atomic.Int64
	total.Store(40)
	denied.Store(7)
	admin := fakeAdmin(t, &total, &denied)

	tf := newTrafficFeed(trafficFake{&fakeClient{}})
	tf.interval = 20 * time.Millisecond
	tf.adminURL = func(idx int32) string {
		if idx != 1 {
			t.Errorf("unexpected node index %d", idx)
		}
		return admin.URL
	}

	ch := tf.subscribe()
	defer tf.unsubscribe(ch)

	// The first polls only prime the baseline; no event may carry the
	// starting total of 40.
	select {
	case b := <-ch:
		t.Fatalf("event before any delta: %s", b)
	case <-time.After(80 * time.Millisecond):
	}

	total.Add(3)
	var ev trafficEvent
	select {
	case b := <-ch:
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after counters moved")
	}
	want := trafficEvent{Node: "node-1", Service: "b", Address: "10.44.128.6", Port: 80, Count: 3, Verdict: "allowed"}
	ev.UnixMs = 0
	if ev != want {
		t.Fatalf("got %+v, want %+v", ev, want)
	}

	// The ingress/… cluster moves every poll (999 constant, so it never
	// deltas) — reaching here without a second event proves it was skipped.
	select {
	case b := <-ch:
		t.Fatalf("unexpected extra event: %s", b)
	case <-time.After(60 * time.Millisecond):
	}

	// A moving rbac denied counter becomes a denied event naming the phase.
	// The starting total of 7 was baseline, so the count is the delta alone.
	denied.Add(2)
	ev = trafficEvent{}
	select {
	case b := <-ch:
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no denied event after rbac counters moved")
	}
	wantDenied := trafficEvent{Node: "node-1", Service: "b", Count: 2, Verdict: "denied", Phase: "deny"}
	ev.UnixMs = 0
	if ev != wantDenied {
		t.Fatalf("got %+v, want %+v", ev, wantDenied)
	}
}

func TestTrafficRouteIsSSE(t *testing.T) {
	srv := New(trafficFake{&fakeClient{}}, []string{"127.0.0.1:7443"}, "", false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/traffic", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
}

// fakeNetd answers RecentQueries with a canned ring.
type fakeNetd struct {
	klitev1.UnimplementedKliteNetServiceServer
	queries []*klitev1.RecentQuery
}

func (f *fakeNetd) RecentQueries(context.Context, *klitev1.RecentQueriesRequest) (*klitev1.RecentQueriesResponse, error) {
	return &klitev1.RecentQueriesResponse{Queries: f.queries}, nil
}

func TestAttributeJoinsKdnsQueries(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	klitev1.RegisterKliteNetServiceServer(srv, &fakeNetd{queries: []*klitev1.RecentQuery{
		{SourceIp: "10.44.128.5", Service: "b", UnixMs: 1},
		{SourceIp: "10.44.128.9", Service: "b", UnixMs: 2},
	}})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	tf := newTrafficFeed(trafficFake{&fakeClient{}})
	tf.netAddr = func(int32) string { return lis.Addr().String() }

	events := tf.attribute(context.Background(), 1, 10_000, []trafficEvent{
		{Node: "node-1", Service: "b", Count: 3, Verdict: "allowed"},
		{Node: "node-1", Service: "c", Count: 1, Verdict: "denied", Phase: "deny"},
	})

	var callers []string
	rest := 0
	for _, ev := range events {
		if ev.Service != "b" {
			continue
		}
		if ev.Caller != "" {
			callers = append(callers, ev.Caller)
			if ev.Count != 1 {
				t.Fatalf("an attributed call is a single call, got count %d", ev.Count)
			}
		} else {
			rest += ev.Count
		}
	}
	if len(callers) != 2 || callers[0] != "10.44.128.5" || callers[1] != "10.44.128.9" {
		t.Fatalf("callers %v", callers)
	}
	if rest != 1 {
		t.Fatalf("unattributed remainder %d, want 1", rest)
	}
	// c had no matching query, so its event rides through untouched
	for _, ev := range events {
		if ev.Service == "c" && (ev.Caller != "" || ev.Count != 1) {
			t.Fatalf("denied c event changed: %+v", ev)
		}
	}
}

func TestAttributeSurvivesAMissingRing(t *testing.T) {
	tf := newTrafficFeed(trafficFake{&fakeClient{}})
	tf.netAddr = func(int32) string { return "127.0.0.1:1" } // nothing listens
	in := []trafficEvent{{Node: "node-1", Service: "b", Count: 2, Verdict: "allowed"}}
	out := tf.attribute(context.Background(), 1, 10_000, in)
	if len(out) != 1 || out[0].Caller != "" || out[0].Count != 2 {
		t.Fatalf("degraded path changed the event: %+v", out)
	}
}

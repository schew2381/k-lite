package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// GET /api/traffic is ADR 0024's feed, finally served (ADR 0041). Every node
// publishes its Envoy admin on loopback at 19500 plus its node index, and
// each admin's /clusters page counts requests per upstream host. The facade
// polls those counters once a second while anyone is subscribed and streams
// the deltas as SSE. A delta names the caller's node, the target service,
// and the exact dial target, which is everything the UI needs to fly a dot.
// The caller's instance stays unknown, since one Envoy fronts every
// instance on its node.

const trafficPollInterval = time.Second

type trafficEvent struct {
	UnixMs  int64  `json:"unixMs"`
	Node    string `json:"node"`             // caller's node, whose Envoy counted the calls
	Service string `json:"service"`          // target service, the Envoy cluster name
	Address string `json:"address,omitzero"` // dial target: an instance IP or a machine address
	Port    int    `json:"port,omitzero"`
	Count   int    `json:"count"`              // calls since the previous poll
	Verdict string `json:"verdict"`            // allowed | denied
	Phase   string `json:"rbacPhase,omitzero"` // denied only: deny | allow, which RBAC filter killed it
	Caller  string `json:"caller,omitzero"`    // caller instance IP, when kdns saw the lookup
}

// trafficFeed fans Envoy counter deltas to SSE subscribers. The baselines
// live only while subscribers exist and die with the last one, so a fresh
// subscriber never gets old totals replayed as one burst.
type trafficFeed struct {
	client   klitev1.ClusterServiceClient
	adminURL func(nodeIndex int32) string
	interval time.Duration
	httpc    *http.Client

	// netAddr names each node's klite-net admin gRPC, whose RecentQueries
	// ring attributes calls to the instance that resolved the name.
	netAddr  func(nodeIndex int32) string
	netConns map[int32]*grpc.ClientConn

	mu   sync.Mutex
	subs map[chan []byte]struct{}
	stop context.CancelFunc
	prev map[string]int64 // node|cluster|address:port → rq_total
}

func newTrafficFeed(client klitev1.ClusterServiceClient) *trafficFeed {
	return &trafficFeed{
		client: client,
		adminURL: func(nodeIndex int32) string {
			return fmt.Sprintf("http://127.0.0.1:%d", 19500+nodeIndex)
		},
		netAddr: func(nodeIndex int32) string {
			return fmt.Sprintf("127.0.0.1:%d", 19000+nodeIndex)
		},
		interval: trafficPollInterval,
		httpc:    &http.Client{Timeout: 800 * time.Millisecond},
		subs:     map[chan []byte]struct{}{},
		netConns: map[int32]*grpc.ClientConn{},
	}
}

func (tf *trafficFeed) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	tf.mu.Lock()
	defer tf.mu.Unlock()
	tf.subs[ch] = struct{}{}
	if tf.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		tf.stop = cancel
		tf.prev = map[string]int64{}
		go tf.run(ctx)
	}
	return ch
}

func (tf *trafficFeed) unsubscribe(ch chan []byte) {
	tf.mu.Lock()
	defer tf.mu.Unlock()
	delete(tf.subs, ch)
	if len(tf.subs) == 0 && tf.stop != nil {
		tf.stop()
		tf.stop = nil
	}
}

func (tf *trafficFeed) run(ctx context.Context) {
	tick := time.NewTicker(tf.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			tf.poll(ctx)
		}
	}
}

func (tf *trafficFeed) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, tf.interval*3)
	defer cancel()
	resp, err := tf.client.List(ctx, &klitev1.ListRequest{Kind: "Node"})
	if err != nil {
		return
	}
	seen := map[string]int64{}
	var events []trafficEvent
	now := time.Now().UnixMilli()
	for _, obj := range resp.GetObjects() {
		node := obj.GetNode()
		if node == nil || node.GetStatus().GetPhase() != klitev1.NodePhase_NODE_PHASE_READY {
			continue
		}
		idx := node.GetStatus().GetNodeIndex()
		if idx == 0 {
			continue
		}
		name, admin := node.GetMeta().GetName(), tf.adminURL(idx)
		nodeEvents := tf.pollNode(ctx, name, admin, now, seen)
		nodeEvents = append(nodeEvents, tf.pollRBAC(ctx, name, admin, now, seen)...)
		events = append(events, tf.attribute(ctx, idx, now, nodeEvents)...)
	}
	tf.mu.Lock()
	// Keys that vanished belong to removed instances, and dropping them
	// keeps the baseline map from growing forever.
	tf.prev = seen
	chans := make([]chan []byte, 0, len(tf.subs))
	for ch := range tf.subs {
		chans = append(chans, ch)
	}
	tf.mu.Unlock()
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		for _, ch := range chans {
			select {
			case ch <- b:
			default: // a stalled subscriber loses events, never blocks the poll
			}
		}
	}
}

// envoyClusters is the slice of /clusters?format=json the feed reads.
type envoyClusters struct {
	ClusterStatuses []struct {
		Name         string `json:"name"`
		HostStatuses []struct {
			Address struct {
				SocketAddress struct {
					Address   string `json:"address"`
					PortValue int    `json:"port_value"`
				} `json:"socket_address"`
			} `json:"address"`
			Stats []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"stats"`
		} `json:"host_statuses"`
	} `json:"cluster_statuses"`
}

func (tf *trafficFeed) pollNode(ctx context.Context, node, adminURL string, now int64, seen map[string]int64) []trafficEvent {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/clusters?format=json", nil)
	if err != nil {
		return nil
	}
	resp, err := tf.httpc.Do(req)
	if err != nil {
		return nil // a node whose admin is unreachable simply goes dark
	}
	defer resp.Body.Close()
	var body envoyClusters
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}

	tf.mu.Lock()
	prev := tf.prev
	prefix := node + "|"
	// Each source primes alone: a node whose /clusters answered first must
	// not treat its first sight of RBAC counters as movement, or the other
	// way round. That's why the baseline check ignores the other source's
	// keys.
	primed := false
	for k := range prev {
		if strings.HasPrefix(k, prefix) && !strings.Contains(k, "|#rbac|") {
			primed = true
			break
		}
	}
	var events []trafficEvent
	for _, c := range body.ClusterStatuses {
		// Names with a slash are the ingress DNAT side. The caller's Envoy
		// already counted those calls, so counting both would double them.
		if strings.Contains(c.Name, "/") {
			continue
		}
		for _, h := range c.HostStatuses {
			var total int64 = -1
			for _, st := range h.Stats {
				if st.Name == "rq_total" {
					if v, err := strconv.ParseInt(st.Value, 10, 64); err == nil {
						total = v
					}
				}
			}
			if total < 0 {
				continue
			}
			addr := h.Address.SocketAddress.Address
			port := h.Address.SocketAddress.PortValue
			key := prefix + c.Name + "|" + addr + ":" + strconv.Itoa(port)
			seen[key] = total
			// The first sight of a node's counters is the baseline. A drop
			// means Envoy restarted, so that's a fresh baseline too.
			if delta := total - prev[key]; primed && delta > 0 {
				events = append(events, trafficEvent{
					UnixMs: now, Node: node, Service: c.Name,
					Address: addr, Port: port, Count: int(delta), Verdict: "allowed",
				})
			}
		}
	}
	tf.mu.Unlock()
	return events
}

// rbacDeniedStat matches one denied counter of a per-listener RBAC filter.
// The xds builder gives each phase its own stat prefix, <service>_deny and
// <service>_allow, exactly so these counters never merge.
var rbacDeniedStat = regexp.MustCompile(`(?m)^(?:[a-z_]+\.)?([A-Za-z0-9.-]+)_(deny|allow)\.rbac\.denied: (\d+)$`)

// pollRBAC turns denied-counter movement into denied events. RBAC kills a
// connection before the upstream cluster sees it, so these calls exist
// nowhere in /clusters and earn their own poll.
func (tf *trafficFeed) pollRBAC(ctx context.Context, node, adminURL string, now int64, seen map[string]int64) []trafficEvent {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/stats?filter=rbac", nil)
	if err != nil {
		return nil
	}
	resp, err := tf.httpc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}

	tf.mu.Lock()
	defer tf.mu.Unlock()
	prev := tf.prev
	prefix := node + "|#rbac|"
	primed := false
	for k := range prev {
		if strings.HasPrefix(k, prefix) {
			primed = true
			break
		}
	}
	var events []trafficEvent
	for _, m := range rbacDeniedStat.FindAllStringSubmatch(string(body), -1) {
		service, phase := m[1], m[2]
		total, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil {
			continue
		}
		key := prefix + service + "|" + phase
		seen[key] = total
		if delta := total - prev[key]; primed && delta > 0 {
			events = append(events, trafficEvent{
				UnixMs: now, Node: node, Service: service,
				Count: int(delta), Verdict: "denied", Phase: phase,
			})
		}
	}
	return events
}

// attribute joins a node's counter deltas with its kdns ring. Every chatty
// call resolves its target name first, and kdns records the query's source
// IP, which is the caller instance. A delta whose service has a matching
// fresh query splits into single calls that carry their caller. Anything
// unmatched stays node-attributed. A donor without the RPC yet just answers
// errors, so the feed degrades to exactly the old behavior.
func (tf *trafficFeed) attribute(ctx context.Context, idx int32, now int64, events []trafficEvent) []trafficEvent {
	if len(events) == 0 {
		return events
	}
	queries := tf.recentQueries(ctx, idx, now-5_000)
	if len(queries) == 0 {
		return events
	}
	bySvc := map[string][]string{}
	for _, q := range queries {
		bySvc[q.GetService()] = append(bySvc[q.GetService()], q.GetSourceIp())
	}
	var out []trafficEvent
	for _, ev := range events {
		pool := bySvc[ev.Service]
		took := min(len(pool), ev.Count)
		for _, ip := range pool[:took] {
			single := ev
			single.Count = 1
			single.Caller = ip
			out = append(out, single)
		}
		bySvc[ev.Service] = pool[took:]
		if rest := ev.Count - took; rest > 0 {
			ev.Count = rest
			out = append(out, ev)
		}
	}
	return out
}

func (tf *trafficFeed) recentQueries(ctx context.Context, idx int32, sinceUnixMs int64) []*klitev1.RecentQuery {
	tf.mu.Lock()
	conn := tf.netConns[idx]
	tf.mu.Unlock()
	if conn == nil {
		fresh, err := grpc.NewClient(tf.netAddr(idx), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil
		}
		conn = fresh
		tf.mu.Lock()
		tf.netConns[idx] = conn
		tf.mu.Unlock()
	}
	resp, err := klitev1.NewKliteNetServiceClient(conn).RecentQueries(ctx, &klitev1.RecentQueriesRequest{SinceUnixMs: sinceUnixMs})
	if err != nil {
		return nil
	}
	return resp.GetQueries()
}

// handleTraffic serves the feed as SSE, one JSON event per data line.
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "response writer cannot stream")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": traffic open\n\n")
	fl.Flush()

	ch := s.traffic.subscribe()
	defer s.traffic.unsubscribe(ch)
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

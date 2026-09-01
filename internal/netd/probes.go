package netd

import (
	"cmp"
	"context"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const (
	probeInterval = 2 * time.Second
	probeTimeout  = time.Second
	// probeParallelism caps concurrent dials per sweep so an oversized
	// target list degrades into a slower sweep, not a goroutine flood.
	probeParallelism = 64
)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// prober TCP-connects to the current config's probe targets every interval
// and retains the latest result per instance.
type prober struct {
	cfg      *atomic.Pointer[netConfig]
	dial     dialFunc
	interval time.Duration
	timeout  time.Duration
	kick     chan struct{}

	mu    sync.Mutex
	state map[string]*klitev1.ProbeState
}

func newProber(cfg *atomic.Pointer[netConfig], dial dialFunc) *prober {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return &prober{
		cfg:      cfg,
		dial:     dial,
		interval: probeInterval,
		timeout:  probeTimeout,
		kick:     make(chan struct{}, 1),
		state:    map[string]*klitev1.ProbeState{},
	}
}

// kickNow schedules an immediate sweep without blocking (a pending kick is enough).
func (p *prober) kickNow() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *prober) run(ctx context.Context) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	p.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-p.kick:
		}
		p.sweep(ctx)
	}
}

// sweep probes every current target concurrently (capped) and replaces the
// retained state, dropping instances the config no longer lists.
func (p *prober) sweep(ctx context.Context) {
	targets := p.cfg.Load().targets
	results := make([]*klitev1.ProbeState, len(targets))
	var g errgroup.Group
	g.SetLimit(probeParallelism)
	for i, t := range targets {
		g.Go(func() error {
			results[i] = &klitev1.ProbeState{
				Instance: t.instance,
				Ip:       t.ip,
				Port:     t.port,
				Ready:    p.probe(ctx, t.addr),
			}
			return nil
		})
	}
	_ = g.Wait() // probes never return errors, only ready=false

	// A cancelled sweep saw its dials aborted, not its targets down. Keep
	// the last real results, or a Probes call racing shutdown sees
	// everything unready.
	if ctx.Err() != nil {
		return
	}
	next := make(map[string]*klitev1.ProbeState, len(results))
	for _, r := range results {
		next[r.GetInstance()] = r
	}
	p.mu.Lock()
	p.state = next
	p.mu.Unlock()
}

func (p *prober) probe(ctx context.Context, addr string) bool {
	dctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := p.dial(dctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (p *prober) snapshot() []*klitev1.ProbeState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*klitev1.ProbeState, 0, len(p.state))
	for _, s := range p.state {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *klitev1.ProbeState) int {
		return cmp.Compare(a.GetInstance(), b.GetInstance())
	})
	return out
}

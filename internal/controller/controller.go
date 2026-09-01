// Package controller holds the leader-only reconcile loops: workload fan-out,
// scheduling, and node health. Each loop is level-based, re-listing and
// converging on every store change and on a periodic resync.
package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

const (
	resyncPeriod = 2 * time.Second
	watchRetry   = time.Second
	casRetries   = 5
)

// RunAll runs every controller until ctx ends. klited calls it while holding
// leadership, so at most one set of controllers actuates at a time.
func RunAll(ctx context.Context, st store.Store) {
	loops := []struct {
		name  string
		kinds []string
		fn    func(context.Context) error
	}{
		{"workload", []string{object.KindWorkload, object.KindInstance, object.KindNode}, newWorkloadController(st).reconcile},
		{"scheduler", []string{object.KindInstance, object.KindNode, object.KindWorkload}, (&scheduler{st: st}).reconcile},
		{"node", []string{object.KindNode, object.KindInstance}, (&nodeController{st: st, now: time.Now}).reconcile},
		{"vip", []string{object.KindService, object.KindNode, object.KindVIPAllocation}, (&vipController{st: st}).reconcile},
	}
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLoop(ctx, st, l.name, l.kinds, l.fn)
		}()
	}
	wg.Wait()
}

// runLoop calls fn on store changes and every resyncPeriod. fn re-reads
// everything it acts on, so a missed event costs one tick, not correctness.
func runLoop(ctx context.Context, st store.Store, name string, kinds []string, fn func(context.Context) error) {
	kicks := make(chan struct{}, 1)
	go watchPump(ctx, st, kinds, kicks)
	ticker := time.NewTicker(resyncPeriod)
	defer ticker.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("reconcile failed", "controller", name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-kicks:
		case <-ticker.C:
		}
	}
}

// watchPump squeezes store events into non-blocking kicks, rebuilding the
// watch whenever it drops.
func watchPump(ctx context.Context, st store.Store, kinds []string, kicks chan<- struct{}) {
	for ctx.Err() == nil {
		events, err := st.Watch(ctx, kinds, 0)
		if err != nil {
			if !sleep(ctx, watchRetry) {
				return
			}
			continue
		}
		for ev := range events {
			if ev.Err != nil {
				break
			}
			select {
			case kicks <- struct{}{}:
			default:
			}
		}
		if !sleep(ctx, watchRetry) {
			return
		}
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

// Package agent runs one node. It registers with klited, watches the node's
// desired state, reconciles Docker against it, and reports status back.
// Snapshots come down a server-push stream (research/prior-art.md).
package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

const (
	resyncPeriod    = 2 * time.Second
	reportPeriod    = 5 * time.Second
	reportTimeout   = 4 * time.Second
	retryBackoffMax = 15 * time.Second
)

// Config wires an Agent.
type Config struct {
	Node    string
	Token   string
	Runtime runtime.Runtime
	Client  klitev1.AgentServiceClient
	// ServerAddrs are the klited endpoints this agent dials; they tell the
	// in-container Envoy where the xDS servers live (infrapod.go).
	ServerAddrs []string
	// StateDir overrides ~/.klite/agent as the root for per-node files.
	StateDir string
	// TLSDir holds the node's persisted identity (join.go); the infra pod
	// bind-mounts it into Envoy. Empty renders a plaintext xDS bootstrap,
	// which only unit tests should ever see.
	TLSDir string
	// CommandDial opens a connection pinned to one endpoint. The command
	// plane needs it: output pushes must reach the exact klited holding the
	// command stream, which Client's round-robin channel cannot promise.
	// nil (unit tests) runs the command plane over Client.
	CommandDial func(endpoint string) (*grpc.ClientConn, error)
}

// Agent is the per-node loop. All mutable state sits behind mu. The reconcile
// loop is the only writer of states, and the report loop reads them.
type Agent struct {
	node        string
	token       string
	rt          runtime.Runtime
	client      klitev1.AgentServiceClient
	serverAddrs []string
	stateDir    string
	tlsDir      string
	now         func() time.Time

	// lockedDonor and lockAttempt belong to the netLoop goroutine alone:
	// which donor container already got the admin-port lockdown, and when
	// the last failed attempt happened (lockdown.go).
	lockedDonor string
	lockAttempt time.Time

	// cmdDial and cmdIdx belong to the commandLoop goroutine alone: the
	// pinned-connection factory and the endpoint rotation cursor
	// (commands.go).
	cmdDial func(endpoint string) (*grpc.ClientConn, error)
	cmdIdx  int

	mu           sync.Mutex
	desired      map[string]*klitev1.Instance // by instance name
	haveSnapshot bool
	lastRev      int64
	states       map[string]*instState         // by instance name
	grace        map[string]int32              // instance name -> last known stop grace, for orphans
	net          *klitev1.NetBootstrap         // saved at Register, drives the infra pod (ADR 0008)
	desiredNet   *klitev1.NetDesired           // latest snapshot's net config
	appliedNet   *klitev1.NetDesired           // last config klite-net acked
	probeReady   map[string]bool               // instance name -> latest probe verdict
	netHealthy   bool                          // klite-net DNS answering
	commands     map[string]context.CancelFunc // running server commands by id (commands.go)

	cmdWG sync.WaitGroup // one entry per running command handler

	kickReconcile chan struct{}
	kickReport    chan struct{}
	kickNet       chan struct{}
}

// New returns an Agent ready to Run.
func New(cfg *Config) *Agent {
	return &Agent{
		node:          cfg.Node,
		token:         cfg.Token,
		rt:            cfg.Runtime,
		client:        cfg.Client,
		serverAddrs:   cfg.ServerAddrs,
		stateDir:      cfg.StateDir,
		tlsDir:        cfg.TLSDir,
		cmdDial:       cfg.CommandDial,
		now:           time.Now,
		desired:       map[string]*klitev1.Instance{},
		states:        map[string]*instState{},
		grace:         map[string]int32{},
		probeReady:    map[string]bool{},
		commands:      map[string]context.CancelFunc{},
		kickReconcile: make(chan struct{}, 1),
		kickReport:    make(chan struct{}, 1),
		kickNet:       make(chan struct{}, 1),
	}
}

// Run blocks until ctx ends. SIGTERM simply cancels ctx: containers keep
// running and the next agent run adopts them, which is the level-based
// contract.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.rt.EnsureNetwork(ctx); err != nil {
		return err
	}
	if err := a.register(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){a.watchLoop, a.eventLoop, a.reconcileLoop, a.reportLoop, a.commandLoop, a.netLoop} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}
	wg.Wait()
	a.cmdWG.Wait()
	return ctx.Err()
}

// register retries until the server admits this node. PermissionDenied means
// the Node object is missing from the store, so we keep retrying while the
// operator applies the node YAML.
func (a *Agent) register(ctx context.Context) error {
	backoff := time.Second
	for {
		resp, err := a.client.Register(ctx, &klitev1.RegisterRequest{Node: a.node, ClusterToken: a.token})
		if err == nil {
			a.mu.Lock()
			a.net = resp.GetNet()
			a.mu.Unlock()
			slog.Info("registered", "node", a.node,
				"index", resp.GetNet().GetNodeIndex(), "infraIP", resp.GetNet().GetKliteNetIp())
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("register failed, retrying", "err", err, "backoff", backoff)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, retryBackoffMax)
	}
}

// watchLoop keeps a WatchDesired stream open. Every message is a full
// snapshot, so reconnecting at the last applied revision loses nothing.
func (a *Agent) watchLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := a.now()
		err := a.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("desired-state stream broke, reconnecting", "err", err, "backoff", backoff)
		if code := status.Code(err); code == codes.Unauthenticated || code == codes.PermissionDenied {
			if a.register(ctx) != nil {
				return
			}
		}
		if !sleep(ctx, backoff) {
			return
		}
		if a.now().Sub(started) > time.Minute {
			backoff = time.Second
		} else {
			backoff = min(backoff*2, retryBackoffMax)
		}
	}
}

func (a *Agent) watchOnce(ctx context.Context) error {
	a.mu.Lock()
	rev := a.lastRev
	a.mu.Unlock()
	stream, err := a.client.WatchDesired(ctx, &klitev1.WatchDesiredRequest{Node: a.node, LastAppliedRevision: rev})
	if err != nil {
		return err
	}
	for {
		ds, err := stream.Recv()
		if err != nil {
			return err
		}
		a.applySnapshot(ds)
	}
}

// applySnapshot swaps in the server's full per-node snapshot and kicks a
// reconcile.
func (a *Agent) applySnapshot(ds *klitev1.DesiredState) {
	desired := make(map[string]*klitev1.Instance, len(ds.GetInstances()))
	a.mu.Lock()
	for _, inst := range ds.GetInstances() {
		name := inst.GetMeta().GetName()
		desired[name] = inst
		if g := inst.GetSpec().GetDrain().GetTerminationGraceSeconds(); g > 0 {
			a.grace[name] = g
		}
	}
	a.desired = desired
	a.haveSnapshot = true
	a.lastRev = ds.GetRevision()
	a.desiredNet = ds.GetNet()
	a.mu.Unlock()
	slog.Info("desired state applied", "revision", ds.GetRevision(), "instances", len(desired),
		"services", len(ds.GetNet().GetServices()))
	kick(a.kickReconcile)
	kick(a.kickNet)
}

// eventLoop turns Docker start/die events into reconcile kicks, resyncing
// after every (re)subscribe: the events-plus-resync informer pattern.
func (a *Agent) eventLoop(ctx context.Context) {
	for ctx.Err() == nil {
		ch, err := a.rt.WatchEvents(ctx, a.node)
		if err != nil {
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}
		kick(a.kickReconcile)
		for ev := range ch {
			slog.Debug("container event", "action", ev.Action, "instance", ev.InstanceName, "exitCode", ev.ExitCode)
			kick(a.kickReconcile)
		}
		if !sleep(ctx, time.Second) {
			return
		}
	}
}

// reconcileLoop is level-based: each kick or tick replays the whole desired
// snapshot against Docker. It waits for the first snapshot so a fresh agent
// never mistakes every container for an orphan.
func (a *Agent) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(resyncPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.kickReconcile:
		case <-ticker.C:
		}
		a.mu.Lock()
		ready := a.haveSnapshot
		a.mu.Unlock()
		if !ready {
			continue
		}
		if err := a.reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("reconcile failed", "err", err)
		}
	}
}

// reportLoop sends status every reportPeriod as the node heartbeat, plus
// immediately after any transition the reconciler flags.
func (a *Agent) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(reportPeriod)
	defer ticker.Stop()
	a.report(ctx) // first heartbeat, so the node turns Ready right away
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.kickReport:
		case <-ticker.C:
		}
		a.report(ctx)
	}
}

func (a *Agent) report(ctx context.Context) {
	req := &klitev1.ReportStatusRequest{Node: a.node, Instances: a.statusUpdates(), KliteNet: a.kliteNetStatus()}
	rctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	if _, err := a.client.ReportStatus(rctx, req); err != nil && ctx.Err() == nil {
		slog.Warn("status report failed", "err", err)
	}
}

func (a *Agent) statusUpdates() []*klitev1.InstanceStatusUpdate {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*klitev1.InstanceStatusUpdate, 0, len(a.states))
	for name, st := range a.states {
		if _, ok := a.desired[name]; !ok {
			continue
		}
		out = append(out, &klitev1.InstanceStatusUpdate{
			Name: name,
			Uid:  st.uid,
			Status: &klitev1.InstanceStatus{
				Phase:       st.phase,
				Restarts:    st.restarts,
				InstanceIp:  st.ip,
				ContainerId: st.containerID,
				Message:     st.message,
			},
		})
	}
	return out
}

func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
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

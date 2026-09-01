package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

func init() {
	// Every Drain test polls fast; init keeps it race-free across parallel tests.
	drainPollInterval = 5 * time.Millisecond
}

// drainStream collects DrainProgress messages on a channel.
type drainStream struct {
	grpc.ServerStream
	ctx  context.Context
	msgs chan *klitev1.DrainProgress
}

func newDrainStream(ctx context.Context) *drainStream {
	return &drainStream{ctx: ctx, msgs: make(chan *klitev1.DrainProgress, 64)}
}

func (s *drainStream) Send(m *klitev1.DrainProgress) error {
	s.msgs <- m
	return nil
}

func (s *drainStream) Context() context.Context { return s.ctx }

// recvContaining waits for a message containing want, failing on timeout.
// Unrelated lines in between are tolerated.
func (s *drainStream) recvContaining(t *testing.T, want string) *klitev1.DrainProgress {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-s.msgs:
			if strings.Contains(m.GetMessage(), want) {
				return m
			}
		case <-deadline:
			t.Fatalf("no drain message containing %q arrived", want)
		}
	}
}

func seedNode(t *testing.T, st store.Store, name string, phase klitev1.NodePhase) {
	t.Helper()
	obj := &klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
		Meta:   &klitev1.Meta{Name: name},
		Spec:   &klitev1.NodeSpec{MaxInstances: 8},
		Status: &klitev1.NodeStatus{Phase: phase, LastHeartbeatUnix: time.Now().Unix()},
	}}}
	if _, err := st.Put(context.Background(), obj, store.RevAny); err != nil {
		t.Fatal(err)
	}
}

func seedInstance(t *testing.T, st store.Store, name, workload, node string, phase klitev1.InstancePhase) {
	t.Helper()
	obj := &klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.InstanceSpec{
			Workload: workload, Node: node,
			Drain: &klitev1.DrainSpec{DrainTimeoutSeconds: 5, TerminationGraceSeconds: 5},
		},
		Status: &klitev1.InstanceStatus{Phase: phase},
	}}}
	if _, err := st.Put(context.Background(), obj, store.RevAny); err != nil {
		t.Fatal(err)
	}
}

func setInstance(t *testing.T, st store.Store, name string, mut func(*klitev1.Instance)) {
	t.Helper()
	obj, rev, err := st.Get(context.Background(), object.KindInstance, name)
	if err != nil {
		t.Fatal(err)
	}
	mut(obj.GetInstance())
	if _, err := st.Put(context.Background(), obj, rev); err != nil {
		t.Fatal(err)
	}
}

// TestDrainStreamsProgress walks the narrator through the full choreography,
// with the test playing the controller's part.
func TestDrainStreamsProgress(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	seedInstance(t, st, "b-old", "b", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newDrainStream(ctx)
	s := NewCluster(st, NewCommandHub(), nil)
	done := make(chan error, 1)
	go func() { done <- s.Drain(&klitev1.DrainRequest{Node: "node-2"}, stream) }()

	stream.recvContaining(t, "cordoned node-2")
	obj, _, err := st.Get(ctx, object.KindNode, "node-2")
	if err != nil {
		t.Fatal(err)
	}
	nst := obj.GetNode().GetStatus()
	if !nst.GetUnschedulable() || nst.GetPhase() != klitev1.NodePhase_NODE_PHASE_DRAINING {
		t.Fatalf("node status = %v, want cordoned and DRAINING", nst)
	}
	stream.recvContaining(t, "draining 1 instance(s) on node-2")

	seedInstance(t, st, "b-new", "b", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	stream.recvContaining(t, "surged b-new to node-1")

	setInstance(t, st, "b-old", func(inst *klitev1.Instance) {
		inst.Status.Phase = klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
	})
	stream.recvContaining(t, "draining b-old (5s)")

	if err := st.Delete(ctx, object.KindInstance, "b-old"); err != nil {
		t.Fatal(err)
	}
	stream.recvContaining(t, "drained b-old")
	final := stream.recvContaining(t, "done")
	if !final.GetDone() {
		t.Error("final message must set Done")
	}
	if err := <-done; err != nil {
		t.Fatalf("Drain returned %v", err)
	}
}

// With force, the server deletes draining instances itself instead of
// waiting out their timeout.
func TestDrainForceDeletesDrainingInstances(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	seedInstance(t, st, "b-old", "b", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newDrainStream(ctx)
	s := NewCluster(st, NewCommandHub(), nil)
	done := make(chan error, 1)
	go func() { done <- s.Drain(&klitev1.DrainRequest{Node: "node-2", Force: true}, stream) }()

	stream.recvContaining(t, "cordoned node-2")
	setInstance(t, st, "b-old", func(inst *klitev1.Instance) {
		inst.Status.Phase = klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
	})
	stream.recvContaining(t, "force-removed b-old")
	if _, _, err := st.Get(ctx, object.KindInstance, "b-old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want the instance force-deleted", err)
	}
	stream.recvContaining(t, "done")
	if err := <-done; err != nil {
		t.Fatalf("Drain returned %v", err)
	}
}

// The narrator flags the drain-first fallback when a surge sits unbound with
// the scheduler's no-capacity reason.
func TestDrainNarratesCapacityFallback(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	seedInstance(t, st, "b-old", "b", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newDrainStream(ctx)
	s := NewCluster(st, NewCommandHub(), nil)
	done := make(chan error, 1)
	go func() { done <- s.Drain(&klitev1.DrainRequest{Node: "node-2"}, stream) }()
	defer func() {
		cancel()
		<-done
	}()

	stream.recvContaining(t, "cordoned node-2")
	seedInstance(t, st, "b-new", "b", "", klitev1.InstancePhase_INSTANCE_PHASE_PENDING)
	setInstance(t, st, "b-new", func(inst *klitev1.Instance) {
		inst.Status.Message = "no ready schedulable node with free capacity"
	})
	stream.recvContaining(t, "falling back to drain-first")
}

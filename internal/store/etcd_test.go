//go:build integration

package store_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
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

func newStore(t *testing.T) *store.Etcd {
	t.Helper()
	return store.NewEtcd(newClient(t))
}

func testName(t *testing.T) string {
	t.Helper()
	return "it-" + uuid.NewString()[:13]
}

func testWorkload(name string, replicas int32) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.WorkloadSpec{
			Replicas: replicas,
			Template: &klitev1.Template{Containers: []*klitev1.Container{{Name: "web", Image: "img"}}},
		},
	}}}
}

func mustCreate(t *testing.T, s *store.Etcd, obj *klitev1.Object) int64 {
	t.Helper()
	kind, name := object.KindOf(obj), object.MetaOf(obj).GetName()
	rev, err := s.Put(context.Background(), obj, store.RevCreate)
	if err != nil {
		t.Fatalf("create %s/%s: %v", kind, name, err)
	}
	t.Cleanup(func() { _ = s.Delete(context.Background(), kind, name) })
	return rev
}

func TestCreateAndGet(t *testing.T) {
	s := newStore(t)
	name := testName(t)
	rev := mustCreate(t, s, testWorkload(name, 2))
	if rev <= 0 {
		t.Fatalf("create returned revision %d", rev)
	}

	got, gotRev, err := s.Get(context.Background(), "workloads", name)
	if err != nil {
		t.Fatal(err)
	}
	if gotRev != rev {
		t.Errorf("Get revision = %d, want %d", gotRev, rev)
	}
	meta := object.MetaOf(got)
	if meta.GetUid() == "" {
		t.Error("create left uid empty")
	}
	if meta.GetCreatedUnix() == 0 {
		t.Error("create left created_unix zero")
	}
	if meta.GetResourceVersion() != rev {
		t.Errorf("resource_version = %d, want %d", meta.GetResourceVersion(), rev)
	}
	if got.GetWorkload().GetSpec().GetReplicas() != 2 {
		t.Errorf("replicas = %d, want 2", got.GetWorkload().GetSpec().GetReplicas())
	}
}

func TestCreateOnlyConflict(t *testing.T) {
	s := newStore(t)
	name := testName(t)
	mustCreate(t, s, testWorkload(name, 1))
	if _, err := s.Put(context.Background(), testWorkload(name, 3), store.RevCreate); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrAlreadyExists", err)
	}
}

func TestCompareAndSwap(t *testing.T) {
	s := newStore(t)
	name := testName(t)
	rev := mustCreate(t, s, testWorkload(name, 1))

	obj, _, err := s.Get(context.Background(), "Workload", name)
	if err != nil {
		t.Fatal(err)
	}
	obj.GetWorkload().Spec.Replicas = 5
	newRev, err := s.Put(context.Background(), obj, rev)
	if err != nil {
		t.Fatal(err)
	}
	if newRev <= rev {
		t.Errorf("newRev = %d, want > %d", newRev, rev)
	}

	// The revision we hold is now stale, so the same write must fail.
	if _, err := s.Put(context.Background(), obj, rev); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale put err = %v, want ErrConflict", err)
	}
}

func TestBlindUpsertPreservesIdentity(t *testing.T) {
	s := newStore(t)
	name := testName(t)
	mustCreate(t, s, testWorkload(name, 1))
	created, _, err := s.Get(context.Background(), "workloads", name)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Put(context.Background(), testWorkload(name, 7), store.RevAny); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(context.Background(), "workloads", name)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetWorkload().GetSpec().GetReplicas() != 7 {
		t.Errorf("replicas = %d, want 7", got.GetWorkload().GetSpec().GetReplicas())
	}
	if object.MetaOf(got).GetUid() != object.MetaOf(created).GetUid() {
		t.Errorf("upsert changed uid: %s then %s", object.MetaOf(created).GetUid(), object.MetaOf(got).GetUid())
	}
}

func TestDeleteAndNotFound(t *testing.T) {
	s := newStore(t)
	name := testName(t)
	mustCreate(t, s, testWorkload(name, 1))

	if err := s.Delete(context.Background(), "workloads", name); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(context.Background(), "workloads", name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "workloads", name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newStore(t)
	names := map[string]bool{testName(t): true, testName(t): true}
	for n := range names {
		mustCreate(t, s, testWorkload(n, 1))
	}

	objs, rev, err := s.List(context.Background(), "workloads")
	if err != nil {
		t.Fatal(err)
	}
	if rev <= 0 {
		t.Errorf("list revision = %d", rev)
	}
	found := 0
	for _, o := range objs {
		if names[object.MetaOf(o).GetName()] {
			found++
			if object.MetaOf(o).GetResourceVersion() == 0 {
				t.Error("list left resource_version zero")
			}
		}
	}
	if found != len(names) {
		t.Errorf("found %d of %d created workloads", found, len(names))
	}
}

func recvFor(t *testing.T, ch <-chan store.Event, name string) store.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("watch channel closed")
			}
			if ev.Err != nil {
				t.Fatalf("watch error: %v", ev.Err)
			}
			if object.MetaOf(ev.Object).GetName() == name {
				return ev
			}
		case <-deadline:
			t.Fatalf("no event for %s within 5s", name)
		}
	}
}

func TestWatchLifecycle(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	name := testName(t)

	ch, err := s.Watch(ctx, []string{"workloads"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	createRev := mustCreate(t, s, testWorkload(name, 1))
	ev := recvFor(t, ch, name)
	if ev.Type != klitev1.EventType_EVENT_TYPE_ADDED || ev.Revision != createRev {
		t.Errorf("event = %v rev=%d, want ADDED rev=%d", ev.Type, ev.Revision, createRev)
	}

	obj, rev, err := s.Get(ctx, "workloads", name)
	if err != nil {
		t.Fatal(err)
	}
	obj.GetWorkload().Spec.Replicas = 4
	if _, err := s.Put(ctx, obj, rev); err != nil {
		t.Fatal(err)
	}
	ev = recvFor(t, ch, name)
	if ev.Type != klitev1.EventType_EVENT_TYPE_MODIFIED {
		t.Errorf("event = %v, want MODIFIED", ev.Type)
	}
	if ev.Object.GetWorkload().GetSpec().GetReplicas() != 4 {
		t.Errorf("event replicas = %d, want 4", ev.Object.GetWorkload().GetSpec().GetReplicas())
	}

	if err := s.Delete(ctx, "workloads", name); err != nil {
		t.Fatal(err)
	}
	ev = recvFor(t, ch, name)
	if ev.Type != klitev1.EventType_EVENT_TYPE_DELETED {
		t.Errorf("event = %v, want DELETED", ev.Type)
	}
	if ev.Object.GetWorkload().GetSpec().GetReplicas() != 4 {
		t.Errorf("deleted event lost the prior value, replicas = %d", ev.Object.GetWorkload().GetSpec().GetReplicas())
	}
}

func TestWatchKindFilterAndReplay(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wlName, svcName := testName(t), testName(t)
	svc := &klitev1.Object{Kind: &klitev1.Object_Service{Service: &klitev1.Service{
		Meta: &klitev1.Meta{Name: svcName},
		Spec: &klitev1.ServiceSpec{Port: 8080, TargetPort: 80},
	}}}

	ch, err := s.Watch(ctx, []string{"workloads"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, svc)
	wlRev := mustCreate(t, s, testWorkload(wlName, 1))

	// The service write lands first, so the workload event arriving alone proves the filter.
	ev := recvFor(t, ch, wlName)
	if kind := object.KindOf(ev.Object); kind != object.KindWorkload {
		t.Errorf("filtered watch delivered a %s", kind)
	}

	// A second watch starting at the create revision replays the same event.
	replay, err := s.Watch(ctx, nil, wlRev)
	if err != nil {
		t.Fatal(err)
	}
	ev = recvFor(t, replay, wlName)
	if ev.Type != klitev1.EventType_EVENT_TYPE_ADDED || ev.Revision != wlRev {
		t.Errorf("replayed event = %v rev=%d, want ADDED rev=%d", ev.Type, ev.Revision, wlRev)
	}
}

// Concurrent CAS writers race from the same revision and exactly one wins.
func TestCompareAndSwapConcurrent(t *testing.T) {
	s := newStore(t)
	name := testName(t)
	rev := mustCreate(t, s, testWorkload(name, 1))

	const writers = 8
	errs := make(chan error, writers)
	for i := range writers {
		go func() {
			_, err := s.Put(context.Background(), testWorkload(name, int32(i+2)), rev)
			errs <- err
		}()
	}
	var wins, conflicts int
	for range writers {
		switch err := <-errs; {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrConflict):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != writers-1 {
		t.Errorf("wins = %d, conflicts = %d, want 1 and %d", wins, conflicts, writers-1)
	}
}

// One corrupt value fails the whole List, and the error names the key.
func TestListSurfacesCorruptValue(t *testing.T) {
	cli := newClient(t)
	s := store.NewEtcd(cli)
	name := testName(t)
	key := "/klite/v1/workloads/" + name
	if _, err := cli.Put(context.Background(), key, "{corrupt"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cli.Delete(context.Background(), key) })

	if _, _, err := s.List(context.Background(), "workloads"); err == nil || !strings.Contains(err.Error(), key) {
		t.Errorf("List err = %v, want the corrupt key named", err)
	}
	if _, _, err := s.Get(context.Background(), "workloads", name); err == nil || !strings.Contains(err.Error(), key) {
		t.Errorf("Get err = %v, want the corrupt key named", err)
	}
}

// A watch from a compacted revision surfaces the error and then closes the channel.
func TestWatchSurfacesCompaction(t *testing.T) {
	cli := newClient(t)
	s := store.NewEtcd(cli)
	name := testName(t)
	firstRev := mustCreate(t, s, testWorkload(name, 1))
	var lastRev int64
	for i := range int32(5) {
		obj, rev, err := s.Get(context.Background(), "workloads", name)
		if err != nil {
			t.Fatal(err)
		}
		obj.GetWorkload().Spec.Replicas = i + 2
		if lastRev, err = s.Put(context.Background(), obj, rev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cli.Compact(context.Background(), lastRev, clientv3.WithCompactPhysical()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := s.Watch(ctx, []string{"workloads"}, firstRev)
	if err != nil {
		t.Fatal(err)
	}
	sawErr := false
	for ev := range ch {
		if ev.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("watch from compacted revision closed without an Err event")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Watch(ctx, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch channel not closed within 5s of cancel")
		}
	}
}

func TestPutRejectsBadInput(t *testing.T) {
	s := newStore(t)
	tests := []struct {
		name string
		obj  *klitev1.Object
		rev  int64
	}{
		{"empty envelope", &klitev1.Object{}, store.RevAny},
		{"empty name", testWorkload("", 1), store.RevAny},
		{"slash in name", testWorkload("a/b", 1), store.RevAny},
		{"negative revision", testWorkload(testName(t), 1), -2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Put(context.Background(), tt.obj, tt.rev); err == nil {
				t.Fatal("Put accepted bad input")
			}
		})
	}
}

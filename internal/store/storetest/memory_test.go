// These tests pin Memory to the etcd store's observable semantics: the CAS
// sentinels, identity stamping, clone-on-boundary reads, and name rules. A
// drift here silently weakens every controller test built on the double.
package storetest_test

import (
	"context"
	"errors"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

func workload(name string) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.WorkloadSpec{Replicas: 1},
	}}}
}

func TestPutSentinels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := storetest.New()

	rev, err := m.Put(ctx, workload("b"), store.RevCreate)
	if err != nil || rev == 0 {
		t.Fatalf("create = %d, %v", rev, err)
	}
	if _, err := m.Put(ctx, workload("b"), store.RevCreate); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrAlreadyExists", err)
	}
	if _, err := m.Put(ctx, workload("b"), rev+100); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale CAS err = %v, want ErrConflict", err)
	}
	if _, err := m.Put(ctx, workload("missing"), 7); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CAS on a missing object err = %v, want ErrConflict", err)
	}
	rev2, err := m.Put(ctx, workload("b"), rev)
	if err != nil || rev2 <= rev {
		t.Fatalf("CAS = %d, %v, want a later revision", rev2, err)
	}
	if _, err := m.Put(ctx, workload("b"), -7); err == nil {
		t.Fatal("negative non-sentinel expectedRev must be rejected")
	}
}

// A blind upsert with empty uid and created time must keep the stored
// identity, the way the etcd store re-reads and adopts.
func TestUpsertAdoptsIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := storetest.New()
	if _, err := m.Put(ctx, workload("b"), store.RevCreate); err != nil {
		t.Fatal(err)
	}
	first, _, err := m.Get(ctx, "Workload", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, workload("b"), store.RevAny); err != nil {
		t.Fatal(err)
	}
	second, _, err := m.Get(ctx, "Workload", "b")
	if err != nil {
		t.Fatal(err)
	}
	fm, sm := object.MetaOf(first), object.MetaOf(second)
	if sm.GetUid() == "" || sm.GetUid() != fm.GetUid() {
		t.Errorf("uid = %q, want the original %q", sm.GetUid(), fm.GetUid())
	}
	if sm.GetCreatedUnix() != fm.GetCreatedUnix() {
		t.Errorf("created = %d, want the original %d", sm.GetCreatedUnix(), fm.GetCreatedUnix())
	}
}

// Mutating a returned object must not touch stored state, and
// resource_version is derived on read, never persisted.
func TestReadsCloneAndCarryRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := storetest.New()
	if _, err := m.Put(ctx, workload("b"), store.RevCreate); err != nil {
		t.Fatal(err)
	}
	got, rev, err := m.Get(ctx, "Workload", "b")
	if err != nil {
		t.Fatal(err)
	}
	if rev == 0 || object.MetaOf(got).GetResourceVersion() != rev {
		t.Fatalf("resource_version = %d, want the returned revision %d", object.MetaOf(got).GetResourceVersion(), rev)
	}
	got.GetWorkload().Spec.Replicas = 99
	again, _, err := m.Get(ctx, "Workload", "b")
	if err != nil {
		t.Fatal(err)
	}
	if again.GetWorkload().GetSpec().GetReplicas() != 1 {
		t.Fatal("mutating a read object leaked into the store")
	}
}

func TestDeleteSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := storetest.New()
	if _, err := m.Put(ctx, workload("b"), store.RevCreate); err != nil {
		t.Fatal(err)
	}
	_, before, err := m.List(ctx, "Workload")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "Workload", "b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "Workload", "b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
	if _, after, err := m.List(ctx, "Workload"); err != nil || after <= before {
		t.Fatalf("list revision = %d, %v, want a bump past %d after delete", after, err, before)
	}
}

// DeleteIfRevision must remove exactly the object the caller read. A write
// in between turns the delete into ErrConflict and absence into ErrNotFound,
// while the Put sentinels are rejected rather than reinterpreted.
func TestDeleteIfRevisionSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := storetest.New()
	rev, err := m.Put(ctx, workload("b"), store.RevCreate)
	if err != nil {
		t.Fatal(err)
	}

	moved, err := m.Put(ctx, workload("b"), rev)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteIfRevision(ctx, "Workload", "b", rev); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale delete err = %v, want ErrConflict", err)
	}
	if _, _, err := m.Get(ctx, "Workload", "b"); err != nil {
		t.Fatalf("the object must survive a stale delete: %v", err)
	}

	if err := m.DeleteIfRevision(ctx, "Workload", "b", moved); err != nil {
		t.Fatalf("current-revision delete err = %v", err)
	}
	if err := m.DeleteIfRevision(ctx, "Workload", "b", moved); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete of a gone object err = %v, want ErrNotFound", err)
	}

	for _, bad := range []int64{store.RevCreate, store.RevAny, -7} {
		err := m.DeleteIfRevision(ctx, "Workload", "b", bad)
		if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			t.Errorf("DeleteIfRevision(%d) err = %v, want a plain rejection", bad, err)
		}
	}
	if err := m.DeleteIfRevision(ctx, "Workload", "a/b", 1); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("invalid name err = %v, want the key-rule rejection", err)
	}
}

// TestInvalidNamesRejected mirrors the etcd key rules: empty names and names
// carrying a slash never reach storage.
func TestInvalidNamesRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := storetest.New()
	for _, name := range []string{"", "a/b"} {
		if _, err := m.Put(ctx, workload(name), store.RevAny); err == nil {
			t.Errorf("Put(%q) succeeded, want an invalid-name error", name)
		}
		if _, _, err := m.Get(ctx, "Workload", name); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get(%q) err = %v, want an invalid-name error", name, err)
		}
		if err := m.Delete(ctx, "Workload", name); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete(%q) err = %v, want an invalid-name error", name, err)
		}
	}
}

func TestWatchClosesWithContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	m := storetest.New()
	events, err := m.Watch(ctx, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, open := <-events; open {
		t.Fatal("watch channel must close once ctx ends")
	}
}

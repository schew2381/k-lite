package store

import (
	"context"
	"strings"
	"testing"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func TestKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kind    string
		obj     string
		want    string
		wantErr bool
	}{
		{"canonical kind", "Workload", "b", "/klite/v1/workloads/b", false},
		{"plural alias", "workloads", "b", "/klite/v1/workloads/b", false},
		{"lowercase alias", "networkpolicy", "p", "/klite/v1/networkpolicies/p", false},
		{"unknown kind", "Pod", "b", "", true},
		{"empty name", "Workload", "", "", true},
		{"slash in name", "Workload", "a/b", "", true},
		{"traversal name", "Workload", "../../etc", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := key(tt.kind, tt.obj)
			if (err != nil) != tt.wantErr {
				t.Fatalf("key(%q, %q) err = %v, wantErr %t", tt.kind, tt.obj, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("key(%q, %q) = %q, want %q", tt.kind, tt.obj, got, tt.want)
			}
		})
	}
}

func marshalWorkload(t *testing.T, name string, replicas int32) []byte {
	t.Helper()
	val, err := protojson.Marshal(&klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.WorkloadSpec{Replicas: replicas},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return val
}

func TestDecode(t *testing.T) {
	t.Parallel()
	obj, err := decode("/klite/v1/workloads/b", marshalWorkload(t, "b", 2), 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := object.MetaOf(obj).GetResourceVersion(); got != 42 {
		t.Errorf("resource_version = %d, want 42", got)
	}
	if got := obj.GetWorkload().GetSpec().GetReplicas(); got != 2 {
		t.Errorf("replicas = %d, want 2", got)
	}

	_, err = decode("/klite/v1/workloads/corrupt", []byte("{not json"), 7)
	if err == nil || !strings.Contains(err.Error(), "/klite/v1/workloads/corrupt") {
		t.Errorf("corrupt decode err = %v, want the key named", err)
	}
}

func putEvent(key string, val []byte, createRev, modRev int64) *clientv3.Event {
	return &clientv3.Event{
		Type: mvccpb.PUT,
		Kv:   &mvccpb.KeyValue{Key: []byte(key), Value: val, CreateRevision: createRev, ModRevision: modRev},
	}
}

func deleteEvent(key string, prev *mvccpb.KeyValue, modRev int64) *clientv3.Event {
	return &clientv3.Event{
		Type:   mvccpb.DELETE,
		Kv:     &mvccpb.KeyValue{Key: []byte(key), ModRevision: modRev},
		PrevKv: prev,
	}
}

func TestToEventPut(t *testing.T) {
	t.Parallel()
	val := marshalWorkload(t, "b", 2)

	e, ok := toEvent(putEvent("/klite/v1/workloads/b", val, 5, 5), nil)
	if !ok || e.Err != nil {
		t.Fatalf("toEvent = %+v, %t", e, ok)
	}
	if e.Type != klitev1.EventType_EVENT_TYPE_ADDED || e.Revision != 5 {
		t.Errorf("create: got %v rev=%d, want ADDED rev=5", e.Type, e.Revision)
	}
	if object.MetaOf(e.Object).GetResourceVersion() != 5 {
		t.Error("resource_version not filled from mod revision")
	}

	e, ok = toEvent(putEvent("/klite/v1/workloads/b", val, 5, 9), nil)
	if !ok || e.Type != klitev1.EventType_EVENT_TYPE_MODIFIED || e.Revision != 9 {
		t.Errorf("update: got %+v, %t, want MODIFIED rev=9", e, ok)
	}
}

func TestToEventCorruptValue(t *testing.T) {
	t.Parallel()
	e, ok := toEvent(putEvent("/klite/v1/workloads/bad", []byte("{"), 5, 5), nil)
	if !ok || e.Err == nil || !strings.Contains(e.Err.Error(), "/klite/v1/workloads/bad") {
		t.Errorf("got %+v, %t, want an Err event naming the key", e, ok)
	}
}

func TestToEventDeleteCarriesPriorValue(t *testing.T) {
	t.Parallel()
	val := marshalWorkload(t, "b", 2)
	prev := &mvccpb.KeyValue{Key: []byte("/klite/v1/workloads/b"), Value: val, ModRevision: 9}
	e, ok := toEvent(deleteEvent("/klite/v1/workloads/b", prev, 12), nil)
	if !ok || e.Err != nil || e.Type != klitev1.EventType_EVENT_TYPE_DELETED {
		t.Fatalf("got %+v, %t", e, ok)
	}
	if e.Object.GetWorkload().GetSpec().GetReplicas() != 2 {
		t.Error("delete event lost the prior spec")
	}
	if e.Revision != 12 {
		t.Errorf("revision = %d, want the deletion revision 12", e.Revision)
	}
}

func TestToEventDeleteAfterCompaction(t *testing.T) {
	t.Parallel()
	e, ok := toEvent(deleteEvent("/klite/v1/workloads/b", nil, 12), nil)
	if !ok || e.Err != nil || e.Type != klitev1.EventType_EVENT_TYPE_DELETED {
		t.Fatalf("got %+v, %t", e, ok)
	}
	if object.KindOf(e.Object) != object.KindWorkload {
		t.Errorf("kind = %s, want Workload", object.KindOf(e.Object))
	}
	if object.MetaOf(e.Object).GetName() != "b" {
		t.Errorf("name = %q, want b", object.MetaOf(e.Object).GetName())
	}
}

func TestToEventDrops(t *testing.T) {
	t.Parallel()
	val := marshalWorkload(t, "b", 2)
	tests := []struct {
		name string
		ev   *clientv3.Event
		segs map[string]bool
	}{
		{"filtered kind", putEvent("/klite/v1/workloads/b", val, 5, 5), map[string]bool{"services": true}},
		{"foreign prefix", putEvent("/other/v1/workloads/b", val, 5, 5), nil},
		{"key without name", putEvent("/klite/v1/workloads", val, 5, 5), nil},
		{"unknown segment delete", deleteEvent("/klite/v1/widgets/b", nil, 12), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if e, ok := toEvent(tt.ev, tt.segs); ok {
				t.Errorf("delivered %+v, want dropped", e)
			}
		})
	}
}

func TestSendRespectsContext(t *testing.T) {
	t.Parallel()
	ch := make(chan Event, 1)
	if !send(t.Context(), ch, Event{Revision: 1}) {
		t.Error("send to buffered channel failed")
	}

	full := make(chan Event) // no reader, so only ctx can release send
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if send(ctx, full, Event{Revision: 2}) {
		t.Error("send reported success on canceled ctx")
	}
}

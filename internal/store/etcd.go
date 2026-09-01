package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

// The trailing slash keeps one kind's prefix from matching another's (research/etcd.md).
const prefix = "/klite/v1/"

// Etcd is the Store backed by an etcd cluster, keyed /klite/v1/<kind>/<name> with protojson values.
type Etcd struct {
	cli *clientv3.Client
}

func NewEtcd(cli *clientv3.Client) *Etcd {
	return &Etcd{cli: cli}
}

func key(kind, name string) (string, error) {
	canonical, err := object.Canonical(kind)
	if err != nil {
		return "", err
	}
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid object name %q", name)
	}
	return prefix + object.Plural(canonical) + "/" + name, nil
}

func decode(key string, val []byte, modRev int64) (*klitev1.Object, error) {
	obj := &klitev1.Object{}
	if err := protojson.Unmarshal(val, obj); err != nil {
		// Name the key: one corrupt value fails a whole List, and the
		// operator fixing it needs to know which one to inspect.
		return nil, fmt.Errorf("decode stored object %s: %w", key, err)
	}
	object.MetaOf(obj).ResourceVersion = modRev
	return obj, nil
}

func (s *Etcd) Get(ctx context.Context, kind, name string) (*klitev1.Object, int64, error) {
	k, err := key(kind, name)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.cli.Get(ctx, k)
	if err != nil {
		return nil, 0, fmt.Errorf("get %s: %w", k, err)
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, fmt.Errorf("%s %q: %w", kind, name, ErrNotFound)
	}
	kv := resp.Kvs[0]
	obj, err := decode(k, kv.Value, kv.ModRevision)
	if err != nil {
		return nil, 0, err
	}
	return obj, kv.ModRevision, nil
}

func (s *Etcd) List(ctx context.Context, kind string) ([]*klitev1.Object, int64, error) {
	canonical, err := object.Canonical(kind)
	if err != nil {
		return nil, 0, err
	}
	pfx := prefix + object.Plural(canonical) + "/"
	resp, err := s.cli.Get(ctx, pfx, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, fmt.Errorf("list %s: %w", pfx, err)
	}
	objs := make([]*klitev1.Object, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		obj, err := decode(string(kv.Key), kv.Value, kv.ModRevision)
		if err != nil {
			return nil, 0, err
		}
		objs = append(objs, obj)
	}
	return objs, resp.Header.Revision, nil
}

func (s *Etcd) Put(ctx context.Context, obj *klitev1.Object, expectedRev int64) (int64, error) {
	kind := object.KindOf(obj)
	if kind == "" {
		return 0, fmt.Errorf("empty object envelope")
	}
	stored := proto.CloneOf(obj)
	meta := object.MetaOf(stored)
	k, err := key(kind, meta.GetName())
	if err != nil {
		return 0, err
	}
	// resource_version is derived from etcd on every read, never persisted.
	meta.ResourceVersion = 0

	switch {
	case expectedRev == RevCreate:
		return s.putCreate(ctx, kind, k, meta, stored)
	case expectedRev == RevAny:
		return s.putUpsert(ctx, kind, k, meta, stored)
	case expectedRev > 0:
		return s.putCAS(ctx, kind, k, meta, stored, expectedRev)
	}
	return 0, fmt.Errorf("invalid expectedRev %d", expectedRev)
}

func (s *Etcd) putCreate(ctx context.Context, kind, k string, meta *klitev1.Meta, stored *klitev1.Object) (int64, error) {
	stampNew(meta)
	val, err := protojson.Marshal(stored)
	if err != nil {
		return 0, err
	}
	resp, err := s.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(k), "=", 0)).
		Then(clientv3.OpPut(k, string(val))).
		Commit()
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", k, err)
	}
	if !resp.Succeeded {
		return 0, fmt.Errorf("%s %q: %w", kind, meta.GetName(), ErrAlreadyExists)
	}
	return resp.Header.Revision, nil
}

func (s *Etcd) putUpsert(ctx context.Context, kind, k string, meta *klitev1.Meta, stored *klitev1.Object) (int64, error) {
	cur, _, err := s.Get(ctx, kind, meta.GetName())
	switch {
	case err == nil:
		curMeta := object.MetaOf(cur)
		if meta.Uid == "" {
			meta.Uid = curMeta.GetUid()
		}
		if meta.CreatedUnix == 0 {
			meta.CreatedUnix = curMeta.GetCreatedUnix()
		}
	case errors.Is(err, ErrNotFound):
		stampNew(meta)
	default:
		return 0, err
	}
	val, err := protojson.Marshal(stored)
	if err != nil {
		return 0, err
	}
	resp, err := s.cli.Put(ctx, k, string(val))
	if err != nil {
		return 0, fmt.Errorf("put %s: %w", k, err)
	}
	return resp.Header.Revision, nil
}

func (s *Etcd) putCAS(ctx context.Context, kind, k string, meta *klitev1.Meta, stored *klitev1.Object, expectedRev int64) (int64, error) {
	val, err := protojson.Marshal(stored)
	if err != nil {
		return 0, err
	}
	resp, err := s.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(k), "=", expectedRev)).
		Then(clientv3.OpPut(k, string(val))).
		Commit()
	if err != nil {
		return 0, fmt.Errorf("put %s: %w", k, err)
	}
	if !resp.Succeeded {
		return 0, fmt.Errorf("%s %q at revision %d: %w", kind, meta.GetName(), expectedRev, ErrConflict)
	}
	return resp.Header.Revision, nil
}

func stampNew(meta *klitev1.Meta) {
	if meta.Uid == "" {
		meta.Uid = uuid.NewString()
	}
	if meta.CreatedUnix == 0 {
		meta.CreatedUnix = time.Now().Unix()
	}
}

func (s *Etcd) Delete(ctx context.Context, kind, name string) error {
	k, err := key(kind, name)
	if err != nil {
		return err
	}
	resp, err := s.cli.Delete(ctx, k)
	if err != nil {
		return fmt.Errorf("delete %s: %w", k, err)
	}
	if resp.Deleted == 0 {
		return fmt.Errorf("%s %q: %w", kind, name, ErrNotFound)
	}
	return nil
}

// Watch streams changes for the given kinds (empty means all), starting at
// fromRev inclusive, or at the next write when fromRev is 0. Resuming from a
// List revision redelivers the write at that revision. The channel closes
// when ctx ends or after an Err event. On compaction the caller recovers by
// re-listing and re-watching.
//
// Delivery applies backpressure: a consumer that stops reading without
// canceling ctx leaves events buffering unboundedly in the etcd client.
// Drain the channel or cancel.
func (s *Etcd) Watch(ctx context.Context, kinds []string, fromRev int64) (<-chan Event, error) {
	segs := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		canonical, err := object.Canonical(k)
		if err != nil {
			return nil, err
		}
		segs[object.Plural(canonical)] = true
	}
	opts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithPrevKV()}
	if fromRev > 0 {
		opts = append(opts, clientv3.WithRev(fromRev))
	}
	wch := s.cli.Watch(ctx, prefix, opts...)
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		for resp := range wch {
			if err := resp.Err(); err != nil {
				send(ctx, out, Event{Err: err})
				return
			}
			for _, ev := range resp.Events {
				e, ok := toEvent(ev, segs)
				if !ok {
					continue
				}
				if !send(ctx, out, e) || e.Err != nil {
					return
				}
			}
		}
	}()
	return out, nil
}

func send(ctx context.Context, ch chan<- Event, e Event) bool {
	select {
	case ch <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

func toEvent(ev *clientv3.Event, segs map[string]bool) (Event, bool) {
	key := string(ev.Kv.Key)
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return Event{}, false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return Event{}, false
	}
	seg, name := parts[0], parts[1]
	if len(segs) > 0 && !segs[seg] {
		return Event{}, false
	}
	rev := ev.Kv.ModRevision

	switch ev.Type {
	case mvccpb.PUT:
		obj, err := decode(key, ev.Kv.Value, rev)
		if err != nil {
			return Event{Err: err}, true
		}
		t := klitev1.EventType_EVENT_TYPE_MODIFIED
		if ev.IsCreate() {
			t = klitev1.EventType_EVENT_TYPE_ADDED
		}
		return Event{Type: t, Object: obj, Revision: rev}, true

	case mvccpb.DELETE:
		var obj *klitev1.Object
		if ev.PrevKv != nil {
			decoded, err := decode(key, ev.PrevKv.Value, rev)
			if err != nil {
				return Event{Err: err}, true
			}
			obj = decoded
		} else {
			// The prior value got compacted away, so identity is all the event can carry.
			canonical, err := object.Canonical(seg)
			if err != nil {
				return Event{}, false
			}
			obj, _ = object.New(canonical)
			object.MetaOf(obj).Name = name
			object.MetaOf(obj).ResourceVersion = rev
		}
		return Event{Type: klitev1.EventType_EVENT_TYPE_DELETED, Object: obj, Revision: rev}, true
	}
	return Event{}, false
}

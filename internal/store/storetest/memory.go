// Package storetest provides an in-memory store.Store for unit tests,
// mirroring the etcd implementation's CAS and stamping semantics.
package storetest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

type entry struct {
	obj *klitev1.Object
	rev int64
}

// Memory is a Store over maps: one global revision counter, clones on every
// boundary, no watch delivery (Watch blocks until ctx ends).
type Memory struct {
	mu   sync.Mutex
	rev  int64
	uid  int64
	objs map[string]map[string]*entry // kind -> name -> entry
}

func New() *Memory {
	return &Memory{objs: map[string]map[string]*entry{}}
}

func (m *Memory) bucket(kind string) (map[string]*entry, error) {
	canonical, err := object.Canonical(kind)
	if err != nil {
		return nil, err
	}
	if m.objs[canonical] == nil {
		m.objs[canonical] = map[string]*entry{}
	}
	return m.objs[canonical], nil
}

func (m *Memory) Get(_ context.Context, kind, name string) (*klitev1.Object, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.bucket(kind)
	if err != nil {
		return nil, 0, err
	}
	e, ok := b[name]
	if !ok {
		return nil, 0, fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
	}
	out := proto.CloneOf(e.obj)
	object.MetaOf(out).ResourceVersion = e.rev
	return out, e.rev, nil
}

func (m *Memory) List(_ context.Context, kind string) ([]*klitev1.Object, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.bucket(kind)
	if err != nil {
		return nil, 0, err
	}
	names := slices.Sorted(maps.Keys(b))
	out := make([]*klitev1.Object, 0, len(b))
	for _, n := range names {
		o := proto.CloneOf(b[n].obj)
		object.MetaOf(o).ResourceVersion = b[n].rev
		out = append(out, o)
	}
	return out, m.rev, nil
}

func (m *Memory) Put(_ context.Context, obj *klitev1.Object, expectedRev int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kind := object.KindOf(obj)
	b, err := m.bucket(kind)
	if err != nil {
		return 0, err
	}
	stored := proto.CloneOf(obj)
	meta := object.MetaOf(stored)
	name := meta.GetName()
	meta.ResourceVersion = 0
	cur := b[name]

	switch {
	case expectedRev == store.RevCreate:
		if cur != nil {
			return 0, fmt.Errorf("%s %q: %w", kind, name, store.ErrAlreadyExists)
		}
		m.stampNew(meta)
	case expectedRev == store.RevAny:
		m.adopt(meta, cur)
	case expectedRev > 0:
		if cur == nil || cur.rev != expectedRev {
			return 0, fmt.Errorf("%s %q: %w", kind, name, store.ErrConflict)
		}
		m.adopt(meta, cur)
	default:
		return 0, fmt.Errorf("invalid expectedRev %d", expectedRev)
	}
	m.rev++
	b[name] = &entry{obj: stored, rev: m.rev}
	return m.rev, nil
}

func (m *Memory) stampNew(meta *klitev1.Meta) {
	if meta.Uid == "" {
		m.uid++
		meta.Uid = "uid-" + strconv.FormatInt(m.uid, 10)
	}
	if meta.CreatedUnix == 0 {
		meta.CreatedUnix = time.Now().Unix()
	}
}

func (m *Memory) adopt(meta *klitev1.Meta, cur *entry) {
	if cur == nil {
		m.stampNew(meta)
		return
	}
	curMeta := object.MetaOf(cur.obj)
	if meta.Uid == "" {
		meta.Uid = curMeta.GetUid()
	}
	if meta.CreatedUnix == 0 {
		meta.CreatedUnix = curMeta.GetCreatedUnix()
	}
}

func (m *Memory) Delete(_ context.Context, kind, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.bucket(kind)
	if err != nil {
		return err
	}
	if _, ok := b[name]; !ok {
		return fmt.Errorf("%s %q: %w", kind, name, store.ErrNotFound)
	}
	delete(b, name)
	m.rev++
	return nil
}

// Watch satisfies the interface for loops that poll anyway: it delivers
// nothing and closes when ctx ends.
func (m *Memory) Watch(ctx context.Context, _ []string, _ int64) (<-chan store.Event, error) {
	ch := make(chan store.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

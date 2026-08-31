// Package store persists k-lite objects in etcd and streams changes back out.
package store

import (
	"context"
	"errors"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

var (
	ErrNotFound      = errors.New("object not found")
	ErrAlreadyExists = errors.New("object already exists")
	ErrConflict      = errors.New("revision conflict")
)

// Put treats these expectedRev values as sentinels. Any positive value must match the object's current mod revision.
const (
	RevCreate int64 = 0  // create only, ErrAlreadyExists when the key is taken
	RevAny    int64 = -1 // blind upsert, no revision check
)

// Event is one change delivered by Watch. A non-nil Err means the watch died and the channel closes next.
type Event struct {
	Type     klitev1.EventType
	Object   *klitev1.Object
	Revision int64
	Err      error
}

// Store reads and writes objects with optimistic concurrency. Kind arguments
// accept any registry alias. List and Get fill meta.resource_version from the
// object's mod revision, and List also returns the cluster revision the listing
// was read at, the resume point for Watch.
type Store interface {
	Get(ctx context.Context, kind, name string) (*klitev1.Object, int64, error)
	List(ctx context.Context, kind string) ([]*klitev1.Object, int64, error)
	Put(ctx context.Context, obj *klitev1.Object, expectedRev int64) (int64, error)
	Delete(ctx context.Context, kind, name string) error
	Watch(ctx context.Context, kinds []string, fromRev int64) (<-chan Event, error)
}

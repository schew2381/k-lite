package object_test

import (
	"fmt"
	"testing"

	"github.com/schew2381/k-lite/internal/object"
)

// minimalYAML is the smallest well-formed document for a kind: envelope keys
// plus a name. Every registered kind must survive it.
func minimalYAML(kind string) string {
	return fmt.Sprintf("apiVersion: klite/v1\nkind: %s\nmetadata:\n  name: x\n", kind)
}

// The registry is four parallel switches (New, KindOf, MetaOf, and the
// codec's message), and a kind wired into some but not all of them once let
// a forged YAML nil-panic klited. This walks every registered kind through
// each switch so the next added kind fails here in a test instead of in
// production.
func TestEveryKindCrossesTheCodec(t *testing.T) {
	t.Parallel()
	for _, kind := range object.Kinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			objs, err := object.Decode([]byte(minimalYAML(kind)))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := object.KindOf(objs[0]); got != kind {
				t.Fatalf("KindOf = %q, want %q", got, kind)
			}
			if got := object.MetaOf(objs[0]).GetName(); got != "x" {
				t.Fatalf("MetaOf name = %q, want the decoded metadata", got)
			}
			if _, err := object.Encode(objs[0]); err != nil {
				t.Fatalf("Encode: %v", err)
			}

			fresh, err := object.New(kind)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if object.KindOf(fresh) != kind {
				t.Fatalf("New(%s) wraps a %q envelope", kind, object.KindOf(fresh))
			}
			if object.MetaOf(fresh) == nil {
				t.Fatal("MetaOf on a fresh envelope must allocate, not return nil")
			}
			if _, err := object.Encode(fresh); err != nil {
				t.Fatalf("Encode of an empty %s: %v", kind, err)
			}
		})
	}
}

// FuzzDecode hammers the YAML boundary, the one place untrusted bytes become
// typed objects. Whatever arrives, the codec may reject it but must never
// panic, and anything it accepts must encode back out (that's the
// `get -o yaml` path for stored objects).
func FuzzDecode(f *testing.F) {
	for _, kind := range object.Kinds {
		f.Add([]byte(minimalYAML(kind)))
	}
	f.Add([]byte(workloadYAML))
	f.Add([]byte(serviceYAML))
	f.Add([]byte(nodeYAML))
	f.Add([]byte(policyYAML))
	f.Add([]byte("apiVersion: klite/v1\nkind: IngressAllocation\nmetadata:\n  name: b.b-aa\nspec:\n  service: b\n  instance: b-aa\n  node: node-1\n  port: 20000\n"))
	f.Add([]byte("apiVersion: klite/v1\nkind: VIPAllocation\nmeta:\n  name: b.node-1\nspec:\n  vip: 10.44.64.1\n"))
	f.Add([]byte("apiVersion: klite/v1\nkind: instances\nmetadata:\n  name: b-aa\nstatus:\n  phase: INSTANCE_PHASE_READY\n"))
	f.Add([]byte("apiVersion: klite/v1\nkind: Workload\nmetadata: 7\n"))
	f.Add([]byte("kind: Workload\n---\n---\napiVersion: klite/v1\nkind: Service\nmetadata:\n  name: s\n"))
	f.Add([]byte("apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: x\nspec:\n  maxInstances: -9999999999999999999\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		objs, err := object.Decode(data)
		if err != nil {
			return
		}
		for _, o := range objs {
			if _, err := object.Encode(o); err != nil {
				t.Fatalf("decoded but would not encode: %v\ninput:\n%s", err, data)
			}
		}
	})
}

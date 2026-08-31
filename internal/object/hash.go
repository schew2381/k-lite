package object

import (
	"fmt"
	"hash/fnv"

	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// TemplateHash returns a short FNV-1a digest of the template's deterministic proto encoding.
// Deterministic marshaling keeps map ordering stable within one binary, which is all
// rollout comparison needs, since every hash a cluster compares comes from klited.
func TemplateHash(t *klitev1.Template) (string, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("marshal template: %w", err)
	}
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf("%x", h.Sum64()), nil
}

package object

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// APIVersion is the only apiVersion this codec accepts or emits.
const APIVersion = "klite/v1"

var docSeparator = regexp.MustCompile(`(?m)^---\s*(#.*)?$`)

// Enum values cross the YAML boundary in the short form users write.
var policyActionShort = map[string]string{
	"ALLOW": "POLICY_ACTION_ALLOW",
	"DENY":  "POLICY_ACTION_DENY",
}

var policyActionLong = map[string]string{
	"POLICY_ACTION_ALLOW": "ALLOW",
	"POLICY_ACTION_DENY":  "DENY",
}

// Decode parses multi-document YAML into Object envelopes, skipping empty documents.
func Decode(data []byte) ([]*klitev1.Object, error) {
	var objs []*klitev1.Object
	for i, doc := range docSeparator.Split(string(data), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj, err := DecodeOne([]byte(doc))
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if obj != nil {
			objs = append(objs, obj)
		}
	}
	return objs, nil
}

// DecodeOne parses a single YAML document. A comments-only document decodes to nil.
func DecodeOne(doc []byte) (*klitev1.Object, error) {
	jsonBytes, err := yaml.YAMLToJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		if string(jsonBytes) == "null" {
			return nil, nil
		}
		return nil, fmt.Errorf("document is not a mapping: %w", err)
	}
	if m == nil {
		return nil, nil
	}

	av, _ := m["apiVersion"].(string)
	if av != APIVersion {
		return nil, fmt.Errorf("apiVersion must be %q, got %q", APIVersion, av)
	}
	rawKind, _ := m["kind"].(string)
	kind, err := Canonical(rawKind)
	if err != nil {
		return nil, err
	}
	delete(m, "apiVersion")
	delete(m, "kind")
	if md, ok := m["metadata"]; ok {
		if _, both := m["meta"]; both {
			return nil, fmt.Errorf("document sets both metadata and meta")
		}
		m["meta"] = md
		delete(m, "metadata")
	}
	if kind == KindNetworkPolicy {
		rewritePolicyAction(m, policyActionShort)
	}

	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	obj, err := New(kind)
	if err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(body, message(obj)); err != nil {
		return nil, fmt.Errorf("%s: %w", strings.ToLower(kind), err)
	}
	return obj, nil
}

// Encode renders one object as a user-facing YAML document with apiVersion, kind, and metadata keys.
func Encode(o *klitev1.Object) ([]byte, error) {
	body, err := EncodeJSON(o)
	if err != nil {
		return nil, err
	}
	return yaml.JSONToYAML(body)
}

// EncodeJSON is Encode without the final YAML rendering.
func EncodeJSON(o *klitev1.Object) ([]byte, error) {
	kind := KindOf(o)
	if kind == "" {
		return nil, fmt.Errorf("empty object envelope")
	}
	body, err := protojson.Marshal(message(o))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if md, ok := m["meta"]; ok {
		m["metadata"] = md
		delete(m, "meta")
	}
	if kind == KindNetworkPolicy {
		rewritePolicyAction(m, policyActionLong)
	}
	m["apiVersion"] = APIVersion
	m["kind"] = kind
	return json.Marshal(m)
}

// message returns the typed message inside the envelope, so protojson sees Workload fields rather than the oneof wrapper.
func message(o *klitev1.Object) proto.Message {
	switch k := o.GetKind().(type) {
	case *klitev1.Object_Workload:
		return k.Workload
	case *klitev1.Object_Service:
		return k.Service
	case *klitev1.Object_Node:
		return k.Node
	case *klitev1.Object_NetworkPolicy:
		return k.NetworkPolicy
	case *klitev1.Object_Instance:
		return k.Instance
	case *klitev1.Object_VipAllocation:
		return k.VipAllocation
	}
	return nil
}

func rewritePolicyAction(m map[string]any, table map[string]string) {
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return
	}
	action, ok := spec["action"].(string)
	if !ok {
		return
	}
	if mapped, ok := table[action]; ok {
		spec["action"] = mapped
	}
}

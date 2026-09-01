// Package object holds the k-lite object model, from the kind registry and YAML codec through validation, defaulting, and template hashing.
package object

import (
	"fmt"
	"strings"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// The canonical kind names match the YAML `kind:` field and the CONTEXT.md vocabulary.
const (
	KindWorkload          = "Workload"
	KindService           = "Service"
	KindNode              = "Node"
	KindNetworkPolicy     = "NetworkPolicy"
	KindInstance          = "Instance"
	KindVIPAllocation     = "VIPAllocation"
	KindIngressAllocation = "IngressAllocation"
)

// Kinds lists every canonical kind.
var Kinds = []string{KindWorkload, KindService, KindNode, KindNetworkPolicy, KindInstance, KindVIPAllocation, KindIngressAllocation}

// LabelPendingDelete marks a Node whose deletion waits on its drain: the
// server sets it instead of deleting outright, and the node controller
// removes the record once the last instance has left (ADR 0010). Re-applying
// the node's YAML overwrites labels and thereby cancels the pending delete.
const LabelPendingDelete = "klite.io/pending-delete"

var plurals = map[string]string{
	KindWorkload:          "workloads",
	KindService:           "services",
	KindNode:              "nodes",
	KindNetworkPolicy:     "networkpolicies",
	KindInstance:          "instances",
	KindVIPAllocation:     "vipallocations",
	KindIngressAllocation: "ingressallocations",
}

var aliases = func() map[string]string {
	m := make(map[string]string, len(Kinds)*2)
	for _, k := range Kinds {
		m[strings.ToLower(k)] = k
		m[plurals[k]] = k
	}
	return m
}()

// Canonical resolves a kind name or one of its aliases ("workloads", "workload", "Workload") case-insensitively.
func Canonical(s string) (string, error) {
	if k, ok := aliases[strings.ToLower(s)]; ok {
		return k, nil
	}
	return "", fmt.Errorf("unknown kind %q (one of %s)", s, strings.Join(Kinds, ", "))
}

// Plural returns the lowercase plural for a canonical kind, used in store keys and CLI arguments.
func Plural(kind string) string {
	return plurals[kind]
}

// New returns an Object envelope wrapping an empty message of the given canonical kind.
func New(kind string) (*klitev1.Object, error) {
	switch kind {
	case KindWorkload:
		return &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{}}}, nil
	case KindService:
		return &klitev1.Object{Kind: &klitev1.Object_Service{Service: &klitev1.Service{}}}, nil
	case KindNode:
		return &klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{}}}, nil
	case KindNetworkPolicy:
		return &klitev1.Object{Kind: &klitev1.Object_NetworkPolicy{NetworkPolicy: &klitev1.NetworkPolicy{}}}, nil
	case KindInstance:
		return &klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{}}}, nil
	case KindVIPAllocation:
		return &klitev1.Object{Kind: &klitev1.Object_VipAllocation{VipAllocation: &klitev1.VIPAllocation{}}}, nil
	case KindIngressAllocation:
		return &klitev1.Object{Kind: &klitev1.Object_IngressAllocation{IngressAllocation: &klitev1.IngressAllocation{}}}, nil
	}
	return nil, fmt.Errorf("unknown kind %q", kind)
}

// KindOf returns the canonical kind of a wrapped object, or "" when the envelope is empty.
func KindOf(o *klitev1.Object) string {
	switch o.GetKind().(type) {
	case *klitev1.Object_Workload:
		return KindWorkload
	case *klitev1.Object_Service:
		return KindService
	case *klitev1.Object_Node:
		return KindNode
	case *klitev1.Object_NetworkPolicy:
		return KindNetworkPolicy
	case *klitev1.Object_Instance:
		return KindInstance
	case *klitev1.Object_VipAllocation:
		return KindVIPAllocation
	case *klitev1.Object_IngressAllocation:
		return KindIngressAllocation
	}
	return ""
}

// MetaOf returns the object's Meta, allocating it when absent so callers can always write through it.
func MetaOf(o *klitev1.Object) *klitev1.Meta {
	switch k := o.GetKind().(type) {
	case *klitev1.Object_Workload:
		if k.Workload.Meta == nil {
			k.Workload.Meta = &klitev1.Meta{}
		}
		return k.Workload.Meta
	case *klitev1.Object_Service:
		if k.Service.Meta == nil {
			k.Service.Meta = &klitev1.Meta{}
		}
		return k.Service.Meta
	case *klitev1.Object_Node:
		if k.Node.Meta == nil {
			k.Node.Meta = &klitev1.Meta{}
		}
		return k.Node.Meta
	case *klitev1.Object_NetworkPolicy:
		if k.NetworkPolicy.Meta == nil {
			k.NetworkPolicy.Meta = &klitev1.Meta{}
		}
		return k.NetworkPolicy.Meta
	case *klitev1.Object_Instance:
		if k.Instance.Meta == nil {
			k.Instance.Meta = &klitev1.Meta{}
		}
		return k.Instance.Meta
	case *klitev1.Object_VipAllocation:
		if k.VipAllocation.Meta == nil {
			k.VipAllocation.Meta = &klitev1.Meta{}
		}
		return k.VipAllocation.Meta
	case *klitev1.Object_IngressAllocation:
		if k.IngressAllocation.Meta == nil {
			k.IngressAllocation.Meta = &klitev1.Meta{}
		}
		return k.IngressAllocation.Meta
	}
	return &klitev1.Meta{}
}

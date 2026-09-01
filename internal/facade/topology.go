package facade

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

// Topology is the composed graph the UI's topology page renders: Nodes with
// their Instances, Services with their routing targets, and the policy list
// the edge verdicts derive from.
type Topology struct {
	Nodes       []TopoNode     `json:"nodes"`
	Services    []TopoService  `json:"services"`
	Policies    []TopoPolicy   `json:"policies"`
	Workloads   []TopoWorkload `json:"workloads"`
	Unscheduled []TopoInstance `json:"unscheduled"`
}

type TopoInstance struct {
	Name     string `json:"name"`
	Workload string `json:"workload"`
	Phase    string `json:"phase"`
	Restarts int32  `json:"restarts"`
	IP       string `json:"ip"`
}

type TopoNode struct {
	Name          string         `json:"name"`
	Phase         string         `json:"phase"`
	Unschedulable bool           `json:"unschedulable"`
	Instances     []TopoInstance `json:"instances"`
}

type TopoService struct {
	Name       string            `json:"name"`
	Port       int32             `json:"port"`
	TargetPort int32             `json:"targetPort"`
	Selector   map[string]string `json:"selector"`
	Endpoints  []string          `json:"endpoints"`
}

type TopoRule struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Except []string `json:"except,omitempty"`
}

type TopoPolicy struct {
	Name   string     `json:"name"`
	Action string     `json:"action"`
	Rules  []TopoRule `json:"rules"`
}

type TopoWorkload struct {
	Name  string `json:"name"`
	Ready int32  `json:"ready"`
	Total int32  `json:"total"`
}

// handleTopology lists all five kinds and returns the composed graph.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	lists := make(map[string][]*klitev1.Object, len(object.Kinds))
	for _, kind := range object.Kinds {
		objs, err := s.listKind(r.Context(), kind)
		if err != nil {
			writeRPCError(w, rpcCause(err))
			return
		}
		lists[kind] = objs
	}
	writeJSON(w, http.StatusOK, ComposeTopology(lists))
}

// ComposeTopology folds List results into the topology graph. It is a pure
// function so tests can feed it canned objects.
func ComposeTopology(lists map[string][]*klitev1.Object) *Topology {
	t := &Topology{
		Nodes:       []TopoNode{},
		Services:    []TopoService{},
		Policies:    []TopoPolicy{},
		Workloads:   []TopoWorkload{},
		Unscheduled: []TopoInstance{},
	}

	// Workload template labels answer "which Instances does this selector
	// pick": a Service selects Workloads, and its Endpoints are those
	// Workloads' routing-ready Instances.
	templateLabels := make(map[string]map[string]string)
	for _, o := range lists[object.KindWorkload] {
		wl := o.GetWorkload()
		templateLabels[wl.GetMeta().GetName()] = wl.GetSpec().GetTemplate().GetLabels()
		t.Workloads = append(t.Workloads, TopoWorkload{
			Name:  wl.GetMeta().GetName(),
			Ready: wl.GetStatus().GetReadyInstances(),
			Total: wl.GetSpec().GetReplicas(),
		})
	}

	byNode := make(map[string][]TopoInstance)
	instanceWorkload := make(map[string]string)
	routable := make(map[string]bool) // instance name -> counts as an Endpoint
	for _, o := range lists[object.KindInstance] {
		in := o.GetInstance()
		ti := TopoInstance{
			Name:     in.GetMeta().GetName(),
			Workload: in.GetSpec().GetWorkload(),
			Phase:    displayPhase(in.GetStatus().GetPhase().String(), "INSTANCE_PHASE_"),
			Restarts: in.GetStatus().GetRestarts(),
			IP:       in.GetStatus().GetInstanceIp(),
		}
		instanceWorkload[ti.Name] = ti.Workload
		// Running counts as ready until M4 wires readiness probes, matching
		// the workload controller's own arithmetic.
		switch in.GetStatus().GetPhase() {
		case klitev1.InstancePhase_INSTANCE_PHASE_RUNNING, klitev1.InstancePhase_INSTANCE_PHASE_READY:
			routable[ti.Name] = true
		default:
		}
		if node := in.GetSpec().GetNode(); node != "" {
			byNode[node] = append(byNode[node], ti)
		} else {
			t.Unscheduled = append(t.Unscheduled, ti)
		}
	}

	for _, o := range lists[object.KindNode] {
		n := o.GetNode()
		name := n.GetMeta().GetName()
		instances := byNode[name]
		slices.SortFunc(instances, func(a, b TopoInstance) int { return strings.Compare(a.Name, b.Name) })
		if instances == nil {
			instances = []TopoInstance{}
		}
		t.Nodes = append(t.Nodes, TopoNode{
			Name:          name,
			Phase:         displayPhase(n.GetStatus().GetPhase().String(), "NODE_PHASE_"),
			Unschedulable: n.GetStatus().GetUnschedulable(),
			Instances:     instances,
		})
	}

	for _, o := range lists[object.KindService] {
		svc := o.GetService()
		spec := svc.GetSpec()
		endpoints := []string{}
		for name, wl := range instanceWorkload {
			if routable[name] && selectorMatches(spec.GetSelector(), templateLabels[wl]) {
				endpoints = append(endpoints, name)
			}
		}
		slices.Sort(endpoints)
		selector := spec.GetSelector()
		if selector == nil {
			selector = map[string]string{}
		}
		t.Services = append(t.Services, TopoService{
			Name:       svc.GetMeta().GetName(),
			Port:       spec.GetPort(),
			TargetPort: spec.GetTargetPort(),
			Selector:   maps.Clone(selector),
			Endpoints:  endpoints,
		})
	}

	for _, o := range lists[object.KindNetworkPolicy] {
		p := o.GetNetworkPolicy()
		rules := make([]TopoRule, 0, len(p.GetSpec().GetRules()))
		for _, r := range p.GetSpec().GetRules() {
			rules = append(rules, TopoRule{From: r.GetFrom(), To: r.GetTo(), Except: r.GetExcept()})
		}
		t.Policies = append(t.Policies, TopoPolicy{
			Name:   p.GetMeta().GetName(),
			Action: displayAction(p.GetSpec().GetAction()),
			Rules:  rules,
		})
	}

	sortByName(t.Nodes, func(n TopoNode) string { return n.Name })
	sortByName(t.Services, func(s TopoService) string { return s.Name })
	sortByName(t.Policies, func(p TopoPolicy) string { return p.Name })
	sortByName(t.Workloads, func(w TopoWorkload) string { return w.Name })
	sortByName(t.Unscheduled, func(i TopoInstance) string { return i.Name })
	return t
}

// selectorMatches reports whether every selector pair appears in the labels.
// An empty selector selects nothing, matching the CLI's <none> rendering.
func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func sortByName[T any](s []T, name func(T) string) {
	slices.SortFunc(s, func(a, b T) int { return strings.Compare(name(a), name(b)) })
}

// displayPhase turns an enum value name into display form, the way the CLI
// tables do: NODE_PHASE_NOT_READY becomes NotReady.
func displayPhase(enumName, prefix string) string {
	s := strings.TrimPrefix(enumName, prefix)
	if s == "UNSPECIFIED" {
		return "Unknown"
	}
	var b strings.Builder
	for word := range strings.SplitSeq(s, "_") {
		if word == "" {
			continue
		}
		b.WriteString(strings.ToUpper(word[:1]))
		b.WriteString(strings.ToLower(word[1:]))
	}
	return b.String()
}

func displayAction(a klitev1.PolicyAction) string {
	switch a {
	case klitev1.PolicyAction_POLICY_ACTION_ALLOW:
		return "ALLOW"
	case klitev1.PolicyAction_POLICY_ACTION_DENY:
		return "DENY"
	default:
		return "UNKNOWN"
	}
}

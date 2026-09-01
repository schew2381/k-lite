package controller

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/policy"
	"github.com/schew2381/k-lite/internal/store"
)

// NodeSnapshot is one node's full desired state: its instances plus the
// NetDesired its infra pod and Envoy consume.
type NodeSnapshot struct {
	Revision  int64
	Instances []*klitev1.Instance
	Net       *klitev1.NetDesired
}

// Endpoints turns the store's services, instances, nodes, policies, and VIP
// allocations into per-node NodeSnapshots. Unlike the leader-only loops it
// runs on every klited replica, because each replica's xDS server must answer
// whichever Envoy dials it (ADR 0007). It only reads the store, so replicas
// converge on identical output.
type Endpoints struct {
	st       store.Store
	onChange func(node string, revision int64, net *klitev1.NetDesired)

	mu       sync.Mutex
	ready    bool
	version  int64
	nodes    map[string]*NodeSnapshot
	subs     map[int]chan struct{}
	nextSub  int
	onRemove func(node string)
}

// NewEndpoints wires the engine. onChange fires outside the lock for every
// node whose snapshot content changed (klited hangs the xDS cache off it) and
// may be nil.
func NewEndpoints(st store.Store, onChange func(node string, revision int64, net *klitev1.NetDesired)) *Endpoints {
	if onChange == nil {
		onChange = func(string, int64, *klitev1.NetDesired) {}
	}
	return &Endpoints{
		st:       st,
		onChange: onChange,
		onRemove: func(string) {},
		nodes:    map[string]*NodeSnapshot{},
		subs:     map[int]chan struct{}{},
	}
}

// OnNodeRemoved registers fn to run, outside the lock, when a departed
// node's snapshot is dropped for good — after one full pass has already
// served its watchers the empty state. klited hangs the xDS cache eviction
// off it, so the ADS cache doesn't hold deleted nodes forever.
func (e *Endpoints) OnNodeRemoved(fn func(node string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if fn != nil {
		e.onRemove = fn
	}
}

// Run recomputes on every relevant store change and on the resync tick,
// blocking until ctx ends.
func (e *Endpoints) Run(ctx context.Context) {
	kinds := []string{
		object.KindService, object.KindInstance, object.KindNode,
		object.KindNetworkPolicy, object.KindVIPAllocation, object.KindIngressAllocation,
	}
	runLoop(ctx, e.st, "endpoints", kinds, e.recompute)
}

// Snapshot returns the node's current desired state. ok is false until the
// first successful recompute, so callers never mistake "not computed yet"
// for "nothing desired". A node the engine has never seen yields an empty
// snapshot at revision zero. The instances and net inside are shared with
// the engine and later passes, so treat them as read-only.
func (e *Endpoints) Snapshot(node string) (NodeSnapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		return NodeSnapshot{}, false
	}
	if snap, ok := e.nodes[node]; ok {
		return *snap, true
	}
	return NodeSnapshot{Net: &klitev1.NetDesired{}}, true
}

// Subscribe returns a channel that receives a kick after any node's snapshot
// changes. Callers re-read Snapshot and compare revisions. cancel releases
// the subscription.
func (e *Endpoints) Subscribe() (<-chan struct{}, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.nextSub
	e.nextSub++
	ch := make(chan struct{}, 1)
	e.subs[id] = ch
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.subs, id)
	}
}

// recompute is level-based: list everything, rebuild every node's snapshot,
// and bump revisions only where content moved. A failed list skips the pass
// so a store hiccup never masquerades as an empty cluster.
func (e *Endpoints) recompute(ctx context.Context) error {
	in, err := listInputs(ctx, e.st)
	if err != nil {
		return err
	}
	changed, removed := e.store(buildAll(in))
	e.mu.Lock()
	onRemove := e.onRemove
	e.mu.Unlock()
	for _, node := range removed {
		onRemove(node)
		slog.Info("departed node dropped", "node", node)
	}
	for _, node := range changed {
		snap, _ := e.Snapshot(node)
		e.onChange(node, snap.Revision, snap.Net)
	}
	if len(changed) > 0 {
		slog.Info("net desired state rebuilt", "nodes", changed)
	}
	return nil
}

// store swaps the freshly built snapshots in, bumping the version once when
// anything moved and kicking subscribers. A node that left the inputs decays
// to an empty snapshot rather than vanishing, so its watcher hears about it.
// A node that already decayed is dropped for good and reported in removed.
func (e *Endpoints) store(built map[string]*NodeSnapshot) (changed, removed []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	emptySnap := &NodeSnapshot{Net: &klitev1.NetDesired{}}
	for node, prev := range e.nodes {
		if _, ok := built[node]; ok {
			continue
		}
		if snapshotEqual(prev, emptySnap) {
			delete(e.nodes, node)
			removed = append(removed, node)
			continue
		}
		built[node] = &NodeSnapshot{Net: &klitev1.NetDesired{}}
	}
	for node, next := range built {
		if prev := e.nodes[node]; prev != nil && snapshotEqual(prev, next) {
			next.Revision = prev.Revision
			e.nodes[node] = next
			continue
		}
		changed = append(changed, node)
	}
	if len(changed) > 0 {
		e.version++
		for _, node := range changed {
			built[node].Revision = e.version
			e.nodes[node] = built[node]
		}
	}
	if len(changed) > 0 || !e.ready {
		for _, ch := range e.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	e.ready = true
	return slices.Sorted(slices.Values(changed)), slices.Sorted(slices.Values(removed))
}

func snapshotEqual(a, b *NodeSnapshot) bool {
	return proto.Equal(
		&klitev1.DesiredState{Instances: a.Instances, Net: a.Net},
		&klitev1.DesiredState{Instances: b.Instances, Net: b.Net},
	)
}

// inputs is one consistent-enough read of everything the build consumes.
type inputs struct {
	services  []*klitev1.Service
	instances []*klitev1.Instance
	nodes     []string
	policies  []*klitev1.NetworkPolicy
	vips      map[string]string                         // AllocationName -> VIP
	ingress   map[string]*klitev1.IngressAllocationSpec // by IngressAllocationName
	advertise map[string]string                         // node -> advertised machine IP (ADR 0024)
}

func listInputs(ctx context.Context, st store.Store) (*inputs, error) {
	in := &inputs{
		vips:      map[string]string{},
		ingress:   map[string]*klitev1.IngressAllocationSpec{},
		advertise: map[string]string{},
	}
	svcObjs, _, err := st.List(ctx, object.KindService)
	if err != nil {
		return nil, err
	}
	for _, o := range svcObjs {
		in.services = append(in.services, o.GetService())
	}
	slices.SortFunc(in.services, func(a, b *klitev1.Service) int {
		return cmp.Compare(a.GetMeta().GetName(), b.GetMeta().GetName())
	})

	instObjs, _, err := st.List(ctx, object.KindInstance)
	if err != nil {
		return nil, err
	}
	for _, o := range instObjs {
		in.instances = append(in.instances, o.GetInstance())
	}
	slices.SortFunc(in.instances, func(a, b *klitev1.Instance) int {
		return cmp.Compare(a.GetMeta().GetName(), b.GetMeta().GetName())
	})

	nodeObjs, _, err := st.List(ctx, object.KindNode)
	if err != nil {
		return nil, err
	}
	nodeSet := map[string]bool{}
	for _, o := range nodeObjs {
		node := o.GetNode()
		nodeSet[node.GetMeta().GetName()] = true
		if addr := node.GetStatus().GetAdvertiseAddress(); addr != "" {
			in.advertise[node.GetMeta().GetName()] = addr
		}
	}
	for _, inst := range in.instances {
		if n := inst.GetSpec().GetNode(); n != "" {
			nodeSet[n] = true
		}
	}
	in.nodes = slices.Sorted(maps.Keys(nodeSet))

	polObjs, _, err := st.List(ctx, object.KindNetworkPolicy)
	if err != nil {
		return nil, err
	}
	for _, o := range polObjs {
		in.policies = append(in.policies, o.GetNetworkPolicy())
	}

	if err := listAllocations(ctx, st, in); err != nil {
		return nil, err
	}
	return in, nil
}

// listAllocations fills the two server-materialized port/address maps: VIPs
// per (service, node) and ingress ports per (service, instance).
func listAllocations(ctx context.Context, st store.Store, in *inputs) error {
	allocObjs, _, err := st.List(ctx, object.KindVIPAllocation)
	if err != nil {
		return err
	}
	for _, o := range allocObjs {
		alloc := o.GetVipAllocation()
		in.vips[alloc.GetMeta().GetName()] = alloc.GetSpec().GetVip()
	}

	ingObjs, _, err := st.List(ctx, object.KindIngressAllocation)
	if err != nil {
		return err
	}
	for _, o := range ingObjs {
		alloc := o.GetIngressAllocation()
		in.ingress[alloc.GetMeta().GetName()] = alloc.GetSpec()
	}
	return nil
}

// buildAll assembles every node's snapshot from one input read. Endpoint
// groups span the whole cluster (any node may dial any instance over klite0,
// ADR 0006), while services, probe targets, and instances are per-node.
func buildAll(in *inputs) map[string]*NodeSnapshot {
	compiled := policy.Compile(in.policies)
	groups := buildGroups(in)
	identity := buildIPIdentity(in)

	out := make(map[string]*NodeSnapshot, len(in.nodes))
	for _, node := range in.nodes {
		net := &klitev1.NetDesired{
			IpIdentity: identity,
			Policies:   compiled,
		}
		for _, svc := range in.services {
			name := svc.GetMeta().GetName()
			vip, ok := in.vips[AllocationName(name, node)]
			if !ok {
				continue // the allocation is still in flight and its write re-kicks us
			}
			net.Services = append(net.Services, &klitev1.ServiceVIP{
				Service:    name,
				Vip:        vip,
				Port:       svc.GetSpec().GetPort(),
				TargetPort: svc.GetSpec().GetTargetPort(),
			})
			if g := groups[name]; len(g.GetEndpoints()) > 0 {
				net.Endpoints = append(net.Endpoints, g)
			}
		}
		net.ProbeTargets = buildProbeTargets(in, node)
		net.IngressListeners = buildIngressListeners(in, node)
		out[node] = &NodeSnapshot{Instances: nodeInstances(in, node), Net: net}
	}
	return out
}

// buildIngressListeners lists the node's mTLS ingress listeners: one per
// allocation whose instance lives here and has an address. Allocation-driven
// rather than endpoint-driven on purpose — the listener stands from instance
// birth, well before any consumer's EDS routes to it, and outlives Ready so
// draining endpoints stay reachable through the hop (ADR 0024, ADR 0010).
func buildIngressListeners(in *inputs, node string) []*klitev1.IngressListener {
	instances := make(map[string]*klitev1.Instance, len(in.instances))
	for _, inst := range in.instances {
		instances[inst.GetMeta().GetName()] = inst
	}
	targetPorts := make(map[string]int32, len(in.services))
	for _, svc := range in.services {
		targetPorts[svc.GetMeta().GetName()] = svc.GetSpec().GetTargetPort()
	}
	var out []*klitev1.IngressListener
	for _, name := range slices.Sorted(maps.Keys(in.ingress)) {
		alloc := in.ingress[name]
		if alloc.GetNode() != node {
			continue
		}
		inst := instances[alloc.GetInstance()]
		ip := inst.GetStatus().GetInstanceIp()
		targetPort, ok := targetPorts[alloc.GetService()]
		if ip == "" || !ok {
			continue // not addressed yet, or the allocation outlived its service
		}
		out = append(out, &klitev1.IngressListener{
			Service:    alloc.GetService(),
			Port:       alloc.GetPort(),
			PodIp:      ip,
			TargetPort: targetPort,
		})
	}
	return out
}

// buildGroups collects each service's dialable endpoints: selected instances
// that are Ready (or Draining, which Envoy treats as unhealthy-but-known,
// ADR 0010) with an address. Each endpoint also carries how OTHER nodes reach
// it — the owning node's advertised IP plus its allocated ingress port — so a
// consuming node's Envoy can render remote endpoints against the mTLS ingress
// listener instead of the dead flat-bridge path (ADR 0024).
func buildGroups(in *inputs) map[string]*klitev1.EndpointGroup {
	out := make(map[string]*klitev1.EndpointGroup, len(in.services))
	for _, svc := range in.services {
		name := svc.GetMeta().GetName()
		group := &klitev1.EndpointGroup{Service: name}
		for _, inst := range in.instances {
			if !selects(svc, inst) {
				continue
			}
			health, ok := endpointHealth(inst.GetStatus().GetPhase())
			if !ok || inst.GetStatus().GetInstanceIp() == "" {
				continue
			}
			node := inst.GetSpec().GetNode()
			group.Endpoints = append(group.Endpoints, &klitev1.Endpoint{
				Ip:             inst.GetStatus().GetInstanceIp(),
				Port:           svc.GetSpec().GetTargetPort(),
				Node:           node,
				Health:         health,
				IngressPort:    in.ingress[IngressAllocationName(name, inst.GetMeta().GetName())].GetPort(),
				MachineAddress: in.advertise[node],
			})
		}
		out[name] = group
	}
	return out
}

func endpointHealth(phase klitev1.InstancePhase) (klitev1.EndpointHealth, bool) {
	switch phase {
	case klitev1.InstancePhase_INSTANCE_PHASE_READY:
		return klitev1.EndpointHealth_ENDPOINT_HEALTH_READY, true
	case klitev1.InstancePhase_INSTANCE_PHASE_DRAINING:
		return klitev1.EndpointHealth_ENDPOINT_HEALTH_DRAINING, true
	default:
		return klitev1.EndpointHealth_ENDPOINT_HEALTH_UNSPECIFIED, false
	}
}

// buildIPIdentity maps every addressed instance to the first service that
// selects it, the source identity Envoy RBAC matches on. Phase is
// deliberately ignored because a not-yet-ready instance can still open
// connections.
func buildIPIdentity(in *inputs) map[string]string {
	out := map[string]string{}
	for _, inst := range in.instances {
		ip := inst.GetStatus().GetInstanceIp()
		if ip == "" {
			continue
		}
		for _, svc := range in.services {
			if selects(svc, inst) {
				out[ip] = svc.GetMeta().GetName()
				break
			}
		}
	}
	return out
}

// buildProbeTargets lists the node's own addressed instances for klite-net
// to TCP-probe, on the readiness port or the selecting service's targetPort
// when the spec names none.
func buildProbeTargets(in *inputs, node string) []*klitev1.ProbeTarget {
	var out []*klitev1.ProbeTarget
	for _, inst := range in.instances {
		if inst.GetSpec().GetNode() != node || inst.GetStatus().GetInstanceIp() == "" {
			continue
		}
		port := inst.GetSpec().GetContainer().GetReadinessProbe().GetTcpPort()
		if port == 0 {
			port = fallbackProbePort(in, inst)
		}
		if port == 0 {
			continue
		}
		out = append(out, &klitev1.ProbeTarget{
			Instance: inst.GetMeta().GetName(),
			Ip:       inst.GetStatus().GetInstanceIp(),
			Port:     port,
		})
	}
	return out
}

func fallbackProbePort(in *inputs, inst *klitev1.Instance) int32 {
	for _, svc := range in.services {
		if selects(svc, inst) {
			return svc.GetSpec().GetTargetPort()
		}
	}
	return 0
}

func nodeInstances(in *inputs, node string) []*klitev1.Instance {
	var out []*klitev1.Instance
	for _, inst := range in.instances {
		if inst.GetSpec().GetNode() == node {
			out = append(out, inst)
		}
	}
	return out
}

// selects reports whether the service's selector matches the instance's
// labels. An empty selector selects nothing, matching Kubernetes semantics.
func selects(svc *klitev1.Service, inst *klitev1.Instance) bool {
	sel := svc.GetSpec().GetSelector()
	if len(sel) == 0 {
		return false
	}
	labels := inst.GetMeta().GetLabels()
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

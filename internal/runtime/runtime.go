// Package runtime runs instances as containers. The agent programs it through
// the Runtime interface. The Docker implementation lives in docker.go, and
// tests substitute a fake.
package runtime

import (
	"context"
	"io"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Every workload container carries these labels, and `docker ps --filter` on
// them is a node's entire worldview (ADR 0003).
const (
	LabelRole         = "io.klite.role"
	LabelNode         = "io.klite.node"
	LabelWorkload     = "io.klite.workload"
	LabelInstance     = "io.klite.instance"
	LabelInstanceUID  = "io.klite.instance-uid"
	LabelTemplateHash = "io.klite.template-hash"
	LabelConfigHash   = "io.klite.config-hash"

	RoleWorkload = "workload"
	RoleNet      = "net"
	RoleEnvoy    = "envoy"
)

// StateRunning is Docker's state string for a live container. Anything else
// (created, exited, dead) means the instance needs work.
const StateRunning = "running"

// The agent subscribes to these event actions.
const (
	ActionStart = "start"
	ActionDie   = "die"
)

// RunningInstance is one workload container as the engine reports it.
type RunningInstance struct {
	ContainerID  string
	InstanceName string
	InstanceUID  string
	TemplateHash string
	State        string
	ExitCode     int // meaningful only when State is not running
	IP           string
}

// Event is a container lifecycle change. ExitCode is set on die events.
type Event struct {
	Action       string
	ContainerID  string
	InstanceName string
	InstanceUID  string
	ExitCode     int
}

// InfraContainer describes one infra-pod container (ADR 0008). Every
// netns-scoped option (static IP, capabilities, extra hosts, published
// ports) belongs on the netns donor; a container joining another's namespace
// carries none of them.
type InfraContainer struct {
	Name       string
	Image      string
	Cmd        []string
	Labels     map[string]string // must include LabelConfigHash for drift checks
	StaticIP   string            // klite0 address; mutually exclusive with JoinNetns
	JoinNetns  string            // container name whose netns to join
	CapAdd     []string
	ExtraHosts []string
	Binds      []string
	// Ports maps "port/proto" to a "host:port" bind on the host.
	Ports map[string]string
}

// InfraStatus is what drift detection needs to know about a named container.
type InfraStatus struct {
	ID         string
	Running    bool
	ConfigHash string
	StartedAt  time.Time
}

// InfraInfo identifies one infra container and its klite0 address, for
// spotting stale donors squatting an address a node was assigned.
type InfraInfo struct {
	ID   string
	Name string
	Node string
	IP   string
}

// Runtime is everything the agent needs from a container engine.
type Runtime interface {
	// EnsureNetwork creates klite0 or validates an existing one. A klite0
	// with different IPAM is an error, never adopted.
	EnsureNetwork(ctx context.Context) error
	// EnsureImage pulls image unless it's already local.
	EnsureImage(ctx context.Context, image string) error
	// RunInstance creates and starts the instance's container on the node,
	// returning the container ID. A non-empty dnsIP wires the container's
	// resolver to that node's klite-net (ADR 0008).
	RunInstance(ctx context.Context, inst *klitev1.Instance, node, dnsIP string) (string, error)
	// RunInfra creates and starts an infra-pod container, replacing any
	// container already holding the name.
	RunInfra(ctx context.Context, spec *InfraContainer) (string, error)
	// InspectInfra reports a named container's state, or nil when no such
	// container exists.
	InspectInfra(ctx context.Context, name string) (*InfraStatus, error)
	// ListInfra lists infra containers carrying the given role label,
	// running or not.
	ListInfra(ctx context.Context, role string) ([]InfraInfo, error)
	// StopInstance sends SIGTERM and escalates to SIGKILL after gracePeriod.
	StopInstance(ctx context.Context, id string, gracePeriod time.Duration) error
	// RemoveInstance force-removes a container. Removing one that's already
	// gone succeeds.
	RemoveInstance(ctx context.Context, id string) error
	// ListInstances returns the node's workload containers, running or not.
	ListInstances(ctx context.Context, node string) ([]RunningInstance, error)
	// WatchEvents streams start and die events for the node's containers.
	// The channel closes when the underlying stream fails. The caller then
	// resyncs with ListInstances and subscribes again.
	WatchEvents(ctx context.Context, node string) (<-chan Event, error)
	// InspectIP returns the container's klite0 address, or "" before one
	// is assigned.
	InspectIP(ctx context.Context, id string) (string, error)
	// Logs opens the container's log stream, stdout and stderr interleaved.
	// tail > 0 limits it to that many recent lines, and follow keeps the
	// stream open until the container exits or ctx ends. The caller closes
	// the reader.
	Logs(ctx context.Context, containerID string, follow bool, tail int32) (io.ReadCloser, error)
}

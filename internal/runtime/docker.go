package runtime

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/go-units"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// These constants pin klite0's IPAM (ADR 0008). Dynamic instance addresses
// come from ipRange, so everything below 10.44.128.0 stays free for static
// assignment, infra pods at 10.44.0.x and VIPs in 10.44.64.0/18.
const (
	networkName  = "klite0"
	networkLabel = "io.klite.network"

	subnet  = "10.44.0.0/16"
	gateway = "10.44.0.1"
	ipRange = "10.44.128.0/17"
)

// Docker drives one shared dockerd. Every moby import stays inside this
// package because the v0.x client breaks between minors
// (research/docker-go-sdk.md).
type Docker struct {
	cli *client.Client
}

// NewDocker connects to dockerd. host overrides everything, and otherwise the
// SDK honors DOCKER_HOST. With neither set we probe the usual local sockets,
// because the SDK never reads the CLI's context store and would miss colima.
func NewDocker(host string) (*Docker, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	} else if os.Getenv("DOCKER_HOST") == "" {
		if sock := probeSocket(); sock != "" {
			opts = append(opts, client.WithHost("unix://"+sock))
		}
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Docker{cli: cli}, nil
}

func probeSocket() string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/var/run/docker.sock",
		filepath.Join(home, ".colima/default/docker.sock"),
		filepath.Join(home, ".docker/run/docker.sock"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ContainerName is the deterministic name for an instance's container.
func ContainerName(node, instance string) string {
	return "klite." + node + "." + instance
}

// EnsureNetwork creates klite0 or validates an existing one. Every address the
// cluster hands out assumes this exact IPAM layout, so a mismatched klite0 is
// refused rather than adopted.
func (d *Docker) EnsureNetwork(ctx context.Context) error {
	got, err := d.cli.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	switch {
	case err == nil:
		return validateIPAM(got.Network.IPAM.Config)
	case !cerrdefs.IsNotFound(err):
		return fmt.Errorf("inspect network %s: %w", networkName, err)
	}
	_, err = d.cli.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
		Driver: "bridge",
		IPAM: &network.IPAM{Config: []network.IPAMConfig{{
			Subnet:  netip.MustParsePrefix(subnet),
			Gateway: netip.MustParseAddr(gateway),
			IPRange: netip.MustParsePrefix(ipRange),
		}}},
		Labels: map[string]string{networkLabel: networkName},
	})
	if err == nil {
		return nil
	}
	if cerrdefs.IsConflict(err) {
		// Another agent won the create race, so validate its work.
		got, ierr := d.cli.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
		if ierr != nil {
			return fmt.Errorf("inspect network %s after create conflict: %w", networkName, ierr)
		}
		return validateIPAM(got.Network.IPAM.Config)
	}
	return fmt.Errorf("create network %s: %w", networkName, err)
}

func validateIPAM(cfgs []network.IPAMConfig) error {
	if len(cfgs) == 1 {
		c := cfgs[0]
		if c.Subnet.String() == subnet && c.Gateway.String() == gateway && c.IPRange.String() == ipRange {
			return nil
		}
	}
	return fmt.Errorf("network %s exists with IPAM %s; want subnet %s, gateway %s, ip-range %s: remove or fix the network, then restart the agent",
		networkName, describeIPAM(cfgs), subnet, gateway, ipRange)
}

func describeIPAM(cfgs []network.IPAMConfig) string {
	if len(cfgs) == 0 {
		return "(none)"
	}
	out := ""
	for i, c := range cfgs {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("subnet %s gateway %s ip-range %s", c.Subnet, c.Gateway, c.IPRange)
	}
	return out
}

// EnsureImage pulls image unless it's already local.
func (d *Docker) EnsureImage(ctx context.Context, image string) error {
	if _, err := d.cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", image, err)
	}
	resp, err := d.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	// Wait drains the progress stream, and the pull only completes once
	// it's consumed.
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	return nil
}

// RunInstance creates and starts the instance's container on klite0 with a
// dynamic IP. Restart policy is "no": the agent owns restarts (ADR 0011).
func (d *Docker) RunInstance(ctx context.Context, inst *klitev1.Instance, node string) (string, error) {
	name := ContainerName(node, inst.GetMeta().GetName())
	opts, err := createOptions(inst, node, name)
	if err != nil {
		return "", err
	}
	created, err := d.cli.ContainerCreate(ctx, opts)
	if cerrdefs.IsConflict(err) {
		// A previous agent life left a container holding the name. The
		// reconciler adopts matching survivors before calling RunInstance,
		// so whatever sits here is stale: replace it.
		if _, rerr := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			return "", fmt.Errorf("remove stale container %s: %w", name, rerr)
		}
		created, err = d.cli.ContainerCreate(ctx, opts)
	}
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", name, err)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start container %s: %w", name, err)
	}
	return created.ID, nil
}

func createOptions(inst *klitev1.Instance, node, name string) (client.ContainerCreateOptions, error) {
	spec := inst.GetSpec()
	c := spec.GetContainer()
	ports, err := portSet(c.GetPorts())
	if err != nil {
		return client.ContainerCreateOptions{}, err
	}
	res, err := resources(c.GetResources())
	if err != nil {
		return client.ContainerCreateOptions{}, err
	}
	return client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        c.GetImage(),
			Entrypoint:   c.GetCommand(),
			Cmd:          c.GetArgs(),
			Env:          envList(c.GetEnv()),
			ExposedPorts: ports,
			Labels: map[string]string{
				LabelRole:         RoleWorkload,
				LabelNode:         node,
				LabelWorkload:     spec.GetWorkload(),
				LabelInstance:     inst.GetMeta().GetName(),
				LabelInstanceUID:  inst.GetMeta().GetUid(),
				LabelTemplateHash: spec.GetTemplateHash(),
			},
		},
		HostConfig: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
			Resources:     res,
		},
		// No IPAMConfig means a dynamic address from klite0's ip-range.
		// DNS options arrive in M4 with the infra pod (ADR 0008).
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{networkName: {}},
		},
	}, nil
}

func envList(vars []*klitev1.EnvVar) []string {
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.GetName()+"="+v.GetValue())
	}
	return out
}

func portSet(ports []*klitev1.Port) (network.PortSet, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	set := make(network.PortSet, len(ports))
	for _, p := range ports {
		port, err := network.ParsePort(strconv.Itoa(int(p.GetContainerPort())) + "/tcp")
		if err != nil {
			return nil, fmt.Errorf("container port %d: %w", p.GetContainerPort(), err)
		}
		set[port] = struct{}{}
	}
	return set, nil
}

// resources maps spec limits onto cgroup limits. They never influence
// placement (ADR 0012).
func resources(r *klitev1.Resources) (container.Resources, error) {
	var out container.Resources
	if s := r.GetCpus(); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0 {
			return out, fmt.Errorf("cpus %q is not a non-negative number", s)
		}
		out.NanoCPUs = int64(f * 1e9)
	}
	if s := r.GetMemory(); s != "" {
		b, err := units.RAMInBytes(s)
		if err != nil {
			return out, fmt.Errorf("memory %q: %w", s, err)
		}
		out.Memory = b
	}
	return out, nil
}

// StopInstance stops a container the way docker stop does: SIGTERM, then
// SIGKILL once gracePeriod runs out. Stopping a missing container succeeds.
func (d *Docker) StopInstance(ctx context.Context, id string, gracePeriod time.Duration) error {
	timeout := new(int(gracePeriod / time.Second))
	if _, err := d.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: timeout}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

// RemoveInstance force-removes a container, treating already-gone as success.
func (d *Docker) RemoveInstance(ctx context.Context, id string) error {
	if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove container: %w", err)
	}
	return nil
}

// ListInstances returns the node's workload containers, running or not.
func (d *Docker) ListInstances(ctx context.Context, node string) ([]RunningInstance, error) {
	list, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", LabelRole+"="+RoleWorkload).
			Add("label", LabelNode+"="+node),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]RunningInstance, 0, len(list.Items))
	for _, c := range list.Items {
		ri := RunningInstance{
			ContainerID:  c.ID,
			InstanceName: c.Labels[LabelInstance],
			InstanceUID:  c.Labels[LabelInstanceUID],
			TemplateHash: c.Labels[LabelTemplateHash],
			State:        string(c.State),
			IP:           summaryIP(&c),
		}
		if ri.State != StateRunning {
			ri.ExitCode = d.exitCode(ctx, c.ID)
		}
		out = append(out, ri)
	}
	return out, nil
}

func summaryIP(c *container.Summary) string {
	if c.NetworkSettings == nil {
		return ""
	}
	if ep := c.NetworkSettings.Networks[networkName]; ep != nil && ep.IPAddress.IsValid() {
		return ep.IPAddress.String()
	}
	return ""
}

// exitCode inspects a stopped container, returning zero when the container
// vanished mid-look.
func (d *Docker) exitCode(ctx context.Context, id string) int {
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || insp.Container.State == nil {
		return 0
	}
	return insp.Container.State.ExitCode
}

// InspectIP returns the container's klite0 address, or "" before one is
// assigned.
func (d *Docker) InspectIP(ctx context.Context, id string) (string, error) {
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if ns := insp.Container.NetworkSettings; ns != nil {
		if ep := ns.Networks[networkName]; ep != nil && ep.IPAddress.IsValid() {
			return ep.IPAddress.String(), nil
		}
	}
	return "", nil
}

// WatchEvents streams start and die events for the node's containers, the
// events half of the events-plus-resync informer pattern
// (research/docker-go-sdk.md). The channel closes when the Docker stream
// fails. The caller then resyncs and subscribes again.
func (d *Docker) WatchEvents(ctx context.Context, node string) (<-chan Event, error) {
	res := d.cli.Events(ctx, client.EventsListOptions{
		Filters: make(client.Filters).
			Add("type", "container").
			Add("event", ActionDie, ActionStart).
			Add("label", LabelNode+"="+node),
	})
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-res.Err:
				return
			case m := <-res.Messages:
				select {
				case out <- toEvent(&m):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func toEvent(m *events.Message) Event {
	attrs := m.Actor.Attributes
	// The daemon merges container labels into every event's attributes and
	// carries the exit code on die.
	code, _ := strconv.Atoi(attrs["exitCode"])
	return Event{
		Action:       string(m.Action),
		ContainerID:  m.Actor.ID,
		InstanceName: attrs[LabelInstance],
		InstanceUID:  attrs[LabelInstanceUID],
		ExitCode:     code,
	}
}

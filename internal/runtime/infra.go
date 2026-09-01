package runtime

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// RunInfra creates and starts one infra-pod container from spec, replacing
// whatever holds the name.
func (d *Docker) RunInfra(ctx context.Context, spec *InfraContainer) (string, error) {
	opts, err := infraCreateOptions(spec)
	if err != nil {
		return "", err
	}
	return d.createAndStart(ctx, spec.Name, opts)
}

func infraCreateOptions(spec *InfraContainer) (client.ContainerCreateOptions, error) {
	exposed, bindings, err := portBindings(spec.Ports)
	if err != nil {
		return client.ContainerCreateOptions{}, err
	}
	host := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		CapAdd:        spec.CapAdd,
		ExtraHosts:    spec.ExtraHosts,
		Binds:         spec.Binds,
		PortBindings:  bindings,
	}
	networking := &network.NetworkingConfig{}
	switch {
	case spec.JoinNetns != "":
		host.NetworkMode = container.NetworkMode("container:" + spec.JoinNetns)
		networking = nil
	case spec.StaticIP != "":
		ip, err := netip.ParseAddr(spec.StaticIP)
		if err != nil {
			return client.ContainerCreateOptions{}, fmt.Errorf("static ip %q: %w", spec.StaticIP, err)
		}
		networking.EndpointsConfig = map[string]*network.EndpointSettings{
			networkName: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: ip}},
		}
	default:
		networking.EndpointsConfig = map[string]*network.EndpointSettings{networkName: {}}
	}
	return client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:        spec.Image,
			Cmd:          spec.Cmd,
			Env:          spec.Env,
			Labels:       spec.Labels,
			ExposedPorts: exposed,
		},
		HostConfig:       host,
		NetworkingConfig: networking,
	}, nil
}

// RunOneShot runs spec to completion and removes the container, returning an
// error that carries the container's output when it exits nonzero. The infra
// pod's lockdown pass rides this: a helper joins the donor netns, applies its
// rules, and vanishes.
func (d *Docker) RunOneShot(ctx context.Context, spec *InfraContainer) error {
	id, err := d.RunInfra(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = d.RemoveInstance(context.WithoutCancel(ctx), id) }()
	wait := d.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case werr := <-wait.Error:
		return fmt.Errorf("wait for %s: %w", spec.Name, werr)
	case res := <-wait.Result:
		// The daemon can report an error with a zero status code. For a
		// lockdown helper a swallowed failure means rules silently not
		// applied, so an error here never passes as success.
		if res.Error != nil {
			return fmt.Errorf("wait for %s: %s", spec.Name, res.Error.Message)
		}
		if res.StatusCode == 0 {
			return nil
		}
		return fmt.Errorf("%s exited %d: %s", spec.Name, res.StatusCode, d.tailLogs(ctx, id))
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tailLogs grabs a one-shot container's last lines for error messages,
// best-effort.
func (d *Docker) tailLogs(ctx context.Context, id string) string {
	rc, err := d.Logs(ctx, id, false, 10)
	if err != nil {
		return "(logs unavailable: " + err.Error() + ")"
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil || len(b) == 0 {
		return "(no output)"
	}
	return strings.TrimSpace(string(b))
}

func portBindings(ports map[string]string) (network.PortSet, network.PortMap, error) {
	if len(ports) == 0 {
		return nil, nil, nil
	}
	exposed := make(network.PortSet, len(ports))
	bindings := make(network.PortMap, len(ports))
	for portProto, hostAddr := range ports {
		port, err := network.ParsePort(portProto)
		if err != nil {
			return nil, nil, fmt.Errorf("port %q: %w", portProto, err)
		}
		hostIP, hostPort, err := splitHostBind(hostAddr)
		if err != nil {
			return nil, nil, err
		}
		exposed[port] = struct{}{}
		bindings[port] = []network.PortBinding{{HostIP: hostIP, HostPort: hostPort}}
	}
	return exposed, bindings, nil
}

func splitHostBind(addr string) (netip.Addr, string, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("host bind %q: %w", addr, err)
	}
	return ap.Addr(), fmt.Sprintf("%d", ap.Port()), nil
}

// maxContainerFileBytes caps ReadContainerFile. The method exists to fetch
// small config files (the donor's /etc/hosts), and an over-cap read errors
// rather than handing back a silent truncation.
const maxContainerFileBytes = 1 << 20

// ReadContainerFile pulls one file out of a container through the archive
// API, which works on scratch images where exec can't.
func (d *Docker) ReadContainerFile(ctx context.Context, name, path string) ([]byte, error) {
	res, err := d.cli.CopyFromContainer(ctx, name, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, fmt.Errorf("copy %s from %s: %w", path, name, err)
	}
	defer res.Content.Close()
	b, err := archivedFile(res.Content)
	if err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", path, name, err)
	}
	return b, nil
}

// archivedFile extracts the single file a CopyFromContainer archive carries:
// its first entry, which the daemon names after the requested path. A
// directory or symlink there fails loudly, where skipping ahead to some
// deeper regular entry would quietly return another file's bytes.
func archivedFile(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err != nil {
		// io.EOF here means an empty archive.
		return nil, err
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("%s is not a regular file", hdr.Name)
	}
	if hdr.Size > maxContainerFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte cap", hdr.Name, hdr.Size, maxContainerFileBytes)
	}
	return io.ReadAll(io.LimitReader(tr, maxContainerFileBytes))
}

// ListInfra lists infra containers carrying the given role label.
func (d *Docker) ListInfra(ctx context.Context, role string) ([]InfraInfo, error) {
	list, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelRole+"="+role),
	})
	if err != nil {
		return nil, fmt.Errorf("list %s containers: %w", role, err)
	}
	out := make([]InfraInfo, 0, len(list.Items))
	for _, c := range list.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, InfraInfo{
			ID:      c.ID,
			Name:    name,
			Node:    c.Labels[LabelNode],
			Cluster: c.Labels[LabelCluster],
			IP:      summaryIP(&c),
		})
	}
	return out, nil
}

// InspectInfra reports a named container's state, or nil when the name is
// free.
func (d *Docker) InspectInfra(ctx context.Context, name string) (*InfraStatus, error) {
	insp, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", name, err)
	}
	c := insp.Container
	st := &InfraStatus{ID: c.ID}
	if c.Config != nil {
		st.ConfigHash = c.Config.Labels[LabelConfigHash]
	}
	if c.State != nil {
		st.Running = c.State.Running
		if t, err := time.Parse(time.RFC3339Nano, c.State.StartedAt); err == nil {
			st.StartedAt = t
		}
	}
	return st, nil
}

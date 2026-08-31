# Docker Go SDK notes for M2 (klite-agent Runtime)

These notes come from a shallow clone of moby/moby master (Aug 2026) and the tagged module sources for client v0.5.1 and api v1.55.0. File references below point at those tags.

## Import path

Use `github.com/moby/moby/client` (v0.5.1, tagged 2026-07-27) with types from `github.com/moby/moby/api` (v1.55.0). The legacy `github.com/docker/docker` module froze at `v28.5.2+incompatible` in November 2025 and hasn't been tagged since, so new code shouldn't start there. The catch is that the new client is still v0.x and breaks between minors. `NewClientWithOpts` already carries a "will be removed in the next release" notice (client.go:171), so pin both modules and treat upgrades as API reviews. `stdcopy` moved to `github.com/moby/moby/api/pkg/stdcopy`, and error classification runs through `github.com/containerd/errdefs` (`cerrdefs.IsConflict`, `cerrdefs.IsNotFound`).

## Client setup

```go
opts := []client.Opt{client.FromEnv}
if os.Getenv("DOCKER_HOST") == "" {
	home, _ := os.UserHomeDir()
	for _, p := range []string{"/var/run/docker.sock",
		filepath.Join(home, ".colima/default/docker.sock"),
		filepath.Join(home, ".docker/run/docker.sock")} {
		if _, err := os.Stat(p); err == nil {
			opts = append(opts, client.WithHost("unix://"+p))
			break
		}
	}
}
cli, err := client.New(opts...) // API version negotiation is on by default
```

`client.New` negotiates the API version on the first request (client.go:184), and `WithAPIVersionNegotiation()` still compiles but is a documented no-op (client_options.go:391). `FromEnv` reads DOCKER_HOST, DOCKER_API_VERSION, DOCKER_CERT_PATH and DOCKER_TLS_VERIFY, and nothing else (client_options.go:90). The SDK never reads the CLI's context store (a grep of the module turns up no such code). So on a colima host, where the CLI works through the `colima` context, the SDK dials `unix:///var/run/docker.sock` (`DefaultDockerHost`, client_unix.go:13) and fails. The socket probe above is there to catch exactly that.

## Create, start, stop

The api module moved addresses to `netip` types and ports to its own `network.Port`, so bad addresses now fail at config parse time instead of at the API call.

```go
cfg := &container.Config{
	Image:  "ghcr.io/klite/echo:1",
	Cmd:    []string{"/echo", "--port=8080"},
	Env:    []string{"KLITE_INSTANCE=" + name},
	Labels: map[string]string{"klite.instance": name, "klite.node": node},
	ExposedPorts: network.PortSet{network.MustParsePort("8080/tcp"): {}},
}
hc := &container.HostConfig{
	DNS:           []netip.Addr{netip.MustParseAddr("10.88.0.2")},
	DNSSearch:     []string{"klite.local"},
	CapAdd:        []string{"NET_ADMIN"},
	RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}, // "no"
	Resources:     container.Resources{NanoCPUs: 500_000_000, Memory: 256 << 20},
}
netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
	// Pins a static IP. Leave IPAMConfig nil for a dynamic address.
	"klite-net": {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.88.0.10")}},
}}
created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
	Name: "klite-" + name, Config: cfg, HostConfig: hc, NetworkingConfig: netCfg,
})
_, err = cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
_, err = cli.ContainerStop(ctx, created.ID, client.ContainerStopOptions{Timeout: new(10)})
_, err = cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
```

A static IPv4 needs a user-defined network whose subnet contains it. For Envoy to join an instance's netns, set `hc.NetworkMode = container.NetworkMode("container:klite-" + name)`, and the string form matters (`containerID()` requires the `container:` prefix, api hostconfig.go:482). The daemon rejects hostname, links, DNS, extra hosts, port bindings, `PublishAllPorts`, exposed ports and a MAC address on the joining container (daemon/internal/runconfig/hostconfig.go:7, container_routes.go:857). The Envoy container therefore carries none of those and no `NetworkingConfig`, since it inherits all networking from its target.

## List and logs

```go
list, err := cli.ContainerList(ctx, client.ContainerListOptions{
	All:     true,
	Filters: make(client.Filters).Add("label", "klite.node="+node),
})
rc, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
	ShowStdout: true, ShowStderr: true, Follow: true, Tail: "100",
})
defer rc.Close()
_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, rc) // demux stdout/stderr
```

`Filters` is now a plain map type in the client package with a chainable `Add` (filters.go:17). Logs come back multiplexed whenever the container runs without a TTY (ours always do), so everything goes through `stdcopy.StdCopy`.

## Watching exits

The agent can watch exits two ways. `ContainerWait` returns result and error channels and misses nothing, but costs one goroutine plus one long-lived HTTP request per container. Its own source warns that proxies cut the response stream mid-wait (container_wait.go:71). `Events` covers every container on the node with one stream:

```go
ev := cli.Events(ctx, client.EventsListOptions{
	Filters: make(client.Filters).Add("type", "container").Add("event", "die").Add("label", "klite.node="+node),
})
for {
	select {
	case m := <-ev.Messages:
		code := m.Actor.Attributes["exitCode"] // string, e.g. "137"
	case err := <-ev.Err:
		// rebuild the stream, then resync via ContainerList
	}
}
```

Label filters work here because the daemon merges container labels into every event's `Actor.Attributes` (daemon/events.go:26), and the `die` event carries the exit code there (daemon/monitor.go:119). For the reconcile loop, use the informer pattern: Events plus a full `ContainerList` resync at startup and after every stream error. The client leaves reopening a dropped stream to the caller (system_events.go:31), so the resync is what makes it correct. Per-container `ContainerWait` then adds nothing.

## Networks

```go
_, err := cli.NetworkCreate(ctx, "klite-net", client.NetworkCreateOptions{
	Driver: "bridge",
	IPAM: &network.IPAM{Config: []network.IPAMConfig{{
		Subnet:  netip.MustParsePrefix("10.88.0.0/16"),
		Gateway: netip.MustParseAddr("10.88.0.1"),
		IPRange: netip.MustParsePrefix("10.88.128.0/17"),
	}}},
	Labels: map[string]string{"klite.managed": "true"},
})
got, err := cli.NetworkInspect(ctx, "klite-net", client.NetworkInspectOptions{})
```

Keep static assignments outside `IPRange`. Docker hands out dynamic addresses only from that pool while static IPs may use the whole subnet, so the two can't collide. Validation at agent start compares `got.Network.IPAM.Config` against the expected subnet and falls back to creating the network when `cerrdefs.IsNotFound(err)` says it's missing.

## Exec (debugging)

The old `ContainerExecCreate` flow is now `ExecCreate`, `ExecAttach`, `ExecStart` (container_exec.go). `ExecAttach` itself posts `/exec/{id}/start` over a hijacked connection, so don't also call `ExecStart`, which exists for detached runs.

```go
ex, _ := cli.ExecCreate(ctx, ctr, client.ExecCreateOptions{Cmd: []string{"ip", "route"}, AttachStdout: true, AttachStderr: true})
att, _ := cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
defer att.Close()
_, _ = stdcopy.StdCopy(os.Stdout, os.Stderr, att.Reader)
```

## Pitfalls

- Don't pin the API version. Negotiation handles colima's engine (server 29.5.2, client max API 1.55) on its own, and a stray `DOCKER_API_VERSION` in the environment silently disables it because `FromEnv` includes `WithAPIVersionFromEnv`.
- Drain the pull stream. `ImagePull` returns a reader the daemon writes progress into, and the pull only completes once you consume it. v0.5.x added `resp.Wait(ctx)`, which also surfaces in-stream errors that a bare `io.Copy(io.Discard, resp)` would hide (image_pull.go:31, internal/jsonmessages.go:86).
- Handle name conflicts instead of avoiding them. With deterministic names (`klite-<instance>`), a crashed agent leaves containers behind and re-creating returns 409 ("is already in use by container", daemon/errors.go:63). Detect it with `cerrdefs.IsConflict(err)`, inspect the survivor, adopt it when its labels match the desired spec, and otherwise remove with Force and recreate.
- Expect churn from the v0.x client. Every method now takes an options struct and `NewClientWithOpts` dies in the next release, so keep moby imports inside the one package that implements our Runtime interface.

## M2 checklist

- Pin `github.com/moby/moby/client@v0.5.1`, `github.com/moby/moby/api@v1.55.0` and `github.com/containerd/errdefs` in go.mod, then build the client with `FromEnv` plus the colima socket probe.
- Ensure `klite-net` exists at agent start (inspect, create on not-found, validate the subnet).
- Create instances with klite labels, a static IP, NET_ADMIN, restart policy "no" and resource limits.
- Run the reconcile loop on label-filtered `die` events with a `ContainerList` resync on every (re)connect.
- Pull images with `Wait(ctx)` before create, and resolve create conflicts by adopt-or-replace.
- Keep an `ExecCreate`/`ExecAttach` helper behind a debug flag.

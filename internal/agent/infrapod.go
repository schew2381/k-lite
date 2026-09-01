package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

// The infra pod is two containers (ADR 0008): klite-net owns the netns and
// Envoy joins it. Everything netns-scoped (static IP, NET_ADMIN, the
// host-gateway hosts entry, published ports) rides the donor, because Docker
// rejects those options alongside --network container:.
const (
	kliteNetImage = "klite-net:dev"
	envoyImage    = "envoyproxy/envoy:v1.31.5"

	// Host ports are derived from the node index so several agents share
	// one machine without colliding: klite-net admin at 19000+i, Envoy
	// admin at 19500+i, both loopback-only.
	netAdminPortBase   = 19000
	envoyAdminPortBase = 19500

	defaultXDSPort = 7443

	bootstrapMount = "/etc/klite/envoy-bootstrap.yaml"
)

// envoyBootstrapTemplate is the proven shape from hack/spike-envoy: ADS over
// one gRPC stream, explicit HTTP/2 on the xDS cluster, node.id equal to the
// snapshot-cache key. host.docker.internal comes from the donor's /etc/hosts.
const envoyBootstrapTemplate = `node:
  id: %s
  cluster: klite
admin:
  address:
    socket_address:
      address: 0.0.0.0
      port_value: 9901
dynamic_resources:
  ads_config:
    api_type: GRPC
    transport_api_version: V3
    grpc_services:
      - envoy_grpc:
          cluster_name: xds
    set_node_on_first_message_only: true
  cds_config:
    resource_api_version: V3
    ads: {}
  lds_config:
    resource_api_version: V3
    ads: {}
static_resources:
  clusters:
    - name: xds
      type: LOGICAL_DNS
      dns_lookup_family: V4_ONLY
      connect_timeout: 1s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: xds
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: host.docker.internal
                      port_value: %d
`

func (a *Agent) netContainerName() string   { return "klite." + a.node + ".net" }
func (a *Agent) envoyContainerName() string { return "klite." + a.node + ".envoy" }

func (a *Agent) netAdminPort() int {
	return netAdminPortBase + int(a.netBootstrap().GetNodeIndex())
}

func (a *Agent) netBootstrap() *klitev1.NetBootstrap {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.net
}

// ensureInfraPod converges the node's two infra containers on their desired
// shape: adopt when the config hash matches and the container runs, recreate
// on any drift. Envoy is recreated whenever the donor is, since a donor
// restart tears down the shared netns under it.
func (a *Agent) ensureInfraPod(ctx context.Context) error {
	nb := a.netBootstrap()
	if nb == nil {
		return nil
	}
	donor, err := a.ensureNetContainer(ctx, nb)
	if err != nil {
		return fmt.Errorf("klite-net: %w", err)
	}
	if err := a.ensureEnvoyContainer(ctx, donor); err != nil {
		return fmt.Errorf("envoy: %w", err)
	}
	return nil
}

func (a *Agent) netContainerSpec(nb *klitev1.NetBootstrap) *runtime.InfraContainer {
	idx := int(nb.GetNodeIndex())
	spec := &runtime.InfraContainer{
		Name:       a.netContainerName(),
		Image:      kliteNetImage,
		StaticIP:   nb.GetKliteNetIp(),
		CapAdd:     []string{"NET_ADMIN"},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Ports: map[string]string{
			"9090/tcp": fmt.Sprintf("127.0.0.1:%d", netAdminPortBase+idx),
			// Envoy's admin listens in the shared netns, so its
			// published port rides the donor too.
			"9901/tcp": fmt.Sprintf("127.0.0.1:%d", envoyAdminPortBase+idx),
		},
		Labels: map[string]string{
			runtime.LabelRole: runtime.RoleNet,
			runtime.LabelNode: a.node,
		},
	}
	spec.Labels[runtime.LabelConfigHash] = configHash(spec)
	return spec
}

func (a *Agent) ensureNetContainer(ctx context.Context, nb *klitev1.NetBootstrap) (*runtime.InfraStatus, error) {
	spec := a.netContainerSpec(nb)
	st, err := a.rt.InspectInfra(ctx, spec.Name)
	if err != nil {
		return nil, err
	}
	if st != nil && st.Running && st.ConfigHash == spec.Labels[runtime.LabelConfigHash] {
		return st, nil
	}
	if err := a.evictNetSquatters(ctx, spec.StaticIP); err != nil {
		return nil, err
	}
	if err := a.recreateInfra(ctx, spec, st); err != nil {
		return nil, err
	}
	return a.rt.InspectInfra(ctx, spec.Name)
}

// evictNetSquatters removes donors from previous cluster lives that still
// hold this node's assigned address (index reshuffles across fresh etcd runs
// leave these behind), along with their orphaned Envoys. A live sibling
// donor never conflicts: indexes are unique within one cluster.
func (a *Agent) evictNetSquatters(ctx context.Context, ip string) error {
	donors, err := a.rt.ListInfra(ctx, runtime.RoleNet)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range donors {
		if d.Name == a.netContainerName() || d.IP != ip {
			continue
		}
		slog.Warn("removing stale donor holding our address", "name", d.Name, "ip", ip)
		errs = append(errs, a.rt.RemoveInstance(ctx, d.ID))
		if d.Node != "" && d.Node != a.node {
			errs = append(errs, a.rt.RemoveInstance(ctx, "klite."+d.Node+".envoy"))
		}
	}
	return errors.Join(errs...)
}

func (a *Agent) ensureEnvoyContainer(ctx context.Context, donor *runtime.InfraStatus) error {
	if donor == nil || !donor.Running {
		return fmt.Errorf("donor %s is not running", a.netContainerName())
	}
	bootstrap, path, err := a.writeEnvoyBootstrap()
	if err != nil {
		return err
	}
	spec := &runtime.InfraContainer{
		Name:      a.envoyContainerName(),
		Image:     envoyImage,
		Cmd:       []string{"-c", bootstrapMount},
		JoinNetns: a.netContainerName(),
		Binds:     []string{path + ":" + bootstrapMount + ":ro"},
		Labels: map[string]string{
			runtime.LabelRole: runtime.RoleEnvoy,
			runtime.LabelNode: a.node,
		},
	}
	// The donor ID folds into the hash so a recreated donor forces a fresh
	// Envoy into the new netns.
	spec.Labels[runtime.LabelConfigHash] = configHash(spec, donor.ID, bootstrap)
	st, err := a.rt.InspectInfra(ctx, spec.Name)
	if err != nil {
		return err
	}
	adoptable := st != nil && st.Running &&
		st.ConfigHash == spec.Labels[runtime.LabelConfigHash] &&
		!donor.StartedAt.After(st.StartedAt)
	if adoptable {
		return nil
	}
	return a.recreateInfra(ctx, spec, st)
}

func (a *Agent) recreateInfra(ctx context.Context, spec *runtime.InfraContainer, prev *runtime.InfraStatus) error {
	if err := a.rt.EnsureImage(ctx, spec.Image); err != nil {
		return err
	}
	if prev != nil {
		if err := a.rt.RemoveInstance(ctx, prev.ID); err != nil {
			return err
		}
	}
	if _, err := a.rt.RunInfra(ctx, spec); err != nil {
		return err
	}
	slog.Info("infra container started", "name", spec.Name, "image", spec.Image)
	return nil
}

// writeEnvoyBootstrap renders the node's bootstrap and persists it under the
// agent's state dir, rewriting only on content change so the file's mtime
// stays meaningful.
func (a *Agent) writeEnvoyBootstrap() (content, path string, err error) {
	content = fmt.Sprintf(envoyBootstrapTemplate, a.node, a.xdsPort())
	dir := a.stateDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve home: %w", err)
		}
		dir = filepath.Join(home, ".klite", "agent")
	}
	dir = filepath.Join(dir, a.node)
	path = filepath.Join(dir, "envoy-bootstrap.yaml")
	if got, err := os.ReadFile(path); err == nil && string(got) == content {
		return content, path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- container-readable path
		return "", "", err
	}
	// #nosec G306 -- the envoy container's user must be able to read it
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", "", err
	}
	return content, path, nil
}

// xdsPort is the port half of the klited address this agent dialed; the
// in-container Envoy reaches the same server via host.docker.internal.
func (a *Agent) xdsPort() int {
	if _, port, err := net.SplitHostPort(a.serverAddr); err == nil {
		if p, err := strconv.Atoi(port); err == nil && p > 0 {
			return p
		}
	}
	return defaultXDSPort
}

// configHash fingerprints a container spec plus any extra inputs. JSON
// marshaling is deterministic (map keys are sorted), so equal specs hash
// equal.
func configHash(spec *runtime.InfraContainer, extra ...string) string {
	h := sha256.New()
	b, _ := json.Marshal(spec)
	h.Write(b)
	for _, e := range extra {
		h.Write([]byte{0})
		h.Write([]byte(e))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

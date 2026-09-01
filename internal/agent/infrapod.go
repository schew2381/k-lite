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
	"strings"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

// The infra pod is two containers (ADR 0008): klite-net owns the netns and
// Envoy joins it. Everything netns-scoped (static IP, NET_ADMIN, the
// host-gateway hosts entry, published ports) rides the donor, because Docker
// rejects those options alongside --network container:.
const (
	// defaultKliteNetImage is what donors run when NetBootstrap carries no
	// net_image: dev clusters, where `make net-image` loads this tag into
	// the local daemon. klited's --net-image overrides it cluster-wide
	// (ADR 0038).
	defaultKliteNetImage = "klite-net:dev"
	envoyImage           = "envoyproxy/envoy:v1.31.5"

	// Host ports are derived from the node index so several agents share
	// one machine without colliding: klite-net admin at base+i, Envoy
	// admin at base+i, both loopback-only. A second deliberate cluster
	// moves the bases through klited's flags (NetBootstrap).
	defaultNetAdminPortBase   = 19000
	defaultEnvoyAdminPortBase = 19500

	defaultXDSPort = 7443

	bootstrapMount = "/etc/klite/envoy-bootstrap.yaml"
	// tlsMount is where Envoy sees the node identity (join.go). The
	// ingress listeners read the same files, so the whole directory
	// mounts, not individual certs.
	tlsMount = "/etc/klite/tls"
)

// envoyBootstrapHeader is the proven shape from hack/spike-envoy: ADS over
// one gRPC stream, explicit HTTP/2 on the xDS cluster, node.id equal to the
// snapshot-cache key. host.docker.internal comes from the donor's /etc/hosts.
const envoyBootstrapHeader = `node:
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
      type: STRICT_DNS
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
`

const envoyBootstrapEndpoint = `              - endpoint:
                  address:
                    socket_address:
                      address: %s
                      port_value: %d
`

// envoyBootstrapTLS rides the node identity to the xDS stream (ADR 0013).
// Both protocol-version pins matter: klited requires 1.3 for Go peers, and
// Envoy's upstream default MAX is 1.2, which would kill the handshake.
const envoyBootstrapTLS = `      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
          common_tls_context:
            tls_params:
              tls_minimum_protocol_version: TLSv1_3
              tls_maximum_protocol_version: TLSv1_3
            tls_certificates:
              - certificate_chain:
                  filename: ` + tlsMount + `/node.crt
                private_key:
                  filename: ` + tlsMount + `/node.key
            validation_context:
              trusted_ca:
                filename: ` + tlsMount + `/ca.crt
`

func (a *Agent) netContainerName() string   { return "klite." + a.node + ".net" }
func (a *Agent) envoyContainerName() string { return "klite." + a.node + ".envoy" }

func (a *Agent) netAdminPort() int {
	nb := a.netBootstrap()
	base := int(nb.GetNetAdminPortBase())
	if base == 0 {
		base = defaultNetAdminPortBase
	}
	return base + int(nb.GetNodeIndex())
}

func (a *Agent) envoyAdminPort() int {
	nb := a.netBootstrap()
	base := int(nb.GetEnvoyAdminPortBase())
	if base == 0 {
		base = defaultEnvoyAdminPortBase
	}
	return base + int(nb.GetNodeIndex())
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
	if err := a.ensureEnvoyContainer(ctx, nb, donor); err != nil {
		return fmt.Errorf("envoy: %w", err)
	}
	a.ensureLockdown(ctx, nb, donor)
	return nil
}

// infraLabels builds the label set every infra container carries, with role,
// node, and the owning cluster so a shared daemon's clusters stay out of
// each other's way.
func (a *Agent) infraLabels(nb *klitev1.NetBootstrap, role string) map[string]string {
	labels := map[string]string{
		runtime.LabelRole: role,
		runtime.LabelNode: a.node,
	}
	if id := nb.GetClusterId(); id != "" {
		labels[runtime.LabelCluster] = id
	}
	return labels
}

func (a *Agent) netContainerSpec(nb *klitev1.NetBootstrap) *runtime.InfraContainer {
	spec := &runtime.InfraContainer{
		Name:       a.netContainerName(),
		Image:      kliteNetImage(nb),
		StaticIP:   nb.GetKliteNetIp(),
		CapAdd:     []string{"NET_ADMIN"},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Ports: map[string]string{
			"9090/tcp": fmt.Sprintf("127.0.0.1:%d", a.netAdminPort()),
			// Envoy's admin listens in the shared netns, so its
			// published port rides the donor too.
			"9901/tcp": fmt.Sprintf("127.0.0.1:%d", a.envoyAdminPort()),
		},
		Labels: a.infraLabels(nb, runtime.RoleNet),
	}
	// The whole ingress slice publishes at creation (Docker can't add
	// ports to a running container), on every interface, because remote
	// machines dial these (ADR 0034). Envoy binds them one listener per
	// local endpoint as allocations arrive. The map feeds the config hash,
	// so donors from before this range recreate exactly once.
	lo, hi := ingressPortRange(nb)
	for p := lo; p < hi; p++ {
		spec.Ports[fmt.Sprintf("%d/tcp", p)] = fmt.Sprintf("0.0.0.0:%d", p)
	}
	spec.Labels[runtime.LabelConfigHash] = configHash(spec)
	return spec
}

// kliteNetImage picks the donor's image: the server's cluster-wide pin when
// NetBootstrap carries one, the compiled-in dev tag otherwise. The image is
// part of the container spec, so a change moves the donor's config hash and
// recreates it exactly once.
func kliteNetImage(nb *klitev1.NetBootstrap) string {
	if img := nb.GetNetImage(); img != "" {
		return img
	}
	return defaultKliteNetImage
}

// ingressPortRange is the node's half-open published slice [lo, hi),
// derived from NetBootstrap exactly like the allocator derives it server
// side. A pre-M9 server sends no base, and the slice stays empty.
func ingressPortRange(nb *klitev1.NetBootstrap) (lo, hi int) {
	base, per, idx := int(nb.GetIngressPortBase()), int(nb.GetIngressPortsPerNode()), int(nb.GetNodeIndex())
	if base <= 0 || per <= 0 || idx < 1 {
		return 0, 0
	}
	lo = base + per*(idx-1)
	hi = lo + per
	if hi > 65536 {
		return 0, 0
	}
	return lo, hi
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
	if err := a.evictNetSquatters(ctx, spec.StaticIP, nb.GetClusterId()); err != nil {
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
// donor never conflicts, indexes being unique within one cluster — so a
// squatter from a DIFFERENT cluster means two clusters share an IP base, and
// deleting a healthy stranger would be sabotage. That case stops the agent
// loudly instead.
func (a *Agent) evictNetSquatters(ctx context.Context, ip, clusterID string) error {
	donors, err := a.rt.ListInfra(ctx, runtime.RoleNet)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range donors {
		if d.Name == a.netContainerName() || d.IP != ip {
			continue
		}
		if d.Cluster != "" && d.Cluster != clusterID {
			return fmt.Errorf("container %s from cluster %s holds our address %s; refusing to remove another cluster's donor — "+
				"give one cluster its own --infra-ip-base and port bases, or remove %s by hand", d.Name, d.Cluster, ip, d.Name)
		}
		slog.Warn("removing stale donor holding our address", "name", d.Name, "ip", ip, "cluster", d.Cluster)
		errs = append(errs, a.rt.RemoveInstance(ctx, d.ID))
		if d.Node != "" && d.Node != a.node {
			errs = append(errs, a.rt.RemoveInstance(ctx, "klite."+d.Node+".envoy"))
		}
	}
	return errors.Join(errs...)
}

func (a *Agent) ensureEnvoyContainer(ctx context.Context, nb *klitev1.NetBootstrap, donor *runtime.InfraStatus) error {
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
		Labels:    a.infraLabels(nb, runtime.RoleEnvoy),
	}
	if a.tlsDir != "" {
		spec.Binds = append(spec.Binds, a.tlsDir+":"+tlsMount+":ro")
		// The identity files are 0600 for the host user; the image's
		// default envoy user could not read them through the mount.
		spec.Env = []string{"ENVOY_UID=0"}
	}
	// The donor ID folds into the hash so a recreated donor forces a fresh
	// Envoy into the new netns. The identity fingerprint folds in so a
	// re-join (new certs at the same paths, e.g. the pre-M9 self-heal)
	// forces one that re-reads them, because Envoy never hot-reloads
	// file-based TLS material.
	spec.Labels[runtime.LabelConfigHash] = configHash(spec, donor.ID, bootstrap, a.identityFingerprint())
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
	content = a.renderEnvoyBootstrap()
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

// renderEnvoyBootstrap builds the per-node bootstrap: one lb_endpoint per
// klited so the ADS stream fails over with the agent (M8), plus the mTLS
// transport socket when the node holds an identity.
func (a *Agent) renderEnvoyBootstrap() string {
	var b strings.Builder
	fmt.Fprintf(&b, envoyBootstrapHeader, a.node)
	for _, hp := range a.xdsEndpoints() {
		fmt.Fprintf(&b, envoyBootstrapEndpoint, hp.host, hp.port)
	}
	if a.tlsDir != "" {
		b.WriteString(envoyBootstrapTLS)
	}
	return b.String()
}

type hostPort struct {
	host string
	port int
}

// xdsEndpoints maps the agent's --server list into container-reachable
// addresses: loopback means "this machine", which a container spells
// host.docker.internal (the donor pins it via host-gateway).
func (a *Agent) xdsEndpoints() []hostPort {
	var out []hostPort
	seen := map[hostPort]bool{}
	for _, addr := range a.serverAddrs {
		hp := hostPort{host: "host.docker.internal", port: defaultXDSPort}
		if h, p, err := net.SplitHostPort(addr); err == nil {
			if port, perr := strconv.Atoi(p); perr == nil && port > 0 {
				hp.port = port
			}
			if h != "" && !isLoopbackHost(h) {
				hp.host = h
			}
		}
		if !seen[hp] {
			seen[hp] = true
			out = append(out, hp)
		}
	}
	if len(out) == 0 {
		out = append(out, hostPort{host: "host.docker.internal", port: defaultXDSPort})
	}
	return out
}

func isLoopbackHost(h string) bool {
	if h == "localhost" || h == "0.0.0.0" || h == "::" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// identityFingerprint digests the public identity files Envoy loads by path.
// The private key stays out of the hash input: it can't change without its
// certificate changing too.
func (a *Agent) identityFingerprint() string {
	if a.tlsDir == "" {
		return ""
	}
	h := sha256.New()
	for _, f := range []string{nodeCertFile, caCertFile} {
		b, err := os.ReadFile(filepath.Join(a.tlsDir, f))
		if err != nil {
			continue // absent now, drifts the hash when it appears
		}
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
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

// klited is the stateless control-plane server: gRPC over mTLS in front,
// etcd behind (ADR 0004, 0005, 0013).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/schew2381/k-lite/internal/ca"
	"github.com/schew2381/k-lite/internal/controller"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/leader"
	"github.com/schew2381/k-lite/internal/server"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/xds"
)

const (
	defaultEtcd = "127.0.0.1:2379,127.0.0.1:2381,127.0.0.1:2383"

	// clusterIDKey lives outside the store's /klite/v1/ prefix on purpose:
	// it's raw bookkeeping, not an object, and must not surface in watches.
	clusterIDKey = "/klite/meta/cluster-id"
)

// config carries every klited flag.
type config struct {
	listen             string
	etcdEndpoints      []string
	clusterToken       string
	stateDir           string
	extraSANs          []string
	netAdminPortBase   int
	envoyAdminPortBase int
	infraIPBase        int
	ingressPortBase    int
}

func main() {
	cfg := &config{}
	listen := flag.String("listen", "127.0.0.1:7443", "gRPC listen address (TLS, ADR 0013)")
	etcdEndpoints := flag.String("etcd", defaultEtcd, "comma-separated etcd client endpoints")
	clusterToken := flag.String("cluster-token", "dev-token", "join secret agents must present at Register")
	stateDir := flag.String("state-dir", "", "server state directory holding tls/ and the admin token (default ~/.klite/server)")
	tlsSAN := flag.String("tls-san", "", "comma-separated extra SANs for the serving cert")
	flag.IntVar(&cfg.netAdminPortBase, "net-admin-port-base", 0, "host port base for klite-net admin ports (default 19000); a second cluster on this machine must move it")
	flag.IntVar(&cfg.envoyAdminPortBase, "envoy-admin-port-base", 0, "host port base for Envoy admin ports (default 19500); a second cluster on this machine must move it")
	flag.IntVar(&cfg.infraIPBase, "infra-ip-base", 0, "last-octet base for donor addresses 10.44.0.<base+index> (default 10); a second cluster on this machine must move it")
	flag.IntVar(&cfg.ingressPortBase, "ingress-port-base", 0, "host port base for per-node cross-node ingress ranges (default 20000, 32 ports per node index); a second cluster on this machine must move it")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cfg.listen = *listen
	cfg.etcdEndpoints = strings.Split(*etcdEndpoints, ",")
	cfg.clusterToken = *clusterToken
	cfg.stateDir = *stateDir
	if cfg.stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Error("resolve home for --state-dir", "err", err)
			os.Exit(1)
		}
		cfg.stateDir = filepath.Join(home, ".klite", "server")
	}
	for san := range strings.SplitSeq(*tlsSAN, ",") {
		if s := strings.TrimSpace(san); s != "" {
			cfg.extraSANs = append(cfg.extraSANs, s)
		}
	}
	if err := run(cfg); err != nil {
		slog.Error("klited exited", "err", err)
		os.Exit(1)
	}
}

// identity is everything the TLS listener and the join flow need.
type identity struct {
	ca         *ca.CA
	serverTLS  *tls.Config
	adminToken string
}

// loadIdentity loads (or mints) the CA, issues this boot's serving cert, and
// loads (or mints) the admin token. Replicas share the state dir, so every
// klited on this machine presents the same CA and honors the same token.
func loadIdentity(cfg *config) (*identity, error) {
	authority, err := ca.LoadOrCreate(filepath.Join(cfg.stateDir, "tls"))
	if err != nil {
		return nil, fmt.Errorf("load ca: %w", err)
	}
	serverCert, err := authority.ServerCert(sanHosts(cfg.listen, cfg.extraSANs))
	if err != nil {
		return nil, fmt.Errorf("serving cert: %w", err)
	}
	tlsCfg := ca.ServerTLS(serverCert, authority.Pool())
	// Every Go peer speaks 1.3, and only Envoy needs its explicit version
	// pin (see the agent's bootstrap template).
	tlsCfg.MinVersion = tls.VersionTLS13
	token, err := loadOrCreateAdminToken(filepath.Join(cfg.stateDir, "token"))
	if err != nil {
		return nil, err
	}
	return &identity{ca: authority, serverTLS: tlsCfg, adminToken: token}, nil
}

// loadOrCreateAdminToken reads the admin bearer token, minting a random one
// on first boot. The CLI reads the same file by default.
func loadOrCreateAdminToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s has mode %04o, want 0600: the admin token must not be group- or world-readable", path, info.Mode().Perm())
		}
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	// O_EXCL settles the race when two replicas boot fresh at once: exactly
	// one mints, the loser rereads the winner's token.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return "", rerr
		}
		return strings.TrimSpace(string(b)), nil
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// The token itself stays out of the log: klited's stderr routinely lands
	// in journals and CI captures, and the CLI reads the file directly anyway.
	slog.Info("admin token minted (first boot)", "path", path)
	return token, nil
}

// sanHosts is the serving cert's SAN list: loopback and Docker names for
// local dials, every interface address so LAN/WAN agents verify without
// configuration, and whatever --tls-san adds.
func sanHosts(listen string, extra []string) []string {
	hosts := []string{"localhost", "host.docker.internal", "127.0.0.1", "::1"}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		hosts = append(hosts, hn)
	}
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	hosts = append(hosts, extra...)
	seen := make(map[string]bool, len(hosts))
	out := hosts[:0]
	for _, h := range hosts {
		if h == "" || h == "0.0.0.0" || h == "::" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// ensureClusterID reads the cluster's random identity, minting it exactly
// once per etcd store via a create-if-absent transaction.
func ensureClusterID(ctx context.Context, cli *clientv3.Client) (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	candidate := hex.EncodeToString(raw)
	resp, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(clusterIDKey), "=", 0)).
		Then(clientv3.OpPut(clusterIDKey, candidate)).
		Else(clientv3.OpGet(clusterIDKey)).
		Commit()
	if err != nil {
		return "", fmt.Errorf("cluster id txn: %w", err)
	}
	if resp.Succeeded {
		slog.Info("cluster id minted (first boot)", "clusterID", candidate)
		return candidate, nil
	}
	kvs := resp.Responses[0].GetResponseRange().Kvs
	if len(kvs) == 0 {
		return "", errors.New("cluster id key vanished mid-transaction")
	}
	return string(kvs[0].Value), nil
}

func run(cfg *config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ident, err := loadIdentity(cfg)
	if err != nil {
		return err
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.etcdEndpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("etcd client: %w", err)
	}
	defer cli.Close()

	idCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	clusterID, err := ensureClusterID(idCtx, cli)
	cancel()
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	auth := server.NewAuth(ident.adminToken)
	grpcSrv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(ident.serverTLS)),
		grpc.ChainUnaryInterceptor(auth.Unary()),
		grpc.ChainStreamInterceptor(auth.Stream()),
		// Keepalive is tuned per research/grpc-go.md so idle agent streams don't outlive dead peers.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 20 * time.Second, Timeout: 10 * time.Second}),
	)
	st := store.NewEtcd(cli)
	// Every replica runs the endpoints engine and an xDS server: whichever
	// klited an Envoy dials must be able to answer it (ADR 0007).
	xdsCache := xds.NewCache()
	xdsCache.RegisterADS(ctx, grpcSrv)
	engine := controller.NewEndpoints(st, func(node string, revision int64, net *klitev1.NetDesired) {
		if err := xdsCache.SetNodeSnapshot(ctx, node, strconv.FormatInt(revision, 10), net); err != nil {
			slog.Warn("xds snapshot rejected", "node", node, "err", err)
		}
	})
	// A departed node leaves the ADS cache too, or it holds every node that
	// ever existed.
	engine.OnNodeRemoved(func(node string) {
		xdsCache.ClearSnapshot(node)
		slog.Info("xds snapshot cleared", "node", node)
	})
	// Both services share the hub: agents park command streams through
	// AgentService, and ClusterService.Logs relays over them.
	hub := server.NewCommandHub()
	klitev1.RegisterClusterServiceServer(grpcSrv, server.NewCluster(st, hub, &server.TokenConfig{
		CAPEM:         ident.ca.CertPEM,
		ClusterSecret: cfg.clusterToken,
	}))
	klitev1.RegisterAgentServiceServer(grpcSrv, server.NewAgent(&server.AgentConfig{
		Store:              st,
		ClusterToken:       cfg.clusterToken,
		Hub:                hub,
		Net:                engine,
		CA:                 ident.ca,
		ClusterID:          clusterID,
		NetAdminPortBase:   int32(cfg.netAdminPortBase),
		EnvoyAdminPortBase: int32(cfg.envoyAdminPortBase),
		InfraIPBase:        int32(cfg.infraIPBase),
		IngressPortBase:    int32(cfg.ingressPortBase),
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		engine.Run(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		hostname, _ := os.Hostname()
		id := fmt.Sprintf("%s/%s/%d", hostname, cfg.listen, os.Getpid())
		err := leader.RunWhenLeader(ctx, cli, id,
			func() { slog.Info("controllers: standing by") },
			func(leadCtx context.Context) {
				slog.Info("controllers: leading")
				controller.RunAll(leadCtx, st, int32(cfg.ingressPortBase))
				slog.Info("controllers: leadership released")
			})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("leader loop exited", "err", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		slog.Info("shutting down")
		done := make(chan struct{})
		go func() {
			grpcSrv.GracefulStop()
			close(done)
		}()
		// Open watch streams would hold GracefulStop forever, so cap the wait.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			grpcSrv.Stop()
		}
	}()

	slog.Info("klited listening", "addr", cfg.listen, "etcd", strings.Join(cfg.etcdEndpoints, ","),
		"clusterID", clusterID, "tls", "1.3")
	err = grpcSrv.Serve(lis)
	wg.Wait()
	return err
}

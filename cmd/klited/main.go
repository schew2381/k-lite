// klited is the stateless control-plane server: gRPC in front, etcd behind (ADR 0004, 0005).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/schew2381/k-lite/internal/controller"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/leader"
	"github.com/schew2381/k-lite/internal/server"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/xds"
)

const defaultEtcd = "127.0.0.1:2379,127.0.0.1:2381,127.0.0.1:2383"

func main() {
	listen := flag.String("listen", "127.0.0.1:7443", "gRPC listen address (plain TCP until M8 adds mTLS)")
	etcdEndpoints := flag.String("etcd", defaultEtcd, "comma-separated etcd client endpoints")
	clusterToken := flag.String("cluster-token", "dev-token", "shared secret agents must present at Register")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*listen, strings.Split(*etcdEndpoints, ","), *clusterToken); err != nil {
		slog.Error("klited exited", "err", err)
		os.Exit(1)
	}
}

func run(listen string, etcdEndpoints []string, clusterToken string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   etcdEndpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("etcd client: %w", err)
	}
	defer cli.Close()

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	grpcSrv := grpc.NewServer(
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
	// Both services share the hub: agents park command streams through
	// AgentService, and ClusterService.Logs relays over them.
	hub := server.NewCommandHub()
	klitev1.RegisterClusterServiceServer(grpcSrv, server.NewCluster(st, hub))
	klitev1.RegisterAgentServiceServer(grpcSrv, server.NewAgent(st, clusterToken, hub, engine))

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
		id := fmt.Sprintf("%s/%s/%d", hostname, listen, os.Getpid())
		err := leader.RunWhenLeader(ctx, cli, id,
			func() { slog.Info("controllers: standing by") },
			func(leadCtx context.Context) {
				slog.Info("controllers: leading")
				controller.RunAll(leadCtx, st)
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

	slog.Info("klited listening", "addr", listen, "etcd", strings.Join(etcdEndpoints, ","))
	err = grpcSrv.Serve(lis)
	wg.Wait()
	return err
}

// klite-net is the per-node network daemon: it owns the infra pod's netns,
// serves DNS for svc.klite, holds the node's service VIPs, and TCP-probes
// instance readiness (ADR 0008). The node's agent drives it over the admin
// gRPC port.
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
	"syscall"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/netd"
)

func main() {
	dnsListen := flag.String("dns-listen", ":53", "DNS listen address, udp+tcp")
	upstream := flag.String("upstream", "1.1.1.1:53", "upstream resolver for non-klite names")
	iface := flag.String("iface", "eth0", "interface that holds the service VIPs")
	adminListen := flag.String("admin-listen", ":9090", "admin gRPC listen address (plain TCP, reachable only via the container's published localhost port)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*dnsListen, *upstream, *iface, *adminListen); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("klite-net exited", "err", err)
		os.Exit(1)
	}
}

func run(dnsListen, upstream, iface, adminListen string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := netd.New(netd.Options{DNSListen: dnsListen, Upstream: upstream, Iface: iface})

	lis, err := net.Listen("tcp", adminListen)
	if err != nil {
		return fmt.Errorf("admin listen: %w", err)
	}
	gs := grpc.NewServer()
	klitev1.RegisterKliteNetServiceServer(gs, srv)
	reflection.Register(gs)

	slog.Info("klite-net starting", "dns", dnsListen, "upstream", upstream,
		"iface", iface, "admin", adminListen)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	g.Go(func() error {
		if err := gs.Serve(lis); err != nil {
			return fmt.Errorf("admin grpc: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		gs.GracefulStop()
		return nil
	})
	return g.Wait()
}

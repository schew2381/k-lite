// klite-facade serves the web UI and its REST/SSE API, speaking gRPC to
// klited like any other client (ADR 0015). It holds no store connection and
// no cluster state. go:embed of the built UI into klited comes later; until
// then --ui-dir points at ui/dist.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/schew2381/k-lite/internal/facade"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7080", "HTTP listen address")
	cluster := flag.String("cluster", "127.0.0.1:7443", "klited gRPC address(es), comma-separated")
	uiDir := flag.String("ui-dir", "", "directory of built UI assets (ui/dist); empty serves the API only")
	dev := flag.Bool("dev", false, "allow cross-origin requests, for the Vite dev server")
	caPath := flag.String("ca", "", "cluster CA certificate (default KLITE_CA, then ~/.klite/server/tls/ca.crt)")
	token := flag.String("token", "", "admin bearer token (default KLITE_TOKEN, then ~/.klite/server/token)")
	insecureSkip := flag.Bool("insecure", false, "skip TLS verification of klited")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	creds, err := facade.LoadCreds(*caPath, *token, *insecureSkip)
	if err != nil {
		slog.Error("klite-facade exited", "err", err)
		os.Exit(1)
	}
	if err := run(*listen, facade.SplitEndpoints(*cluster), *uiDir, *dev, creds); err != nil {
		slog.Error("klite-facade exited", "err", err)
		os.Exit(1)
	}
}

func run(listen string, endpoints []string, uiDir string, dev bool, creds *facade.Creds) error {
	if len(endpoints) == 0 {
		return errors.New("--cluster must name at least one klited address")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, client, err := facade.Dial(endpoints, creds)
	if err != nil {
		return fmt.Errorf("dial cluster: %w", err)
	}
	defer conn.Close()

	srv := &http.Server{
		Addr:              listen,
		Handler:           facade.New(client, endpoints, uiDir, dev, creds).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No write timeout: watch and log streams stay open indefinitely.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("klite-facade listening", "addr", listen, "cluster", strings.Join(endpoints, ","), "ui", uiDir, "dev", dev)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	// Open SSE and log streams would hold Shutdown forever, so cap the wait.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return srv.Close()
	}
	return nil
}

// klite is the CLI for the k-lite control plane.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/schew2381/k-lite/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Root().ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

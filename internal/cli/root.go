// Package cli implements the klite command tree.
package cli

import (
	"time"

	"github.com/spf13/cobra"
)

// One-shot commands give up after this. Watches run until interrupted.
const callTimeout = 15 * time.Second

// Root builds the klite command.
func Root() *cobra.Command {
	var server string
	root := &cobra.Command{
		Use:           "klite",
		Short:         "klite talks to the k-lite control plane",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVar(&server, "server", "", "klited address(es), comma-separated (overrides KLITE_SERVER and ~/.klite/config)")
	root.AddCommand(
		newApplyCmd(&server),
		newGetCmd(&server),
		newDescribeCmd(&server),
		newDeleteCmd(&server),
		newScaleCmd(&server),
		newLogsCmd(&server),
	)
	return root
}

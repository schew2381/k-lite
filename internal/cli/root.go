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
	cfg := &connCfg{}
	root := &cobra.Command{
		Use:           "klite",
		Short:         "klite talks to the k-lite control plane",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVar(&cfg.server, "server", "", "klited address(es), comma-separated (overrides KLITE_SERVER and ~/.klite/config)")
	root.PersistentFlags().BoolVar(&cfg.insecure, "insecure", false, "skip TLS server verification (still encrypted; for hosts without the cluster CA)")
	root.AddCommand(
		newApplyCmd(cfg),
		newGetCmd(cfg),
		newDescribeCmd(cfg),
		newDeleteCmd(cfg),
		newScaleCmd(cfg),
		newDrainCmd(cfg),
		newUncordonCmd(cfg),
		newLogsCmd(cfg),
		newPolicyCmd(cfg),
		newNodeCmd(cfg),
	)
	return root
}

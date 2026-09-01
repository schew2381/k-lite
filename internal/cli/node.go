package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func newNodeCmd(cfg *connCfg) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Node lifecycle helpers",
	}
	cmd.AddCommand(newNodeTokenCmd(cfg))
	return cmd
}

func newNodeTokenCmd(cfg *connCfg) *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Mint a join token for a new node's agent (--token)",
		Long: "Mint a join token carrying the cluster secret and the CA hash a joining\n" +
			"agent pins on its first, unverified dial (ADR 0013).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, client, err := dial(cfg)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			resp, err := client.NodeToken(ctx, &klitev1.NodeTokenRequest{})
			if err != nil {
				return rpcErr(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), resp.GetToken())
			return nil
		},
	}
}

func newUncordonCmd(cfg *connCfg) *cobra.Command {
	return &cobra.Command{
		Use:   "uncordon <node>",
		Short: "Allow scheduling on a drained node again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, client, err := dial(cfg)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			if _, err := client.Uncordon(ctx, &klitev1.UncordonRequest{Node: args[0]}); err != nil {
				return rpcErr(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "node %s uncordoned\n", args[0])
			return nil
		},
	}
}

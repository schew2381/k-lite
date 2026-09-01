package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func newDrainCmd(server *string) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "drain <node>",
		Short: "Cordon a node and move its instances elsewhere, surge-first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, client, err := dial(*server)
			if err != nil {
				return err
			}
			defer conn.Close()
			// No callTimeout: a drain runs as long as its slowest instance.
			stream, err := client.Drain(cmd.Context(), &klitev1.DrainRequest{Node: args[0], Force: force})
			if err != nil {
				return rpcErr(err)
			}
			for {
				msg, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return rpcErr(err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), msg.GetMessage())
			}
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "delete draining instances immediately instead of waiting out drain timeouts")
	return cmd
}

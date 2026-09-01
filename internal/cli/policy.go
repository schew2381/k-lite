package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// newPolicyCmd builds `klite policy check <from> <to>`, which asks the server
// for the verdict its own data plane enforces.
func newPolicyCmd(server *string) *cobra.Command {
	policy := &cobra.Command{
		Use:   "policy",
		Short: "Inspect NetworkPolicy behavior",
	}
	check := &cobra.Command{
		Use:   "check <from> <to>",
		Short: "Report whether traffic from one service to another is allowed",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, client, err := dial(*server)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			resp, err := client.PolicyCheck(ctx, &klitev1.PolicyCheckRequest{From: args[0], To: args[1]})
			if err != nil {
				return rpcErr(err)
			}
			verdict := "denied"
			if resp.GetAllowed() {
				verdict = "allowed"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s: %s (%s)\n", args[0], args[1], verdict, resp.GetReason())
			if !resp.GetAllowed() {
				// Nonzero exit so scripts can branch on the verdict.
				cmd.SilenceErrors = true
				return fmt.Errorf("denied")
			}
			return nil
		},
	}
	policy.AddCommand(check)
	return policy
}

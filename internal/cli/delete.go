package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func newDeleteCmd(server *string) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "delete (-f <file|dir|-> | <kind> <name>)",
		Short: "Delete objects by file or by kind and name",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &klitev1.DeleteRequest{}
			switch {
			case len(files) > 0 && len(args) > 0:
				return fmt.Errorf("use either -f or <kind> <name>, not both")
			case len(files) > 0:
				yamlBytes, err := readInputs(files)
				if err != nil {
					return err
				}
				req.Yaml = yamlBytes
			case len(args) == 2:
				kind, err := object.Canonical(args[0])
				if err != nil {
					return err
				}
				req.Kind = kind
				req.Name = args[1]
			default:
				return fmt.Errorf("provide -f, or a kind and a name")
			}

			conn, client, err := dial(*server)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			resp, err := client.Delete(ctx, req)
			if err != nil {
				return rpcErr(err)
			}
			return printResults(cmd.OutOrStdout(), resp.GetResults())
		},
	}
	cmd.Flags().StringSliceVarP(&files, "filename", "f", nil, "YAML file, directory of YAML files, or - for stdin")
	return cmd
}

func newScaleCmd(server *string) *cobra.Command {
	var replicas int32
	cmd := &cobra.Command{
		Use:   "scale workload <name> --replicas N",
		Short: "Set a workload's replica count",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := object.Canonical(args[0])
			if err != nil || kind != object.KindWorkload {
				return fmt.Errorf("scale only works on workloads, got %q", args[0])
			}
			if replicas < 0 {
				return fmt.Errorf("--replicas must be >= 0, got %d", replicas)
			}
			conn, client, err := dial(*server)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			if _, err := client.Scale(ctx, &klitev1.ScaleRequest{Workload: args[1], Replicas: replicas}); err != nil {
				return rpcErr(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s/%s scaled to %d\n", strings.ToLower(object.KindWorkload), args[1], replicas)
			return nil
		},
	}
	cmd.Flags().Int32Var(&replicas, "replicas", 0, "desired replica count")
	_ = cmd.MarkFlagRequired("replicas")
	return cmd
}

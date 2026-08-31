package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func newApplyCmd(server *string) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "apply -f <file|dir|->",
		Short: "Create or update objects from YAML",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			yamlBytes, err := readInputs(files)
			if err != nil {
				return err
			}
			conn, client, err := dial(*server)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			resp, err := client.Apply(ctx, &klitev1.ApplyRequest{Yaml: yamlBytes})
			if err != nil {
				return rpcErr(err)
			}
			return printResults(cmd.OutOrStdout(), resp.GetResults())
		},
	}
	cmd.Flags().StringSliceVarP(&files, "filename", "f", nil, "YAML file, directory of YAML files, or - for stdin")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// readInputs concatenates every named source into one multi-document YAML stream.
func readInputs(paths []string) ([]byte, error) {
	var docs [][]byte
	for _, p := range paths {
		if p == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("read stdin: %w", err)
			}
			docs = append(docs, b)
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			docs = append(docs, b)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if ext != ".yaml" && ext != ".yml" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(p, e.Name()))
			if err != nil {
				return nil, err
			}
			docs = append(docs, b)
			found = true
		}
		if !found {
			return nil, fmt.Errorf("no .yaml or .yml files in %s", p)
		}
	}
	return bytes.Join(docs, []byte("\n---\n")), nil
}

// printResults renders per-object outcomes and reports failure when any object errored.
func printResults(w io.Writer, results []*klitev1.ApplyResult) error {
	failed := 0
	for _, r := range results {
		ref := fmt.Sprintf("%s/%s", strings.ToLower(r.GetKind()), r.GetName())
		if r.GetError() != "" {
			failed++
			fmt.Fprintf(w, "%s error: %s\n", ref, r.GetError())
			continue
		}
		fmt.Fprintf(w, "%s %s\n", ref, r.GetAction())
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d objects failed", failed, len(results))
	}
	return nil
}

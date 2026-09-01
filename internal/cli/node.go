package cli

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func newNodeCmd(cfg *connCfg) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Node lifecycle helpers",
	}
	cmd.AddCommand(newNodeTokenCmd(cfg), newNodeAddCmd(cfg))
	return cmd
}

// nodeAddOpts carries everything runNodeAdd needs, resolved from flags so the
// core stays testable against a fake client.
type nodeAddOpts struct {
	name         string
	labels       map[string]string
	maxInstances int32
	// url is the klited address the printed join line tells agents to dial.
	url string
}

func newNodeAddCmd(cfg *connCfg) *cobra.Command {
	opts := &nodeAddOpts{}
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Declare a node and print its join command",
		Long: "Declare the Node object (membership is declared, never discovered, ADR 0018),\n" +
			"mint its join token, and print the paste-ready join command for the new\n" +
			"machine: the release-based one-liner plus a manual fallback (ADR 0038).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			if opts.url == "" {
				opts.url = endpoints(cfg.server)[0]
			}
			conn, client, err := dial(cfg)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			return runNodeAdd(ctx, client, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringToStringVar(&opts.labels, "labels", nil, "node labels as k=v pairs, comma-separated")
	cmd.Flags().Int32Var(&opts.maxInstances, "max-instances", 0, "instance capacity for the scheduler (default 32)")
	cmd.Flags().StringVar(&opts.url, "url", "", "klited address the printed join command dials (default: this CLI's first server endpoint)")
	return cmd
}

// runNodeAdd applies the Node object, mints a token, and prints the join
// block. Apply is idempotent, so re-running refreshes the printout for a node
// that already exists.
func runNodeAdd(ctx context.Context, client klitev1.ClusterServiceClient, out io.Writer, opts *nodeAddOpts) error {
	manifest, err := nodeYAML(opts.name, opts.labels, opts.maxInstances)
	if err != nil {
		return err
	}
	resp, err := client.Apply(ctx, &klitev1.ApplyRequest{Yaml: manifest})
	if err != nil {
		return rpcErr(err)
	}
	if err := printResults(out, resp.GetResults()); err != nil {
		return err
	}
	tok, err := client.NodeToken(ctx, &klitev1.NodeTokenRequest{})
	if err != nil {
		return rpcErr(err)
	}
	printJoinBlock(out, opts.name, opts.url, tok.GetToken())
	return nil
}

// nodeYAML builds the Node manifest node add applies. Zero maxInstances is
// omitted so the server's default (32) wins.
func nodeYAML(name string, labels map[string]string, maxInstances int32) ([]byte, error) {
	meta := map[string]any{"name": name}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	doc := map[string]any{
		"apiVersion": object.APIVersion,
		"kind":       "Node",
		"metadata":   meta,
	}
	if maxInstances > 0 {
		doc["spec"] = map[string]any{"maxInstances": maxInstances}
	}
	return yaml.Marshal(doc)
}

// printJoinBlock renders the two ways onto the cluster: the release-based
// one-liner join.sh serves, and the copy-the-binary fallback for machines
// (or times) the releases don't cover. Both embed the minted token.
func printJoinBlock(out io.Writer, name, url, token string) {
	fmt.Fprintf(out, "\njoin from the new machine (Linux with systemd, as root):\n\n")
	fmt.Fprintf(out, "  curl -sfL https://github.com/schew2381/k-lite/releases/latest/download/join.sh | \\\n")
	fmt.Fprintf(out, "    KLITE_URL=%s KLITE_TOKEN='%s' KLITE_NODE=%s sh -\n\n", url, token, name)
	fmt.Fprintf(out, "until a public release exists (or off Linux), copy bin/klite-agent to the\nmachine and run it by hand:\n\n")
	fmt.Fprintf(out, "  sudo ./klite-agent --node %s --server %s \\\n", name, url)
	fmt.Fprintf(out, "    --token '%s' \\\n", token)
	fmt.Fprintf(out, "    --advertise-address <address other machines dial>\n\n")
	fmt.Fprintf(out, "on a real Linux machine --advertise-address is not optional: the default\n"+
		"resolves to the Docker bridge gateway there, usually 172.17.0.1, and every\n"+
		"other node would dial its own bridge. join.sh detects the public IPv4 and\n"+
		"refuses to guess when it only finds private addresses.\n")
	if h := hostOf(url); localOnlyHost(h) {
		fmt.Fprintf(out, "\nnote: the new machine cannot dial %s. re-run with --url set to an\n"+
			"address it can reach, and make sure klited listens there\n"+
			"(bin/klited --listen 0.0.0.0:7443).\n", url)
	}
}

// localOnlyHost reports whether the join URL's host only works from this
// machine: loopback, or an unspecified bind address.
func localOnlyHost(h string) bool {
	if h == "localhost" || h == "0.0.0.0" || h == "::" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
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

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func newGetCmd(server *string) *cobra.Command {
	var output string
	var watch bool
	cmd := &cobra.Command{
		Use:   "get <kind> [name]",
		Short: "List objects, or stream changes with --watch",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := object.Canonical(args[0])
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			conn, client, err := dial(*server)
			if err != nil {
				return err
			}
			defer conn.Close()

			if watch {
				return runWatch(cmd.Context(), cmd.OutOrStdout(), client, kind, name)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			resp, err := client.List(ctx, &klitev1.ListRequest{Kind: kind, Name: name})
			if err != nil {
				return rpcErr(err)
			}
			return render(cmd.OutOrStdout(), kind, name, output, resp.GetObjects())
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table, json, or yaml")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "stream change events instead of listing")
	return cmd
}

func runWatch(ctx context.Context, w io.Writer, client klitev1.ClusterServiceClient, kind, name string) error {
	stream, err := client.Watch(ctx, &klitev1.WatchRequest{Kinds: []string{kind}})
	if err != nil {
		return rpcErr(err)
	}
	for {
		ev, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF), status.Code(err) == codes.Canceled:
			return nil
		case err != nil:
			return rpcErr(err)
		}
		meta := object.MetaOf(ev.GetObject())
		if name != "" && meta.GetName() != name {
			continue
		}
		evName := strings.TrimPrefix(ev.GetType().String(), "EVENT_TYPE_")
		ref := fmt.Sprintf("%s/%s", strings.ToLower(object.KindOf(ev.GetObject())), meta.GetName())
		fmt.Fprintf(w, "%-9s %-30s rev=%d\n", evName, ref, ev.GetRevision())
	}
}

func render(w io.Writer, kind, name, output string, objs []*klitev1.Object) error {
	switch output {
	case "table":
		return printTable(w, kind, objs)
	case "yaml":
		for i, o := range objs {
			if i > 0 {
				fmt.Fprintln(w, "---")
			}
			b, err := object.Encode(o)
			if err != nil {
				return err
			}
			fmt.Fprint(w, string(b))
		}
		return nil
	case "json":
		items := make([]any, 0, len(objs))
		for _, o := range objs {
			raw, err := object.EncodeJSON(o)
			if err != nil {
				return err
			}
			var m any
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			items = append(items, m)
		}
		var out any = items
		if name != "" && len(items) == 1 {
			out = items[0]
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	return fmt.Errorf("unknown output format %q (table, json, or yaml)", output)
}

package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func newDescribeCmd(cfg *connCfg) *cobra.Command {
	return &cobra.Command{
		Use:   "describe <kind> <name>",
		Short: "Show one object's spec and status in full",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := object.Canonical(args[0])
			if err != nil {
				return err
			}
			conn, client, err := dial(cfg)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout)
			defer cancel()
			resp, err := client.List(ctx, &klitev1.ListRequest{Kind: kind, Name: args[1]})
			if err != nil {
				return rpcErr(err)
			}
			if len(resp.GetObjects()) == 0 {
				return fmt.Errorf("%s %q not found", strings.ToLower(kind), args[1])
			}
			related, err := relatedInstances(ctx, client, kind, args[1])
			if err != nil {
				return err
			}
			return describe(cmd.OutOrStdout(), resp.GetObjects()[0], related, time.Now())
		},
	}
}

// relatedInstances fetches the instances a workload owns or a node hosts,
// sorted by name. Other kinds have none.
func relatedInstances(ctx context.Context, client klitev1.ClusterServiceClient, kind, name string) ([]*klitev1.Instance, error) {
	if kind != object.KindWorkload && kind != object.KindNode {
		return nil, nil
	}
	resp, err := client.List(ctx, &klitev1.ListRequest{Kind: object.KindInstance})
	if err != nil {
		return nil, rpcErr(err)
	}
	var out []*klitev1.Instance
	for _, o := range resp.GetObjects() {
		inst := o.GetInstance()
		owned := kind == object.KindWorkload && inst.GetSpec().GetWorkload() == name
		hosted := kind == object.KindNode && inst.GetSpec().GetNode() == name
		if owned || hosted {
			out = append(out, inst)
		}
	}
	slices.SortFunc(out, func(a, b *klitev1.Instance) int {
		return strings.Compare(a.GetMeta().GetName(), b.GetMeta().GetName())
	})
	return out, nil
}

// describe renders one object as a label-per-line detail view.
func describe(w io.Writer, obj *klitev1.Object, related []*klitev1.Instance, now time.Time) error {
	switch k := obj.GetKind().(type) {
	case *klitev1.Object_Workload:
		return describeWorkload(w, k.Workload, related, now)
	case *klitev1.Object_Instance:
		return describeInstance(w, k.Instance, now)
	case *klitev1.Object_Node:
		return describeNode(w, k.Node, related, now)
	case *klitev1.Object_Service:
		return describeService(w, k.Service, now)
	case *klitev1.Object_NetworkPolicy:
		return describePolicy(w, k.NetworkPolicy, now)
	}
	return fmt.Errorf("no describe view for kind %q", object.KindOf(obj))
}

type kv struct{ k, v string }

func fields(w io.Writer, rows []kv) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s:\t%s\n", r.k, r.v)
	}
	return tw.Flush()
}

// instanceBlock prints the related instances under a workload or node. The
// middle column differs between the two views, so the caller names it.
func instanceBlock(w io.Writer, instances []*klitev1.Instance, col string, colOf func(*klitev1.Instance) string) error {
	fmt.Fprintln(w, "Instances:")
	if len(instances) == 0 {
		fmt.Fprintln(w, "  <none>")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	fmt.Fprintf(tw, "  NAME\t%s\tPHASE\tRESTARTS\n", col)
	for _, in := range instances {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\n",
			in.GetMeta().GetName(), colOf(in),
			phase(in.GetStatus().GetPhase().String(), "INSTANCE_PHASE_"),
			in.GetStatus().GetRestarts())
	}
	return tw.Flush()
}

func describeWorkload(w io.Writer, wl *klitev1.Workload, instances []*klitev1.Instance, now time.Time) error {
	spec := wl.GetSpec()
	rows := []kv{
		{"Name", wl.GetMeta().GetName()},
		{"Labels", formatSelector(wl.GetMeta().GetLabels())},
		{"Age", age(wl.GetMeta().GetCreatedUnix(), now)},
		{"Replicas", fmt.Sprintf("%d desired, %d ready", spec.GetReplicas(), wl.GetStatus().GetReadyInstances())},
		{"Node pin", orDash(spec.GetNodeName())},
	}
	for _, c := range spec.GetTemplate().GetContainers() {
		rows = append(rows, kv{"Container", c.GetName() + " (" + c.GetImage() + ")"})
	}
	rows = append(rows,
		kv{"Template hash", orDash(wl.GetStatus().GetTemplateHash())},
		kv{"Drain", fmt.Sprintf("timeout %ds, grace %ds",
			spec.GetDrain().GetDrainTimeoutSeconds(), spec.GetDrain().GetTerminationGraceSeconds())},
	)
	if err := fields(w, rows); err != nil {
		return err
	}
	return instanceBlock(w, instances, "NODE", func(in *klitev1.Instance) string {
		return orDash(in.GetSpec().GetNode())
	})
}

func describeInstance(w io.Writer, in *klitev1.Instance, now time.Time) error {
	st := in.GetStatus()
	return fields(w, []kv{
		{"Name", in.GetMeta().GetName()},
		{"Workload", in.GetSpec().GetWorkload()},
		{"Node", orDash(in.GetSpec().GetNode())},
		{"Phase", phase(st.GetPhase().String(), "INSTANCE_PHASE_")},
		{"Restarts", strconv.Itoa(int(st.GetRestarts()))},
		{"IP", orDash(st.GetInstanceIp())},
		{"Container", orDash(shortID(st.GetContainerId()))},
		// Message carries the scheduler's why-still-pending reason and the
		// agent's last failure, whichever wrote most recently.
		{"Message", orDash(st.GetMessage())},
		{"Age", age(in.GetMeta().GetCreatedUnix(), now)},
	})
}

func describeNode(w io.Writer, n *klitev1.Node, instances []*klitev1.Instance, now time.Time) error {
	st := n.GetStatus()
	rows := []kv{
		{"Name", n.GetMeta().GetName()},
		{"Phase", phase(st.GetPhase().String(), "NODE_PHASE_")},
		{"Unschedulable", strconv.FormatBool(st.GetUnschedulable())},
		{"Heartbeat age", age(st.GetLastHeartbeatUnix(), now)},
		{"Index", strconv.Itoa(int(st.GetNodeIndex()))},
		{"Max instances", strconv.Itoa(int(n.GetSpec().GetMaxInstances()))},
		{"Age", age(n.GetMeta().GetCreatedUnix(), now)},
	}
	if err := fields(w, rows); err != nil {
		return err
	}
	return instanceBlock(w, instances, "WORKLOAD", func(in *klitev1.Instance) string {
		return in.GetSpec().GetWorkload()
	})
}

func describeService(w io.Writer, svc *klitev1.Service, now time.Time) error {
	return fields(w, []kv{
		{"Name", svc.GetMeta().GetName()},
		{"Selector", formatSelector(svc.GetSpec().GetSelector())},
		{"Port", strconv.Itoa(int(svc.GetSpec().GetPort()))},
		{"Target port", strconv.Itoa(int(svc.GetSpec().GetTargetPort()))},
		{"Age", age(svc.GetMeta().GetCreatedUnix(), now)},
	})
}

func describePolicy(w io.Writer, p *klitev1.NetworkPolicy, now time.Time) error {
	return fields(w, []kv{
		{"Name", p.GetMeta().GetName()},
		{"Action", phase(p.GetSpec().GetAction().String(), "POLICY_ACTION_")},
		{"Rules", formatRules(p.GetSpec().GetRules())},
		{"Age", age(p.GetMeta().GetCreatedUnix(), now)},
	})
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

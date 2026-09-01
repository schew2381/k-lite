package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func printTable(w io.Writer, kind string, objs []*klitev1.Object) error {
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	defer tw.Flush()
	now := time.Now()

	switch kind {
	case object.KindWorkload:
		fmt.Fprintln(tw, "NAME\tREADY\tREPLICAS\tAGE")
		for _, o := range objs {
			wl := o.GetWorkload()
			fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%s\n",
				wl.GetMeta().GetName(),
				wl.GetStatus().GetReadyInstances(), wl.GetSpec().GetReplicas(),
				wl.GetSpec().GetReplicas(),
				age(wl.GetMeta().GetCreatedUnix(), now))
		}
	case object.KindService:
		fmt.Fprintln(tw, "NAME\tPORT\tTARGETPORT\tSELECTOR")
		for _, o := range objs {
			svc := o.GetService()
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n",
				svc.GetMeta().GetName(),
				svc.GetSpec().GetPort(), svc.GetSpec().GetTargetPort(),
				formatSelector(svc.GetSpec().GetSelector()))
		}
	case object.KindNode:
		fmt.Fprintln(tw, "NAME\tPHASE\tUNSCHEDULABLE\tINSTANCES\tAGE")
		for _, o := range objs {
			n := o.GetNode()
			fmt.Fprintf(tw, "%s\t%s\t%t\t%d\t%s\n",
				n.GetMeta().GetName(),
				phase(n.GetStatus().GetPhase().String(), "NODE_PHASE_"),
				n.GetStatus().GetUnschedulable(),
				n.GetStatus().GetInstanceCount(),
				age(n.GetMeta().GetCreatedUnix(), now))
		}
	case object.KindNetworkPolicy:
		fmt.Fprintln(tw, "NAME\tACTION\tRULES")
		for _, o := range objs {
			p := o.GetNetworkPolicy()
			fmt.Fprintf(tw, "%s\t%s\t%s\n",
				p.GetMeta().GetName(),
				phase(p.GetSpec().GetAction().String(), "POLICY_ACTION_"),
				formatRules(p.GetSpec().GetRules()))
		}
	case object.KindVIPAllocation:
		printVIPAllocations(tw, objs)
	case object.KindIngressAllocation:
		printIngressAllocations(tw, objs)
	case object.KindInstance:
		fmt.Fprintln(tw, "NAME\tWORKLOAD\tNODE\tPHASE\tRESTARTS\tIP\tAGE")
		for _, o := range objs {
			in := o.GetInstance()
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				in.GetMeta().GetName(),
				in.GetSpec().GetWorkload(), in.GetSpec().GetNode(),
				phase(in.GetStatus().GetPhase().String(), "INSTANCE_PHASE_"),
				in.GetStatus().GetRestarts(),
				orDash(in.GetStatus().GetInstanceIp()),
				age(in.GetMeta().GetCreatedUnix(), now))
		}
	default:
		return fmt.Errorf("no table renderer for kind %q", kind)
	}
	return nil
}

func printVIPAllocations(tw io.Writer, objs []*klitev1.Object) {
	fmt.Fprintln(tw, "NAME\tSERVICE\tNODE\tVIP")
	for _, o := range objs {
		v := o.GetVipAllocation()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			v.GetMeta().GetName(),
			v.GetSpec().GetService(), v.GetSpec().GetNode(), v.GetSpec().GetVip())
	}
}

func printIngressAllocations(tw io.Writer, objs []*klitev1.Object) {
	fmt.Fprintln(tw, "NAME\tSERVICE\tINSTANCE\tNODE\tPORT")
	for _, o := range objs {
		ia := o.GetIngressAllocation()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			ia.GetMeta().GetName(),
			ia.GetSpec().GetService(), ia.GetSpec().GetInstance(),
			ia.GetSpec().GetNode(), ia.GetSpec().GetPort())
	}
}

// phase turns an enum value name into display form: NODE_PHASE_NOT_READY becomes NotReady.
func phase(enumName, prefix string) string {
	s := strings.TrimPrefix(enumName, prefix)
	if s == "UNSPECIFIED" {
		return "Unknown"
	}
	var b strings.Builder
	for word := range strings.SplitSeq(s, "_") {
		if word == "" {
			continue
		}
		b.WriteString(strings.ToUpper(word[:1]))
		b.WriteString(strings.ToLower(word[1:]))
	}
	out := b.String()
	// Policy actions read better the way users wrote them.
	if out == "Allow" || out == "Deny" {
		return strings.ToUpper(out)
	}
	return out
}

func formatSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return "<none>"
	}
	keys := slices.Sorted(maps.Keys(sel))
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+sel[k])
	}
	return strings.Join(pairs, ",")
}

func formatRules(rules []*klitev1.PolicyRule) string {
	if len(rules) == 0 {
		return "<none>"
	}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		s := r.GetFrom() + "->" + r.GetTo()
		if len(r.GetExcept()) > 0 {
			s += " except " + strings.Join(r.GetExcept(), ",")
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// age renders elapsed time kubectl-style, in the single largest useful unit.
func age(createdUnix int64, now time.Time) string {
	if createdUnix == 0 {
		return "-"
	}
	d := now.Sub(time.Unix(createdUnix, 0))
	switch {
	case d < 0:
		return "0s"
	case d < 2*time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

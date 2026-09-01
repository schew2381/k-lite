package object_test

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

const workloadYAML = `apiVersion: klite/v1
kind: Workload
metadata:
  name: b
  labels:
    app: b
spec:
  replicas: 2
  template:
    labels:
      app: b
    containers:
      - name: web
        image: traefik/whoami:v1.10
        env:
          - name: WHOAMI_NAME
            value: b
        ports:
          - containerPort: 80
        readinessProbe:
          tcpPort: 80
`

const serviceYAML = `apiVersion: klite/v1
kind: Service
metadata:
  name: b
spec:
  selector:
    app: b
  port: 8080
  targetPort: 80
`

const nodeYAML = `apiVersion: klite/v1
kind: Node
metadata:
  name: node-1
  labels:
    zone: local
spec:
  maxInstances: 32
`

const policyYAML = `apiVersion: klite/v1
kind: NetworkPolicy
metadata:
  name: lockdown-a
spec:
  action: DENY
  rules:
    - from: a
      to: "*"
      except: [b]
`

func checkWorkloadDoc(t *testing.T, o *klitev1.Object) {
	t.Helper()
	w := o.GetWorkload()
	if w == nil {
		t.Fatal("not a workload")
	}
	if got := w.GetMeta().GetName(); got != "b" {
		t.Errorf("name = %q, want b", got)
	}
	if got := w.GetSpec().GetReplicas(); got != 2 {
		t.Errorf("replicas = %d, want 2", got)
	}
	cs := w.GetSpec().GetTemplate().GetContainers()
	if len(cs) != 1 || cs[0].GetImage() != "traefik/whoami:v1.10" {
		t.Errorf("containers = %v", cs)
	}
	if got := cs[0].GetReadinessProbe().GetTcpPort(); got != 80 {
		t.Errorf("tcpPort = %d, want 80", got)
	}
}

func checkServiceDoc(t *testing.T, o *klitev1.Object) {
	t.Helper()
	s := o.GetService()
	if s.GetSpec().GetPort() != 8080 || s.GetSpec().GetTargetPort() != 80 {
		t.Errorf("ports = %d/%d, want 8080/80", s.GetSpec().GetPort(), s.GetSpec().GetTargetPort())
	}
	if s.GetSpec().GetSelector()["app"] != "b" {
		t.Errorf("selector = %v", s.GetSpec().GetSelector())
	}
}

func checkNodeDoc(t *testing.T, o *klitev1.Object) {
	t.Helper()
	if got := o.GetNode().GetSpec().GetMaxInstances(); got != 32 {
		t.Errorf("maxInstances = %d, want 32", got)
	}
}

func checkPolicyDoc(t *testing.T, o *klitev1.Object) {
	t.Helper()
	p := o.GetNetworkPolicy()
	if p.GetSpec().GetAction() != klitev1.PolicyAction_POLICY_ACTION_DENY {
		t.Errorf("action = %v, want DENY", p.GetSpec().GetAction())
	}
	r := p.GetSpec().GetRules()[0]
	if r.GetTo() != "*" || len(r.GetExcept()) != 1 || r.GetExcept()[0] != "b" {
		t.Errorf("rule = %v", r)
	}
}

func TestDecodeSingleDocuments(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		check func(t *testing.T, o *klitev1.Object)
	}{
		{name: "workload", yaml: workloadYAML, check: checkWorkloadDoc},
		{name: "service", yaml: serviceYAML, check: checkServiceDoc},
		{name: "node", yaml: nodeYAML, check: checkNodeDoc},
		{name: "network policy with short enum", yaml: policyYAML, check: checkPolicyDoc},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs, err := object.Decode([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(objs) != 1 {
				t.Fatalf("got %d objects, want 1", len(objs))
			}
			tt.check(t, objs[0])
		})
	}
}

func TestDecodeMultiDoc(t *testing.T) {
	multi := "# leading comment\n---\n" + workloadYAML + "---\n" + serviceYAML + "---\n# only a comment\n"
	objs, err := object.Decode([]byte(multi))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2", len(objs))
	}
	if object.KindOf(objs[0]) != object.KindWorkload || object.KindOf(objs[1]) != object.KindService {
		t.Errorf("kinds = %s, %s", object.KindOf(objs[0]), object.KindOf(objs[1]))
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"missing apiVersion", "kind: Workload\nmetadata:\n  name: a\n", "apiVersion"},
		{"wrong apiVersion", "apiVersion: klite/v2\nkind: Workload\nmetadata:\n  name: a\n", "apiVersion"},
		{"unknown kind", "apiVersion: klite/v1\nkind: Pod\nmetadata:\n  name: a\n", "unknown kind"},
		{"unknown field", "apiVersion: klite/v1\nkind: Service\nmetadata:\n  name: a\nspec:\n  porta: 1\n", "unknown field"},
		{"bad enum", "apiVersion: klite/v1\nkind: NetworkPolicy\nmetadata:\n  name: a\nspec:\n  action: REJECT\n", "invalid value"},
		{"meta and metadata", "apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: a\nmeta:\n  name: a\n", "both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := object.Decode([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRejectsOversizeInput(t *testing.T) {
	big := append([]byte(workloadYAML), bytes.Repeat([]byte("# padding\n"), 512*1024)...)
	if _, err := object.Decode(big); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("Decode(5MB) err = %v, want size limit error", err)
	}
	if _, err := object.DecodeOne(big); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("DecodeOne(5MB) err = %v, want size limit error", err)
	}
}

// The YAML library refuses documents whose aliases expand far beyond their
// byte size. This pins that a billion-laughs payload errors instead of
// eating the heap.
func TestDecodeRejectsAliasBomb(t *testing.T) {
	bomb := `apiVersion: klite/v1
kind: Workload
metadata:
  name: a
spec:
  a: &a ["x","x","x","x","x","x","x","x","x"]
  b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
  c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
  d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]
  e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]
  f: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e]
  g: &g [*f,*f,*f,*f,*f,*f,*f,*f,*f]
  h: &h [*g,*g,*g,*g,*g,*g,*g,*g,*g]
  i: &i [*h,*h,*h,*h,*h,*h,*h,*h,*h]
`
	if _, err := object.Decode([]byte(bomb)); err == nil {
		t.Error("Decode accepted an alias bomb")
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"workload", workloadYAML},
		{"service", serviceYAML},
		{"node", nodeYAML},
		{"network policy", policyYAML},
		// Server-materialized kinds still cross the codec: Apply rejects
		// them later by kind, so a client-sent document must decode (never
		// panic) and a stored one must encode for `get -o yaml`.
		{"ingress allocation", "apiVersion: klite/v1\nkind: IngressAllocation\nmetadata:\n  name: b.b-aa\nspec:\n  service: b\n  instance: b-aa\n  node: node-1\n  port: 20000\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, err := object.Decode([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			out, err := object.Encode(first[0])
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			second, err := object.Decode(out)
			if err != nil {
				t.Fatalf("Decode(Encode(...)): %v\n%s", err, out)
			}
			if !proto.Equal(first[0], second[0]) {
				t.Errorf("round trip changed the object:\nfirst:  %v\nsecond: %v\nyaml:\n%s", first[0], second[0], out)
			}
		})
	}
}

func TestEncodeShape(t *testing.T) {
	objs, err := object.Decode([]byte(policyYAML))
	if err != nil {
		t.Fatal(err)
	}
	out, err := object.Encode(objs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apiVersion: klite/v1", "kind: NetworkPolicy", "metadata:", "action: DENY"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("encoded yaml missing %q:\n%s", want, out)
		}
	}
	wl, _ := object.Decode([]byte(workloadYAML))
	wout, err := object.Encode(wl[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"containerPort: 80", "tcpPort: 80"} {
		if !strings.Contains(string(wout), want) {
			t.Errorf("encoded yaml missing camelCase %q:\n%s", want, wout)
		}
	}
}

func TestCanonical(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Workload", object.KindWorkload, false},
		{"workload", object.KindWorkload, false},
		{"workloads", object.KindWorkload, false},
		{"WORKLOADS", object.KindWorkload, false},
		{"services", object.KindService, false},
		{"nodes", object.KindNode, false},
		{"NetworkPolicy", object.KindNetworkPolicy, false},
		{"networkpolicies", object.KindNetworkPolicy, false},
		{"instances", object.KindInstance, false},
		{"pod", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run("in="+tt.in, func(t *testing.T) {
			got, err := object.Canonical(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Canonical(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

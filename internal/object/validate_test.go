package object_test

import (
	"strings"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func workload(name string, replicas int32, containers ...*klitev1.Container) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.WorkloadSpec{
			Replicas: replicas,
			Template: &klitev1.Template{Containers: containers},
		},
	}}}
}

func service(name string, port, targetPort int32) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Service{Service: &klitev1.Service{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.ServiceSpec{Port: port, TargetPort: targetPort},
	}}}
}

func policy(name string, action klitev1.PolicyAction, rules ...*klitev1.PolicyRule) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_NetworkPolicy{NetworkPolicy: &klitev1.NetworkPolicy{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.NetworkPolicySpec{Action: action, Rules: rules},
	}}}
}

func node(name string, maxInstances int32) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.NodeSpec{MaxInstances: maxInstances},
	}}}
}

func withLabels(o *klitev1.Object, labels map[string]string) *klitev1.Object {
	object.MetaOf(o).Labels = labels
	return o
}

func TestValidate(t *testing.T) {
	web := &klitev1.Container{Name: "web", Image: "img"}
	allow := klitev1.PolicyAction_POLICY_ACTION_ALLOW
	deny := klitev1.PolicyAction_POLICY_ACTION_DENY
	long := strings.Repeat("a", 64)

	pinned := workload("b", 1, web)
	pinned.GetWorkload().Spec.NodeName = "Bad_Node"

	badPort := workload("b", 1, &klitev1.Container{Image: "img", Ports: []*klitev1.Port{{ContainerPort: 700000}}})
	badProbe := workload("b", 1, &klitev1.Container{Image: "img", ReadinessProbe: &klitev1.ReadinessProbe{}})
	badEnv := workload("b", 1, &klitev1.Container{Image: "img", Env: []*klitev1.EnvVar{{Value: "v"}}})
	badTplLabels := workload("b", 1, web)
	badTplLabels.GetWorkload().Spec.Template.Labels = map[string]string{"": "x"}

	badSelector := service("b", 8080, 80)
	badSelector.GetService().Spec.Selector = map[string]string{"app": long}

	tests := []struct {
		name    string
		obj     *klitev1.Object
		wantErr string
	}{
		{"valid workload", workload("b", 2, web), ""},
		{"zero replicas ok", workload("b", 0, web), ""},
		{"negative replicas", workload("b", -1, web), "replicas"},
		{"replicas over cap", workload("b", 1001, web), "replicas"},
		{"bad node pin", pinned, "nodeName"},
		{"no containers", workload("b", 1), "exactly one container"},
		{"two containers", workload("b", 1, web, web), "exactly one container"},
		{"missing image", workload("b", 1, &klitev1.Container{Name: "web"}), "image is required"},
		{"container port out of range", badPort, "containerPort"},
		{"probe port zero", badProbe, "readinessProbe.tcpPort"},
		{"env without name", badEnv, "env 1"},
		{"empty template label key", badTplLabels, "template.labels"},
		{"empty name", workload("", 1, web), "name is required"},
		{"uppercase name", workload("Bad", 1, web), "DNS label"},
		{"underscore name", workload("a_b", 1, web), "DNS label"},
		{"trailing dash name", workload("a-", 1, web), "DNS label"},
		{"name too long", workload(strings.Repeat("a", 64), 1, web), "63"},
		{"labels ok", withLabels(workload("b", 1, web), map[string]string{"app": "b"}), ""},
		{"empty label key", withLabels(workload("b", 1, web), map[string]string{"": "x"}), "label key"},
		{"label key too long", withLabels(workload("b", 1, web), map[string]string{long: "x"}), "exceeds 63"},
		{"label value too long", withLabels(workload("b", 1, web), map[string]string{"app": long}), "exceeds 63"},
		{"valid service", service("b", 8080, 80), ""},
		{"port zero", service("b", 0, 80), "port 0"},
		{"port too big", service("b", 65536, 80), "outside"},
		{"targetPort too big", service("b", 80, 70000), "targetPort"},
		{"selector value too long", badSelector, "spec.selector"},
		{"valid node", node("n1", 32), ""},
		{"negative maxInstances", node("n1", -1), "maxInstances"},
		{"valid allow policy", policy("p", allow, &klitev1.PolicyRule{From: "a", To: "b"}), ""},
		{"wildcards both sides", policy("p", deny, &klitev1.PolicyRule{From: "*", To: "*"}), ""},
		{"except with wildcard to", policy("p", deny, &klitev1.PolicyRule{From: "a", To: "*", Except: []string{"b"}}), ""},
		{"unspecified action", policy("p", klitev1.PolicyAction_POLICY_ACTION_UNSPECIFIED), "ALLOW or DENY"},
		{"empty from", policy("p", deny, &klitev1.PolicyRule{To: "b"}), "from is required"},
		{"empty to", policy("p", deny, &klitev1.PolicyRule{From: "a"}), "to is required"},
		{"except without wildcard", policy("p", deny, &klitev1.PolicyRule{From: "a", To: "b", Except: []string{"c"}}), "except"},
		{"empty envelope", &klitev1.Object{}, "empty object envelope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := object.Validate(tt.obj)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

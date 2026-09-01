package object_test

import (
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

// template uses several map keys so a non-deterministic marshal would flip the hash between runs.
func template(image string) *klitev1.Template {
	return &klitev1.Template{
		Labels: map[string]string{"app": "b", "tier": "web", "zone": "local", "env": "dev"},
		Containers: []*klitev1.Container{{
			Name:  "web",
			Image: image,
			Env: []*klitev1.EnvVar{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "2"},
			},
			Ports: []*klitev1.Port{{ContainerPort: 80}},
		}},
	}
}

// TestTemplateHashGolden pins the hash of a fixed template. If this fails,
// the hash inputs or algorithm changed, and shipping that change re-rolls
// every Workload in every existing cluster (see internal/object/CLAUDE.md).
// Do not update the constant until that is the intent.
func TestTemplateHashGolden(t *testing.T) {
	tpl := &klitev1.Template{
		Labels: map[string]string{"app": "b", "tier": "web", "zone": "local", "env": "dev"},
		Containers: []*klitev1.Container{{
			Name:    "web",
			Image:   "traefik/whoami:v1.10",
			Command: []string{"/bin/whoami"},
			Args:    []string{"--port=80"},
			Env: []*klitev1.EnvVar{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "2"},
			},
			Ports:          []*klitev1.Port{{ContainerPort: 80}},
			Resources:      &klitev1.Resources{Cpus: "0.5", Memory: "128Mi"},
			ReadinessProbe: &klitev1.ReadinessProbe{TcpPort: 80},
		}},
	}
	got, err := object.TemplateHash(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if want := "151e9c6519123fa1"; got != want {
		t.Errorf("full template hash = %s, want %s", got, want)
	}
	empty, err := object.TemplateHash(&klitev1.Template{})
	if err != nil {
		t.Fatal(err)
	}
	// An empty template marshals to zero bytes, so its hash is the FNV-1a
	// offset basis.
	if want := "cbf29ce484222325"; empty != want {
		t.Errorf("empty template hash = %s, want %s", empty, want)
	}
}

func TestTemplateHashStability(t *testing.T) {
	want, err := object.TemplateHash(template("img:1"))
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		t.Fatal("empty hash")
	}
	for i := range 100 {
		got, err := object.TemplateHash(template("img:1"))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("hash flipped on iteration %d: %s then %s", i, want, got)
		}
	}
}

func TestTemplateHashDiffers(t *testing.T) {
	tests := []struct {
		name string
		a    *klitev1.Template
		b    *klitev1.Template
	}{
		{"image change", template("img:1"), template("img:2")},
		{"label change", template("img:1"), func() *klitev1.Template {
			tpl := template("img:1")
			tpl.Labels["zone"] = "remote"
			return tpl
		}()},
		{"env change", template("img:1"), func() *klitev1.Template {
			tpl := template("img:1")
			tpl.Containers[0].Env[0].Value = "9"
			return tpl
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ha, err := object.TemplateHash(tt.a)
			if err != nil {
				t.Fatal(err)
			}
			hb, err := object.TemplateHash(tt.b)
			if err != nil {
				t.Fatal(err)
			}
			if ha == hb {
				t.Errorf("different templates hashed alike: %s", ha)
			}
		})
	}
}

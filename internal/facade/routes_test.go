package facade

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The fake answers Unimplemented for the one-shot RPCs, so a 501 proves the
// route reached its handler and the gRPC status mapping ran.
func TestActionRoutesAreWired(t *testing.T) {
	srv := newTestServer()
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/workloads/b/scale", `{"replicas":3}`, 501},
		{"POST", "/api/workloads/b/scale", `nonsense`, 400},
		{"POST", "/api/nodes/node-1/uncordon", "", 501},
		{"GET", "/api/nodetoken", "", 501},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("got %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

package facade

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// tokenFake mints tokens, declares node-9 and node-10, and leaves every
// other RPC Unimplemented.
type tokenFake struct{ *fakeClient }

func (tokenFake) NodeToken(context.Context, *klitev1.NodeTokenRequest, ...grpc.CallOption) (*klitev1.NodeTokenResponse, error) {
	return &klitev1.NodeTokenResponse{Token: "tok-1"}, nil
}

func (tokenFake) List(_ context.Context, req *klitev1.ListRequest, _ ...grpc.CallOption) (*klitev1.ListResponse, error) {
	if req.GetName() == "node-9" || req.GetName() == "node-10" {
		return &klitev1.ListResponse{Objects: []*klitev1.Object{{Kind: &klitev1.Object_Node{Node: &klitev1.Node{Meta: &klitev1.Meta{Name: req.GetName()}}}}}}, nil
	}
	return &klitev1.ListResponse{}, nil
}

// stubAgent stands in for klite-agent: it records its arguments and stays
// alive long enough for the double-join guard to see it.
func stubAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "klite-agent")
	script := "#!/bin/sh\necho \"$@\" > \"" + filepath.Join(dir, "args") + "\"\nsleep 30\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func postJoin(t *testing.T, srv *Server, node string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/nodes/"+node+"/join", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestJoinSpawnsAgent(t *testing.T) {
	bin := stubAgent(t)
	srv := New(tokenFake{&fakeClient{}}, []string{"127.0.0.1:7443"}, "", false, nil)
	srv.EnableLocalJoin(bin, t.TempDir())

	rec := postJoin(t, srv, "node-9")
	if rec.Code != 200 {
		t.Fatalf("join: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Pid int    `json:"pid"`
		Log string `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Pid <= 0 {
		t.Fatalf("join response %q: pid missing (err %v)", rec.Body.String(), err)
	}
	t.Cleanup(func() { _ = syscall.Kill(out.Pid, syscall.SIGKILL) })

	// Under full-suite load the stub can take seconds to get scheduled, so
	// the wait is a deadline, not a fixed spin count.
	argsPath := filepath.Join(filepath.Dir(bin), "args")
	var args string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(argsPath); err == nil {
			args = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	want := "--node node-9 --server 127.0.0.1:7443 --token tok-1"
	if args != want {
		t.Fatalf("agent argv: got %q, want %q", args, want)
	}

	if rec := postJoin(t, srv, "node-9"); rec.Code != 409 {
		t.Fatalf("second join: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	rec2 := postJoin(t, srv, "node-10")
	if rec2.Code != 200 {
		t.Fatalf("join of a different node: got %d (%s)", rec2.Code, rec2.Body.String())
	}
	var other struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &other); err == nil && other.Pid > 0 {
		t.Cleanup(func() { _ = syscall.Kill(other.Pid, syscall.SIGKILL) })
	}
}

func TestJoinUndeclaredNodeIs404(t *testing.T) {
	srv := New(tokenFake{&fakeClient{}}, []string{"127.0.0.1:7443"}, "", false, nil)
	srv.EnableLocalJoin(stubAgent(t), t.TempDir())
	rec := postJoin(t, srv, "node-99")
	if rec.Code != 404 {
		t.Fatalf("got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestNodeTokenListsMachineAddresses(t *testing.T) {
	srv := New(tokenFake{&fakeClient{}}, []string{"127.0.0.1:7443"}, "", false, nil)
	req := httptest.NewRequest("GET", "/api/nodetoken", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Token            string   `json:"token"`
		MachineAddresses []string `json:"machineAddresses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-1" {
		t.Fatalf("token %q", out.Token)
	}
	for _, a := range out.MachineAddresses {
		if strings.HasPrefix(a, "127.") {
			t.Fatalf("loopback %s offered as a machine address", a)
		}
	}
}

func TestTailnetAddressPicksCGNATOnly(t *testing.T) {
	mk := func(ip string, bits int) net.Addr {
		return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(bits, 32)}
	}
	got := tailnetAddressFrom([]net.Addr{mk("192.168.1.20", 24), mk("100.20.1.5", 16), mk("100.69.43.39", 10)})
	if got != "100.69.43.39" {
		t.Fatalf("got %q, want the CGNAT address", got)
	}
	if got := tailnetAddressFrom([]net.Addr{mk("192.168.1.20", 24)}); got != "" {
		t.Fatalf("a LAN-only machine claimed a tailnet address: %q", got)
	}
}

func TestJoinWithoutSpawnerIs501(t *testing.T) {
	srv := New(tokenFake{&fakeClient{}}, []string{"127.0.0.1:7443"}, "", false, nil)
	if rec := postJoin(t, srv, "node-9"); rec.Code != 501 {
		t.Fatalf("got %d, want 501", rec.Code)
	}
}

func TestJoinMissingBinaryIs424(t *testing.T) {
	srv := New(tokenFake{&fakeClient{}}, []string{"127.0.0.1:7443"}, "", false, nil)
	srv.EnableLocalJoin(filepath.Join(t.TempDir(), "nope"), t.TempDir())
	rec := postJoin(t, srv, "node-9")
	if rec.Code != 424 {
		t.Fatalf("got %d, want 424 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "klite-agent") {
		t.Fatalf("error should name the missing binary: %s", rec.Body.String())
	}
}

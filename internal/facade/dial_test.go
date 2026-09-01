package facade

import "testing"

// The manual resolver must pin TLS verification to each endpoint's own host.
// Without ServerName, gRPC verifies klited's certificate against the target
// URI's placeholder authority and every backed route hangs on WaitForReady —
// the exact failure the M10 demo run hit.
func TestEndpointAddressesPinServerName(t *testing.T) {
	addrs := endpointAddresses([]string{"127.0.0.1:7443", "klited.example.com:7443", "bare-host"})
	want := []struct{ addr, serverName string }{
		{"127.0.0.1:7443", "127.0.0.1"},
		{"klited.example.com:7443", "klited.example.com"},
		{"bare-host", "bare-host"},
	}
	if len(addrs) != len(want) {
		t.Fatalf("got %d addresses, want %d", len(addrs), len(want))
	}
	for i, w := range want {
		if addrs[i].Addr != w.addr || addrs[i].ServerName != w.serverName {
			t.Errorf("addrs[%d] = {%q, %q}, want {%q, %q}", i, addrs[i].Addr, addrs[i].ServerName, w.addr, w.serverName)
		}
	}
}

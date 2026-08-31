//go:build !linux

package netd

import (
	"errors"
	"net/netip"
)

// bindVIPs is linux-only (netlink). The binary only ever runs on linux in a
// container, so this stub exists to keep `go build ./...` working on darwin.
func bindVIPs(_ string, _ []netip.Addr) (int, error) {
	return 0, errors.New("vip reconciliation unsupported on this platform")
}

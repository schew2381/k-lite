//go:build linux

package netd

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
)

// bindVIPs reconciles the /32s on iface inside vipPool to exactly want:
// adds missing, deletes stale. Addresses outside the pool (the primary IP)
// are never touched. Returns how many wanted VIPs are bound.
func bindVIPs(iface string, want []netip.Addr) (int, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return 0, fmt.Errorf("link %s: %w", iface, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return 0, fmt.Errorf("addr list %s: %w", iface, err)
	}

	have := map[netip.Addr]bool{}
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP.To4())
		if ones, bits := a.Mask.Size(); !ok || ones != bits || !vipPool.Contains(ip) {
			continue
		}
		have[ip] = true
	}

	wanted := map[netip.Addr]bool{}
	bound := 0
	var errs []error
	for _, ip := range want {
		wanted[ip] = true
		if have[ip] {
			bound++
			continue
		}
		if err := netlink.AddrAdd(link, vipAddr(ip)); err != nil && !errors.Is(err, os.ErrExist) {
			errs = append(errs, fmt.Errorf("add %s: %w", ip, err))
			continue
		}
		bound++
	}
	for ip := range have {
		if wanted[ip] {
			continue
		}
		if err := netlink.AddrDel(link, vipAddr(ip)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("del %s: %w", ip, err))
		}
	}
	return bound, errors.Join(errs...)
}

func vipAddr(ip netip.Addr) *netlink.Addr {
	return &netlink.Addr{IPNet: &net.IPNet{IP: ip.AsSlice(), Mask: net.CIDRMask(32, 32)}}
}

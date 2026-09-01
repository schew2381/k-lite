package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
)

// advertise.go resolves --advertise-address into the literal IP other nodes'
// proxies dial for this node's ingress ports (ADR 0024). EDS carries only
// IPs, so a hostname flag has to become one before it leaves the node. The
// default, host.docker.internal, is a name only containers can resolve: it
// exists in the donor's /etc/hosts, where dockerd wrote the host-gateway
// address at create time, and nowhere else on a Mac. Real DNS names fall
// back to the host resolver.

// literalAdvertiseIP accepts the flag right away when it's already a usable
// IP, so Register can carry it before any container exists. Loopback would
// send every remote consumer to itself and is refused here and everywhere.
func literalAdvertiseIP(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil || ip.IsLoopback() {
		return ""
	}
	return addr
}

// currentAdvertiseIP is the resolved address, or "" while resolution is
// still pending (or no flag was given).
func (a *Agent) currentAdvertiseIP() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.advertiseIP
}

// ensureAdvertiseIP resolves the flag once the donor exists, then kicks a
// report so the address reaches NodeStatus without waiting a full heartbeat.
// Failures retry on the next net tick.
func (a *Agent) ensureAdvertiseIP(ctx context.Context) {
	if a.advertiseFlag == "" || a.currentAdvertiseIP() != "" {
		return
	}
	ip, err := a.resolveAdvertise(ctx, a.advertiseFlag)
	if err != nil {
		slog.Warn("advertise address not resolved yet", "addr", a.advertiseFlag, "err", err)
		return
	}
	a.mu.Lock()
	a.advertiseIP = ip
	a.mu.Unlock()
	slog.Info("advertise address resolved", "addr", a.advertiseFlag, "ip", ip)
	kick(a.kickReport)
}

// resolveAdvertise turns a hostname flag into an IP. The donor's /etc/hosts
// wins over host DNS: it holds what containers actually dial (the pinned
// host-gateway line), while the host resolver can't see that pin and, on
// some setups, answers host.docker.internal with poisonous loopback.
func (a *Agent) resolveAdvertise(ctx context.Context, addr string) (string, error) {
	if ip := literalAdvertiseIP(addr); ip != "" {
		return ip, nil
	}
	if _, err := netip.ParseAddr(addr); err == nil {
		return "", fmt.Errorf("%s is loopback; advertise an address other machines can dial", addr)
	}
	hosts, err := a.rt.ReadContainerFile(ctx, a.netContainerName(), "/etc/hosts")
	if err == nil {
		if ip, ok := hostsLookup(hosts, addr); ok {
			return ip, nil
		}
	}
	ips, lookupErr := net.DefaultResolver.LookupNetIP(ctx, "ip4", addr)
	if lookupErr != nil {
		return "", fmt.Errorf("not in the donor's /etc/hosts and host DNS failed: %w", lookupErr)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.Unmap().String(), nil
		}
	}
	return "", fmt.Errorf("%s resolves only to loopback or link-local addresses", addr)
}

// hostsLookup scans /etc/hosts content for a usable address bound to host.
func hostsLookup(content []byte, host string) (string, bool) {
	for line := range strings.SplitSeq(string(content), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip, err := netip.ParseAddr(fields[0])
		if err != nil || ip.IsLoopback() {
			continue
		}
		for _, name := range fields[1:] {
			if strings.EqualFold(name, host) {
				return ip.Unmap().String(), true
			}
		}
	}
	return "", false
}

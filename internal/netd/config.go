// Package netd is the per-node network daemon: DNS for svc.klite, service
// VIPs on the infra pod's interface, and TCP readiness probes (ADRs 0006,
// 0008, 0017).
package netd

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// vipPool is the klited-allocated VIP range. Reconciliation never touches
// addresses outside it, which keeps the container's primary IP safe.
var vipPool = netip.MustParsePrefix("10.44.64.0/18")

// serviceLabel is the DNS-label grammar klited enforces on Service names.
// Anything outside it (a dot, a space, 64+ characters) would build a vips
// key no wire-format query can ever match, so the push gets rejected loudly
// instead of resolving to silent NXDOMAINs.
var serviceLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type probeTarget struct {
	instance string
	ip       string
	port     int32
	addr     string
}

// netConfig is an immutable snapshot of one ApplyConfig push. Handlers read
// it through an atomic pointer, and every push builds a fresh value.
type netConfig struct {
	serial  uint32
	vips    map[string]netip.Addr // lowercased service name -> VIP
	vipList []netip.Addr
	targets []probeTarget
}

func emptyConfig() *netConfig {
	return &netConfig{vips: map[string]netip.Addr{}}
}

func parseConfig(desired *klitev1.NetDesired, serial uint32) (*netConfig, error) {
	cfg := &netConfig{serial: serial, vips: map[string]netip.Addr{}}
	for _, svc := range desired.GetServices() {
		if svc.GetService() == "" {
			return nil, fmt.Errorf("service with empty name (vip %q)", svc.GetVip())
		}
		name := strings.ToLower(svc.GetService())
		if !serviceLabel.MatchString(name) {
			return nil, fmt.Errorf("service %q is not a DNS label", svc.GetService())
		}
		if _, dup := cfg.vips[name]; dup {
			return nil, fmt.Errorf("service %q listed twice", name)
		}
		vip, err := netip.ParseAddr(svc.GetVip())
		if err != nil {
			return nil, fmt.Errorf("service %s: vip: %w", svc.GetService(), err)
		}
		if !vip.Is4() || !vipPool.Contains(vip) {
			return nil, fmt.Errorf("service %s: vip %s outside pool %s", svc.GetService(), vip, vipPool)
		}
		cfg.vips[name] = vip
	}
	seen := map[netip.Addr]bool{}
	for _, vip := range cfg.vips {
		if !seen[vip] {
			seen[vip] = true
			cfg.vipList = append(cfg.vipList, vip)
		}
	}
	for _, t := range desired.GetProbeTargets() {
		cfg.targets = append(cfg.targets, probeTarget{
			instance: t.GetInstance(),
			ip:       t.GetIp(),
			port:     t.GetPort(),
			addr:     net.JoinHostPort(t.GetIp(), strconv.Itoa(int(t.GetPort()))),
		})
	}
	return cfg, nil
}

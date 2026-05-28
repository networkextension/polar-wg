package wg

// hub_local_routes.go — best-effort iface address + route snapshot for
// the hub-local poll, so the admin UI can spot misconfigurations like
// "peers handshake but hub can't reply" (missing subnet route → TX=0
// on every peer even though RX flows in).
//
// Cross-platform: Darwin uses `ifconfig` + `netstat -rn -f inet`;
// Linux uses `ip -4 addr show` + `ip -4 route show dev`. Everything
// is best-effort — parser failures return empty rather than block
// the main sample. The fields land under the report's `iface_net`
// key so older UI builds simply ignore them.

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
)

// ifaceNetInfo is what we attach to each hub-status sample. UI joins
// `addrs` ∩ `routes` to flag missing subnet routes (the case where a
// hub has 10.88.0.1/8 assigned but no 10.88.0.0/24 → iface entry).
type ifaceNetInfo struct {
	// Addrs — strings like "10.88.0.1/24" pulled from the iface itself.
	Addrs []string `json:"addrs"`
	// Routes — strings like "10.88.0.0/24" (destination CIDR only) for
	// every kernel route whose output interface is `iface`. The
	// iface's own /32 self-route is filtered out (noise).
	Routes []string `json:"routes"`
}

// collectIfaceNetInfo returns address + route info for `iface`.
// Always returns a non-nil struct; empty slices on any failure.
func collectIfaceNetInfo(iface string) ifaceNetInfo {
	info := ifaceNetInfo{Addrs: []string{}, Routes: []string{}}
	if iface == "" {
		return info
	}
	if addrs := collectIfaceAddrs(iface); len(addrs) > 0 {
		info.Addrs = addrs
	}
	if routes := collectIfaceRoutes(iface); len(routes) > 0 {
		info.Routes = routes
	}
	return info
}

// collectIfaceAddrs uses net.Interfaces() — portable, no shelling out.
// Skips IPv6 + link-local; emits "10.88.0.1/24" form.
func collectIfaceAddrs(iface string) []string {
	out := []string{}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return out
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		out = append(out, fmt.Sprintf("%s/%d", ip4.String(), ones))
	}
	return out
}

// collectIfaceRoutes shells to the platform's route-listing tool and
// keeps destination CIDRs whose output iface is `iface`. The iface's
// self-route (e.g. 10.88.0.1/32 → utun0) is filtered because it tells
// us nothing about subnet reachability.
func collectIfaceRoutes(iface string) []string {
	switch runtime.GOOS {
	case "darwin":
		return collectIfaceRoutesDarwin(iface)
	case "linux":
		return collectIfaceRoutesLinux(iface)
	default:
		return nil
	}
}

func collectIfaceRoutesDarwin(iface string) []string {
	out, err := exec.Command("netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return nil
	}
	// netstat -rn columns: Destination Gateway Flags Netif Expire
	// e.g. "10.88.0.1  10.88.0.1  UH  utun0"
	// e.g. "10.88.0/24  10.88.0.1  UGSc  utun0"
	routes := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		// Netif is column 4 (0-indexed 3).
		if f[3] != iface {
			continue
		}
		dst := normalizeRouteDst(f[0])
		if dst == "" {
			continue
		}
		// Filter the iface's own /32 self-route — caller already knows
		// the iface address from Addrs[].
		if strings.HasSuffix(dst, "/32") {
			continue
		}
		routes = append(routes, dst)
	}
	return routes
}

func collectIfaceRoutesLinux(iface string) []string {
	out, err := exec.Command("ip", "-4", "route", "show", "dev", iface).Output()
	if err != nil {
		return nil
	}
	// `ip route show dev wg0` rows: "10.88.0.0/24 proto kernel scope link src 10.88.0.1"
	routes := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		dst := normalizeRouteDst(f[0])
		if dst == "" || strings.HasSuffix(dst, "/32") {
			continue
		}
		routes = append(routes, dst)
	}
	return routes
}

// normalizeRouteDst turns netstat's shorthand ("10.88.0/24",
// "10/8", "default") into RFC-CIDR form ("10.88.0.0/24",
// "10.0.0.0/8", "0.0.0.0/0") so the UI can do prefix math.
func normalizeRouteDst(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if s == "default" {
		return "0.0.0.0/0"
	}
	// Already a CIDR with full octets?
	if _, _, err := net.ParseCIDR(s); err == nil {
		return s
	}
	// Try to pad short-form like "10.88.0/24" → "10.88.0.0/24".
	if i := strings.Index(s, "/"); i > 0 {
		host := s[:i]
		mask := s[i:]
		octets := strings.Split(host, ".")
		for len(octets) < 4 {
			octets = append(octets, "0")
		}
		padded := strings.Join(octets, ".") + mask
		if _, _, err := net.ParseCIDR(padded); err == nil {
			return padded
		}
		return ""
	}
	// Bare IP — return as /32.
	if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
		return ip.String() + "/32"
	}
	return ""
}

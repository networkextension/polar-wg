package wg

// /internal/v1/wg-devices/link — dock posts here from its WS agent_hub
// when a polar-agent `hello` frame carries host_info.wg_pubkeys. It
// stamps wg_devices.host_id on the rows whose pubkey the agent reported,
// lighting up the WG→Hosts UI cross-link (doc/arch/wg-host-crosslink.md,
// P2). The agent is the authoritative source of the (host_id ↔ pubkey)
// pairing — it knows both locally — so this replaces the unreliable
// hostname-matching backfill.
//
// Auth: loopback-only, same gate + rationale as polar-hosts'
// /internal/v1/hosts/hello (dock + wg-svc run on the same host by deploy
// convention; nginx does not proxy /internal/*). No HMAC.

import (
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type internalWGLinkReq struct {
	HostID  string   `json:"host_id"`
	Pubkeys []string `json:"pubkeys"`
	// DeviceIPs are the agent's mesh IPs (host_info.ipv4_by_iface values).
	// Used to stamp host_id by wg_devices.device_ip — the NE-proof path,
	// since wg-mac NE boxes don't expose a readable WG public key.
	DeviceIPs []string `json:"device_ips"`
}

func (p *Plugin) handleInternalWGDeviceLink(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	var req internalWGLinkReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	hostID := strings.TrimSpace(req.HostID)
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_id required"})
		return
	}
	linked := 0
	for _, pk := range req.Pubkeys {
		pk = strings.TrimSpace(pk)
		if pk == "" {
			continue
		}
		n, err := p.linkWGDeviceHostByPubkey(pk, hostID)
		if err != nil {
			// Best-effort: log and keep going. An unknown pubkey (device
			// not in this hub's DB) is not an error — surface via count.
			log.Printf("internal/v1/wg-devices/link: pubkey=%s host=%s: %v", pk, hostID, err)
			continue
		}
		linked += n
	}
	// NE-proof path: match by the agent's mesh IP against wg_devices.device_ip.
	// Only the box's mesh IP (10.88.x / 100.64.x) will match a device row;
	// LAN IPs in the same list simply hit nothing.
	for _, ip := range req.DeviceIPs {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		n, err := p.linkWGDeviceHostByDeviceIP(ip, hostID)
		if err != nil {
			log.Printf("internal/v1/wg-devices/link: device_ip=%s host=%s: %v", ip, hostID, err)
			continue
		}
		linked += n
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "host_id": hostID, "linked": linked})
}

// isLoopbackRequest reports whether the request originated on 127.0.0.0/8
// or ::1. Mirrors the helper in polar-hosts; gate for /internal/* routes.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

package wg

// HTTP layer for wg-mac Phase 2 join control plane.
//
// Public /v1/* — see JOIN_PROTOCOL.md §2:
//   POST /v1/register         token-in-body auth → returns role + hub info
//   GET  /v1/peers            Bearer device-token + X-Device-Id (per-hub scope)
//   GET  /v1/hub/peers        Bearer hub-device-token + X-Device-Id (hub-only)
//   POST /v1/heartbeat        Bearer device-token + X-Device-Id
//   POST /v1/leave            Bearer device-token + X-Device-Id (hub → also clears wg_hup.pubkey)
//   POST /v1/token/refresh    Bearer device-token + X-Device-Id
//   GET  /v1/install          unauthenticated
//   GET  /v1/install/:version unauthenticated
//   GET  /v1/bundle           unauthenticated
//   GET  /v1/bundle/:version  unauthenticated
//
// Admin /api/admin/wg-* — AuthMiddleware + AdminMiddleware.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- helpers ----

func wgServerBaseURL(c *gin.Context) string {
	scheme := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto"))
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

// ---- /v1/register ----

type wgRegisterRequest struct {
	Token    string      `json:"token" binding:"required"`
	Pubkey   string      `json:"pubkey" binding:"required"`
	Hostname string      `json:"hostname"`
	HostID   string      `json:"host_id"` // optional: polar-hosts host.id, for UI cross-link
	OS       string      `json:"os"`
	Arch     string      `json:"arch"`
	AgentVer string      `json:"agent_ver"`
	LANAddrs []WGLanAddr `json:"lan_addrs"`
	WGListen int         `json:"wg_listen"`
	SiteSlug string      `json:"site_slug"`
}

type wgPeerResponse struct {
	Pubkey       string   `json:"pubkey"`
	WGIP         string   `json:"wg_ip,omitempty"`
	Endpoint     string   `json:"endpoint"`
	SiteSlug     string   `json:"site_id,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	AllowedExtra []string `json:"allowed_extra,omitempty"`
}

type wgHubResponse struct {
	Slug     string `json:"slug"`
	Pubkey   string `json:"pubkey"`
	Endpoint string `json:"endpoint"`
	WGIP     string `json:"wg_ip"`
}

type wgRegisterResponse struct {
	DeviceID     string           `json:"device_id"`
	DeviceIP     string           `json:"device_ip"`
	SiteSlug     string           `json:"site_id"`
	HubSlug      string           `json:"hub_slug"`
	Role         string           `json:"role"` // "hub" | "device"
	MeshCIDR     string           `json:"mesh_cidr"`
	Hub          wgHubResponse    `json:"hub"`
	Peers        []wgPeerResponse `json:"peers"`
	KeepaliveSec int              `json:"keepalive_sec"`
	RefreshSec   int              `json:"refresh_sec"`
	// Policy — DNS/proxy settings the device applies to its tunnel network
	// settings (mobile). Per-hub. See doc/wg-dns-proxy-push-design.md.
	Policy       *WGPolicy  `json:"policy,omitempty"`
	Token        string     `json:"token,omitempty"`
	TokenExpires *time.Time `json:"token_expires,omitempty"`
}

func (p *Plugin) handleWGRegister(c *gin.Context) {
	var req wgRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	publicIP := extractPublicIP(
		c.GetHeader("X-Forwarded-For"),
		c.GetHeader("X-Real-IP"),
		c.Request.RemoteAddr,
	)
	in := wgRegisterInput{
		Token:        req.Token,
		Pubkey:       req.Pubkey,
		Hostname:     req.Hostname,
		HostID:       req.HostID,
		OS:           req.OS,
		Arch:         req.Arch,
		AgentVer:     req.AgentVer,
		LANAddrs:     req.LANAddrs,
		WGListenPort: req.WGListen,
		SiteSlug:     req.SiteSlug,
		PublicIP:     publicIP,
	}
	res, err := p.allocateWGDevice(c.Request.Context(), in)
	if err != nil {
		result := "error"
		switch {
		case errors.Is(err, errWGTokenInvalid), errors.Is(err, errWGTokenExpired), errors.Is(err, errWGTokenRevoked), errors.Is(err, errWGTokenNoHub):
			result = "invalid_token"
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		case errors.Is(err, errWGTokenAlreadyBound):
			result = "token_already_bound"
			c.JSON(http.StatusConflict, gin.H{"error": "token_already_bound"})
		case errors.Is(err, errWGPubkeyTaken):
			result = "pubkey_already_registered"
			c.JSON(http.StatusConflict, gin.H{"error": "pubkey_already_registered"})
		case errors.Is(err, errWGHubAlreadyBound):
			result = "hub_already_bound"
			c.JSON(http.StatusConflict, gin.H{"error": "hub_already_bound"})
		case errors.Is(err, errWGHubNotConfigured):
			result = "hub_not_configured"
			c.JSON(http.StatusFailedDependency, gin.H{"error": "hub_not_configured", "hint": "ask operator to run the role=hub install first"})
		case errors.Is(err, errWGSiteExhausted), errors.Is(err, errWGSiteSlotsExhausted):
			result = "site_exhausted"
			c.JSON(http.StatusInsufficientStorage, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "register failed: " + err.Error()})
		}
		// role is unknown on early-token-validation failures; report as
		// "unknown" so the dashboard still ticks the total.
		p.metrics.recordWGRegister("unknown", result)
		return
	}
	resp, err := p.buildPeerListResponse(res.Device, res.Hub)
	if err != nil {
		p.metrics.recordWGRegister(res.Role, "error")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "peer list: " + err.Error()})
		return
	}
	resp.DeviceID = res.Device.DeviceID
	resp.Role = res.Role
	resp.Token = res.PlaintextToken
	resp.TokenExpires = res.TokenExpiresAt
	p.metrics.recordWGRegister(res.Role, "ok")
	c.JSON(http.StatusOK, resp)
}

// ---- /v1/peers ----

func (p *Plugin) extractDeviceBearer(c *gin.Context) (*WGDevice, bool) {
	raw := extractBearerToken(c)
	if raw == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return nil, false
	}
	devIDClaimed := strings.TrimSpace(c.GetHeader("X-Device-Id"))
	if devIDClaimed == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing X-Device-Id"})
		return nil, false
	}
	dev, err := p.getWGDeviceByTokenHash(hashWGToken(raw))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return nil, false
	}
	if dev == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
		return nil, false
	}
	if dev.DeviceID != devIDClaimed {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token does not match X-Device-Id"})
		return nil, false
	}
	if dev.TokenExpiresAt != nil && time.Now().UTC().After(*dev.TokenExpiresAt) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
		return nil, false
	}
	return dev, true
}

func (p *Plugin) handleWGPeers(c *gin.Context) {
	dev, ok := p.extractDeviceBearer(c)
	if !ok {
		return
	}
	hub, err := p.getWGHubByID(dev.HubID)
	if err != nil || hub == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hub lookup failed"})
		return
	}
	resp, err := p.buildPeerListResponse(dev, hub)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// buildPeerListResponse computes the per-device peer list scoped to
// the device's hub. LAN-direct peers (same site) + hub peer.
func (p *Plugin) buildPeerListResponse(dev *WGDevice, hub *WGHub) (*wgRegisterResponse, error) {
	if hub == nil {
		return nil, fmt.Errorf("nil hub")
	}
	lanPeers, err := p.listWGDevicesInSite(dev.SiteID, dev.ID)
	if err != nil {
		return nil, fmt.Errorf("lan peers: %w", err)
	}
	peerOut := make([]wgPeerResponse, 0, len(lanPeers)+1)
	for _, p := range lanPeers {
		endpoint := ""
		if len(p.LANAddrs) > 0 {
			ip := firstLANIP(p.LANAddrs[0].CIDR)
			if ip != "" {
				endpoint = fmt.Sprintf("%s:%d", ip, p.WGListenPort)
			}
		}
		peerOut = append(peerOut, wgPeerResponse{
			Pubkey:   p.Pubkey,
			WGIP:     p.DeviceIP,
			Endpoint: endpoint,
			Hostname: p.Hostname,
		})
	}
	isHubSelf := hub.BoundDeviceID != nil && *hub.BoundDeviceID == dev.ID
	allHubs, err := p.listWGHubs()
	if err != nil {
		return nil, fmt.Errorf("list hubs: %w", err)
	}
	// Cross-hub routes are operator-published, not auto-meshed: only hubs the
	// operator has explicitly linked to this hub get their /24 distributed.
	linkedHubIDs, err := p.hubLinkSet(hub.ID)
	if err != nil {
		return nil, fmt.Errorf("hub links: %w", err)
	}
	if hub.Configured() && !isHubSelf {
		// Spoke: one hub peer. AllowedIPs = own hub's /24 + every OTHER
		// hub's /24 — cross-hub traffic routes via the own hub, which
		// forwards into the hub-to-hub fabric. Client-transparent:
		// allowed_extra is already honored for the own-hub /24.
		allowedExtra := make([]string, 0, 4)
		if mesh, err := parseMeshCIDR(hub.MeshCIDR); err == nil {
			allowedExtra = append(allowedExtra, mesh.ipnet.String())
		}
		// Drop a cross-hub /24 for any mesh this spoke's host already joins
		// directly (dual-homed box): otherwise its two wg interfaces collide
		// on the same /24 route and the second wg-quick up fails.
		skipHubIDs, err := p.hubIDsForHost(dev.HostID, dev.Hostname)
		if err != nil {
			return nil, fmt.Errorf("host hub lookup: %w", err)
		}
		allowedExtra = append(allowedExtra, crossHubAllowedExtra(allHubs, hub.ID, hub.MeshCIDR, linkedHubIDs, skipHubIDs)...)
		// P2 egress opt-in: the egress hub's advertised routes ride the
		// own-hub peer too (cross-hub traffic transits the fabric).
		allowedExtra = append(allowedExtra, egressAllowedExtra(dev.EgressHubID, hub, allHubs)...)
		peerOut = append(peerOut, wgPeerResponse{
			Pubkey:       hub.Pubkey,
			WGIP:         hub.WGIP,
			Endpoint:     p.hubEndpointFor(hub, dev),
			SiteSlug:     "hub",
			AllowedExtra: allowedExtra,
		})
	} else if isHubSelf {
		// Hub itself: every OTHER configured hub becomes a direct peer
		// (full mesh among public-IP hubs). This puts the fabric into the
		// hub's INITIAL conf at install time — the install script's
		// renderer already handles endpoint + allowed_extra.
		for _, e := range otherConfiguredHubPeers(allHubs, hub.ID, hub.MeshCIDR, linkedHubIDs) {
			peerOut = append(peerOut, wgPeerResponse{
				Pubkey:       e.Pubkey,
				WGIP:         strings.TrimSuffix(e.WGIP, "/32"),
				Endpoint:     e.Endpoint,
				SiteSlug:     e.Hostname, // "hub:<slug>"
				AllowedExtra: e.AllowedExtra,
			})
		}
	}
	return &wgRegisterResponse{
		DeviceID: dev.DeviceID,
		DeviceIP: dev.DeviceIP,
		SiteSlug: dev.SiteSlug,
		HubSlug:  hub.Slug,
		MeshCIDR: hub.MeshCIDR,
		Hub: wgHubResponse{
			Slug:     hub.Slug,
			Pubkey:   hub.Pubkey,
			Endpoint: hub.Endpoint,
			WGIP:     hub.WGIP,
		},
		Peers:        peerOut,
		KeepaliveSec: hub.KeepaliveSec,
		RefreshSec:   hub.RefreshSec,
		Policy:       hub.Policy,
		TokenExpires: dev.TokenExpiresAt,
	}, nil
}

func firstLANIP(cidr string) string {
	if i := strings.Index(cidr, "/"); i > 0 {
		return cidr[:i]
	}
	return cidr
}

// ---- cross-hub routing (multi-hub mesh) ----
//
// Each hub owns one disjoint /24 (mesh_cidr). The two helpers below build
// the routes that interconnect hubs:
//   - crossHubAllowedExtra widens a SPOKE's own-hub peer so cross-hub /24s
//     route to (and are forwarded by) the spoke's own hub.
//   - otherConfiguredHubPeers gives a HUB the other hubs as direct peers
//     (full mesh among public-IP hubs).
// Both are pure (no DB) so they unit-test without Postgres. Both skip the
// own hub, hubs not yet bound (no pubkey) or without a public endpoint, and
// any hub whose /24 duplicates one already emitted (defensive against
// overlapping mesh_cidrs — see suggestFreeMeshCIDR for the disjoint guarantee).

// hubMeshNetwork normalizes a hub's mesh_cidr to its network address
// ("100.64.1.5/24" → "100.64.1.0/24"), or "" if unparseable. Mirrors the
// idiom used inline in buildPeerListResponse.
func hubMeshNetwork(meshCIDR string) string {
	if m, err := parseMeshCIDR(meshCIDR); err == nil {
		return m.ipnet.String()
	}
	return ""
}

// otherConfiguredHubs is the shared filter: every OTHER hub usable as a
// cross-hub peer (bound + public endpoint + /24 not seen yet, seeding
// the dedup set with the own hub's /24).
func otherConfiguredHubs(allHubs []WGHub, ownHubID int64, ownCIDR string) []WGHub {
	out := make([]WGHub, 0, len(allHubs))
	seen := map[string]bool{}
	if own := hubMeshNetwork(ownCIDR); own != "" {
		seen[own] = true
	}
	for _, h := range allHubs {
		if h.ID == ownHubID || !h.Configured() || strings.TrimSpace(h.Endpoint) == "" {
			continue
		}
		n := hubMeshNetwork(h.MeshCIDR)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, h)
	}
	return out
}

// crossHubAllowedExtra widens a spoke's own-hub peer with another hub's /24 so
// cross-hub traffic routes via the own hub into the fabric. Routes are NOT
// auto-distributed: a hub's /24 is handed out only when the operator has
// PUBLISHED a link between the two hubs (linkedHubIDs = hubs linked to ownHub).
// skipHubIDs additionally drops any hub whose mesh the spoke's HOST already
// joins directly (a dual-homed box), which would otherwise collide on the same
// /24 route. linkedHubIDs nil/empty → no cross-hub routes at all.
func crossHubAllowedExtra(allHubs []WGHub, ownHubID int64, ownCIDR string, linkedHubIDs, skipHubIDs map[int64]bool) []string {
	hubs := otherConfiguredHubs(allHubs, ownHubID, ownCIDR)
	out := make([]string, 0, len(hubs))
	for _, h := range hubs {
		if !linkedHubIDs[h.ID] || skipHubIDs[h.ID] {
			continue
		}
		out = append(out, hubMeshNetwork(h.MeshCIDR))
	}
	return out
}

// otherConfiguredHubPeers gives a HUB the other hubs as direct fabric peers —
// but only those the operator has PUBLISHED a link to (linkedHubIDs). Empty
// set → no fabric peers (the hub stays standalone until links are published).
func otherConfiguredHubPeers(allHubs []WGHub, ownHubID int64, ownCIDR string, linkedHubIDs map[int64]bool) []wgHubPeerEntry {
	hubs := otherConfiguredHubs(allHubs, ownHubID, ownCIDR)
	out := make([]wgHubPeerEntry, 0, len(hubs))
	for _, h := range hubs {
		if !linkedHubIDs[h.ID] {
			continue
		}
		wgip := h.WGIP
		if wgip != "" && !strings.Contains(wgip, "/") {
			wgip += "/32"
		}
		// Fabric peers carry the other hub's /24 PLUS its advertised
		// SUBNETS (never 0.0.0.0/0) — unconditional: this is the transit
		// path for spokes that opted into a cross-hub egress.
		allowed := append([]string{hubMeshNetwork(h.MeshCIDR)}, subnetRoutes(h.AdvertisedRoutes)...)
		out = append(out, wgHubPeerEntry{
			Pubkey:       h.Pubkey,
			WGIP:         wgip,
			Hostname:     "hub:" + h.Slug,
			Endpoint:     h.Endpoint,
			AllowedExtra: allowed,
		})
	}
	return out
}

// ---- egress / advertised routes (P2 出口) ----
//
// A hub may declare egress CIDRs it gateways to (advertised_routes):
// datacenter subnets, or "0.0.0.0/0" full tunnel. Distribution is strictly
// per-device opt-in (wg_devices.egress_hub_id) — nothing auto-spreads.
// Full tunnel is only honored when the egress hub IS the device's own hub:
// a cross-hub default route would hijack the transit hub's own traffic.

const defaultRouteCIDR = "0.0.0.0/0"

// subnetRoutes filters advertised routes down to the non-default ones.
func subnetRoutes(routes []string) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		if strings.TrimSpace(r) != defaultRouteCIDR {
			out = append(out, strings.TrimSpace(r))
		}
	}
	return out
}

// egressAllowedExtra computes the extra AllowedIPs a spoke gets from its
// egress opt-in. Pure. Cross-hub egress requires the egress hub to be
// configured (it must be reachable through the fabric to forward).
func egressAllowedExtra(egressHubID *int64, ownHub *WGHub, allHubs []WGHub) []string {
	if egressHubID == nil || ownHub == nil {
		return nil
	}
	if *egressHubID == ownHub.ID {
		// Own hub: everything it advertises, incl. 0.0.0.0/0.
		out := make([]string, 0, len(ownHub.AdvertisedRoutes))
		for _, r := range ownHub.AdvertisedRoutes {
			out = append(out, strings.TrimSpace(r))
		}
		return out
	}
	for _, h := range allHubs {
		if h.ID != *egressHubID {
			continue
		}
		if !h.Configured() || strings.TrimSpace(h.Endpoint) == "" {
			return nil // unreachable egress hub — emit nothing rather than blackhole
		}
		return subnetRoutes(h.AdvertisedRoutes)
	}
	return nil
}

// cidrOverlaps reports whether two parsed IPv4 networks overlap.
func cidrOverlaps(a, b *meshNet) bool {
	return a.ipnet.Contains(b.ipnet.IP) || b.ipnet.Contains(a.ipnet.IP)
}

// validateAdvertisedRoutes checks operator input for a hub's egress list:
// each entry must be a valid IPv4 CIDR ("0.0.0.0/0" allowed as the full-
// tunnel marker); subnets must not overlap ANY hub's mesh_cidr (that would
// shadow mesh routing).
func validateAdvertisedRoutes(routes []string, allHubs []WGHub) error {
	for _, r := range routes {
		r = strings.TrimSpace(r)
		if r == "" {
			return fmt.Errorf("empty route entry")
		}
		if r == defaultRouteCIDR {
			continue
		}
		n, err := parseMeshCIDR(r)
		if err != nil {
			return fmt.Errorf("invalid route %q: %v", r, err)
		}
		for _, h := range allHubs {
			m, merr := parseMeshCIDR(h.MeshCIDR)
			if merr != nil {
				continue
			}
			if cidrOverlaps(n, m) {
				return fmt.Errorf("route %q overlaps hub %q mesh_cidr %s", r, h.Slug, h.MeshCIDR)
			}
		}
	}
	return nil
}

// validateWGPolicy checks operator DNS/proxy push input (light). DNS servers
// must parse as IPs; doh mode needs a doh_url; proxy host:port must split and
// pac_url must parse. v1 populates only DNS; proxy fields validate for v2.
func validateWGPolicy(pol *WGPolicy) error {
	if pol == nil {
		return nil
	}
	if d := pol.DNS; d != nil {
		for _, s := range d.Servers {
			if net.ParseIP(strings.TrimSpace(s)) == nil {
				return fmt.Errorf("invalid DNS server %q", s)
			}
		}
		if d.Mode != "" && d.Mode != "plain" && d.Mode != "doh" {
			return fmt.Errorf("dns mode must be 'plain' or 'doh'")
		}
		if d.Mode == "doh" && strings.TrimSpace(d.DoHURL) == "" {
			return fmt.Errorf("doh mode requires doh_url")
		}
	}
	if px := pol.Proxy; px != nil {
		for _, hp := range []string{px.HTTP, px.HTTPS} {
			if hp == "" {
				continue
			}
			if _, _, err := net.SplitHostPort(hp); err != nil {
				return fmt.Errorf("invalid proxy host:port %q: %v", hp, err)
			}
		}
		if px.PACURL != "" {
			if _, err := url.Parse(px.PACURL); err != nil {
				return fmt.Errorf("invalid pac_url %q: %v", px.PACURL, err)
			}
		}
	}
	return nil
}

// validateMeshCIDRDisjoint rejects a mesh_cidr that overlaps any OTHER
// hub's (mint-time guard; suggestFreeMeshCIDR picks disjoint /24s but
// operator-supplied mesh_cidr_pref / update values are free-form).
func validateMeshCIDRDisjoint(meshCIDR string, allHubs []WGHub, selfHubID int64) error {
	n, err := parseMeshCIDR(meshCIDR)
	if err != nil {
		return err
	}
	for _, h := range allHubs {
		if h.ID == selfHubID {
			continue
		}
		m, merr := parseMeshCIDR(h.MeshCIDR)
		if merr != nil {
			continue
		}
		if cidrOverlaps(n, m) {
			return fmt.Errorf("mesh_cidr %s overlaps hub %q (%s)", meshCIDR, h.Slug, h.MeshCIDR)
		}
	}
	return nil
}

// ---- /v1/hub/peers ----

type wgHubPeerEntry struct {
	Pubkey   string `json:"pubkey"`
	WGIP     string `json:"wg_ip"` // includes /32
	Hostname string `json:"hostname,omitempty"`
	// Set only for OTHER-hub peers (the hub-to-hub fabric); empty for the
	// hub's own spokes (which dial in and have no fixed public endpoint).
	Endpoint     string   `json:"endpoint,omitempty"`      // other-hub public endpoint
	AllowedExtra []string `json:"allowed_extra,omitempty"` // other-hub /24
}

type wgHubPeersResponse struct {
	Peers      []wgHubPeerEntry `json:"peers"`
	Rev        string           `json:"rev"`
	RefreshSec int              `json:"refresh_sec"`
}

func (p *Plugin) handleWGHubPeers(c *gin.Context) {
	dev, ok := p.extractDeviceBearer(c)
	if !ok {
		return
	}
	hub, err := p.getWGHubByID(dev.HubID)
	if err != nil || hub == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hub lookup failed"})
		return
	}
	// Caller MUST be the bound hub device.
	if hub.BoundDeviceID == nil || *hub.BoundDeviceID != dev.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not the hub for this mesh"})
		return
	}
	devices, err := p.listWGDevicesInHub(hub.ID, dev.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	peers := make([]wgHubPeerEntry, 0, len(devices))
	var revTS time.Time
	for _, d := range devices {
		peers = append(peers, wgHubPeerEntry{
			Pubkey:   d.Pubkey,
			WGIP:     d.DeviceIP + "/32",
			Hostname: d.Hostname,
		})
		if d.CreatedAt.After(revTS) {
			revTS = d.CreatedAt
		}
		if d.LastSeenAt != nil && d.LastSeenAt.After(revTS) {
			revTS = *d.LastSeenAt
		}
	}
	// Cross-hub fabric: every OTHER configured hub becomes a direct peer so
	// this hub forwards traffic destined for their /24s. Folded into rev via
	// updated_at so the hub re-renders its conf when the hub roster changes.
	allHubs, err := p.listWGHubs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	linkedHubIDs, err := p.hubLinkSet(hub.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	peers = append(peers, otherConfiguredHubPeers(allHubs, hub.ID, hub.MeshCIDR, linkedHubIDs)...)
	for _, h := range allHubs {
		if h.ID != hub.ID && h.UpdatedAt.After(revTS) {
			revTS = h.UpdatedAt
		}
	}
	rev := strconv.FormatInt(revTS.UnixNano(), 10) + "-" + strconv.Itoa(len(peers))
	c.JSON(http.StatusOK, wgHubPeersResponse{
		Peers:      peers,
		Rev:        rev,
		RefreshSec: hub.RefreshSec,
	})
}

// ---- /v1/heartbeat ----

type wgHeartbeatRequest struct {
	LANAddrs   []WGLanAddr `json:"lan_addrs"`
	WGEndpoint string      `json:"wg_endpoint"`
	Stats      *struct {
		RXBytes          *int64 `json:"rx_bytes"`
		TXBytes          *int64 `json:"tx_bytes"`
		LastHandshakeSec *int64 `json:"last_handshake_sec"`
	} `json:"stats"`
}

func (p *Plugin) handleWGHeartbeat(c *gin.Context) {
	dev, ok := p.extractDeviceBearer(c)
	if !ok {
		return
	}
	var req wgHeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	rec := &WGHeartbeatRecord{
		DeviceID:   dev.ID,
		WGEndpoint: strings.TrimSpace(req.WGEndpoint),
		LANAddrs:   req.LANAddrs,
	}
	if req.Stats != nil {
		rec.RXBytes = req.Stats.RXBytes
		rec.TXBytes = req.Stats.TXBytes
		rec.LastHandshakeSec = req.Stats.LastHandshakeSec
	}
	hubSlug := ""
	if hub, _ := p.getWGHubByID(dev.HubID); hub != nil {
		hubSlug = hub.Slug
	}
	if err := p.insertWGHeartbeat(rec, time.Now().UTC()); err != nil {
		p.metrics.recordWGHeartbeat(hubSlug, "error")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "heartbeat: " + err.Error()})
		return
	}
	p.metrics.recordWGHeartbeat(hubSlug, "ok")
	c.Status(http.StatusOK)
}

// ---- /v1/leave ----
//
// Per JOIN_PROTOCOL §2 v0.2: if the caller IS the hub for its mesh,
// also clear wg_hup.pubkey + bound_device_id so the next hub-token
// register reclaims the slot.

func (p *Plugin) handleWGLeave(c *gin.Context) {
	dev, ok := p.extractDeviceBearer(c)
	if !ok {
		return
	}
	if err := p.markWGDeviceRemoved(dev.ID, time.Now().UTC()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "leave: " + err.Error()})
		return
	}
	hub, _ := p.getWGHubByID(dev.HubID)
	if hub != nil && hub.BoundDeviceID != nil && *hub.BoundDeviceID == dev.ID {
		if err := p.clearWGHubBind(hub.ID, time.Now().UTC()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "clear hub: " + err.Error()})
			return
		}
	}
	c.Status(http.StatusOK)
}

// ---- /v1/token/refresh ----

type wgTokenRefreshResponse struct {
	Token   string     `json:"token"`
	Expires *time.Time `json:"expires,omitempty"`
}

func (p *Plugin) handleWGTokenRefresh(c *gin.Context) {
	dev, ok := p.extractDeviceBearer(c)
	if !ok {
		return
	}
	plaintext, _ := newWGTokenPlaintext()
	newHash := hashWGToken(plaintext)
	var newExpires *time.Time
	if dev.TokenExpiresAt != nil {
		v := time.Now().UTC().Add(90 * 24 * time.Hour)
		newExpires = &v
	}
	if err := p.updateWGDeviceTokenHash(dev.ID, newHash, newExpires); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refresh: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, wgTokenRefreshResponse{Token: plaintext, Expires: newExpires})
}

// ---- /v1/install (+ /:version) ----

func (p *Plugin) handleWGInstallScript(c *gin.Context) {
	// Target platform: ?os=&arch= pre-targets a specific bundle (e.g. for
	// cross-platform packaging). When omitted the script auto-detects via uname
	// at run time. OS defaults to darwin for back-compat.
	osTarget, archTarget := reqOSArch(c)
	version := strings.TrimSpace(c.Param("version"))
	if version == "" {
		latest, err := p.getLatestWGBundleFor(osTarget, archTarget)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup latest: " + err.Error()})
			return
		}
		if latest == nil {
			version = "latest"
		} else {
			version = latest.Version
		}
	}
	// Mesh CIDR baked into the script is informational only — the
	// actual CIDR for this device is the hub it joins (set at register).
	// Use the first hub's mesh_cidr as a sensible default for the
	// script comment; fall back to the canonical default.
	meshCIDR := "100.64.0.0/24"
	if hubs, err := p.listWGHubs(); err == nil && len(hubs) > 0 {
		meshCIDR = hubs[0].MeshCIDR
	}
	// When the request pinned os/arch, bake them so the script fetches that
	// exact bundle; otherwise leave blank so the script uses its uname result.
	bakeOS, bakeArch := "", archTarget
	if c.Query("os") != "" {
		bakeOS = osTarget
	}
	script, err := renderWGInstallScript(wgInstallScriptInput{
		Server:        wgServerBaseURL(c),
		BundleVersion: version,
		MeshCIDR:      meshCIDR,
		OS:            bakeOS,
		Arch:          bakeArch,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "render: " + err.Error()})
		return
	}
	c.Header("Content-Type", "text/x-shellscript; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, script)
}

// ---- admin: hubs (plural) ----

func (p *Plugin) handleAdminWGHubList(c *gin.Context) {
	hubs, err := p.listWGHubs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hubs": hubs})
}

func (p *Plugin) handleAdminWGHubUpdate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	existing, err := p.getWGHubByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var req struct {
		Label        string `json:"label"`
		Endpoint     string `json:"endpoint"`
		WGIP         string `json:"wg_ip"`
		MeshCIDR     string `json:"mesh_cidr"`
		KeepaliveSec int    `json:"keepalive_sec"`
		RefreshSec   int    `json:"refresh_sec"`
		// P2 egress: nil = leave unchanged; [] = clear; non-empty = replace.
		AdvertisedRoutes *[]string `json:"advertised_routes"`
		// DNS/proxy push: nil = leave unchanged; object = replace.
		Policy *WGPolicy `json:"policy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	allHubs, err := p.listWGHubs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	existing.Label = req.Label
	existing.Endpoint = req.Endpoint
	if req.WGIP != "" {
		existing.WGIP = req.WGIP
	}
	if req.MeshCIDR != "" {
		// Mint-time disjointness guard: a hand-edited mesh_cidr must not
		// overlap any other hub's (cross-hub routing depends on it).
		if err := validateMeshCIDRDisjoint(req.MeshCIDR, allHubs, existing.ID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		existing.MeshCIDR = req.MeshCIDR
	}
	if req.KeepaliveSec > 0 {
		existing.KeepaliveSec = req.KeepaliveSec
	}
	if req.RefreshSec > 0 {
		existing.RefreshSec = req.RefreshSec
	}
	if req.AdvertisedRoutes != nil {
		if err := validateAdvertisedRoutes(*req.AdvertisedRoutes, allHubs); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		existing.AdvertisedRoutes = *req.AdvertisedRoutes
	}
	if req.Policy != nil {
		if err := validateWGPolicy(req.Policy); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		existing.Policy = req.Policy
	}
	out, err := p.updateWGHub(existing, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hub": out})
}

func (p *Plugin) handleAdminWGHubDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := p.deleteWGHub(id); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Admin "reset hub binding" — clear pubkey + bound_device_id so a
// fresh hub token can reclaim the slot without admin having to wait
// for the bound device to /v1/leave.
func (p *Plugin) handleAdminWGHubResetBind(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := p.clearWGHubBind(id, time.Now().UTC()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- cross-hub route publishing (wg_hub_links) ----

func (p *Plugin) handleAdminWGHubLinkList(c *gin.Context) {
	links, err := p.listWGHubLinks()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"links": links})
}

func (p *Plugin) handleAdminWGHubLinkCreate(c *gin.Context) {
	var req struct {
		HubAID int64 `json:"hub_a_id"`
		HubBID int64 `json:"hub_b_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.HubAID <= 0 || req.HubBID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hub_a_id and hub_b_id required"})
		return
	}
	link, err := p.createWGHubLink(req.HubAID, req.HubBID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"link": link})
}

func (p *Plugin) handleAdminWGHubLinkDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := p.deleteWGHubLink(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- admin: tokens (role-aware) ----

func (p *Plugin) handleAdminWGTokenList(c *gin.Context) {
	toks, err := p.listWGTokens()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": toks})
}

func (p *Plugin) handleAdminWGTokenCreate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	var req struct {
		Label   string `json:"label" binding:"required"`
		Role    string `json:"role" binding:"required"`
		TTLDays int    `json:"ttl_days"`
		// role=device
		HubID int64 `json:"hub_id"`
		// role=hub
		HubSlug        string `json:"hub_slug"`
		HubLabel       string `json:"hub_label"`
		PublicEndpoint string `json:"public_endpoint"`
		MeshCIDR       string `json:"mesh_cidr"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	var expiresAt *time.Time
	if req.TTLDays > 0 {
		v := time.Now().UTC().Add(time.Duration(req.TTLDays) * 24 * time.Hour)
		expiresAt = &v
	}
	now := time.Now().UTC()
	// Phase 1-D-1 note: embedded Headscale stays in dock. wg-svc
	// doesn't have a Headscale client, so token-create never returns
	// a tailscale_authkey from this path. Operators who need the
	// Tailscale PreAuthKey continue using dock's /api/admin/wg-tokens
	// endpoint until Phase 1-D-2 figures out the headscale split.
	tailscaleAuthKey := ""

	switch req.Role {
	case "hub":
		tok, hub, err := p.createWGTokenForHub(req.Label, userIDStr, req.HubSlug, req.HubLabel, req.PublicEndpoint, req.MeshCIDR, expiresAt, now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		resp := gin.H{
			"token":   tok,
			"hub":     hub,
			"warning": "plaintext 只显示一次，关闭后无法找回。请立即复制保存。",
		}
		if tailscaleAuthKey != "" {
			resp["tailscale_authkey"] = tailscaleAuthKey
		}
		c.JSON(http.StatusOK, resp)
	case "device":
		if req.HubID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hub_id required for role=device"})
			return
		}
		tok, err := p.createWGTokenForDevice(req.Label, userIDStr, req.HubID, expiresAt, now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		resp := gin.H{
			"token":   tok,
			"warning": "plaintext 只显示一次，关闭后无法找回。请立即复制保存。",
		}
		if tailscaleAuthKey != "" {
			resp["tailscale_authkey"] = tailscaleAuthKey
		}
		c.JSON(http.StatusOK, resp)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'hub' or 'device'"})
	}
}

func (p *Plugin) handleAdminWGTokenRevoke(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := p.revokeWGToken(id, time.Now().UTC()); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- admin: devices ----

func (p *Plugin) handleAdminWGDeviceList(c *gin.Context) {
	includeRemoved := c.Query("include_removed") == "1"
	devs, err := p.listWGDevices(includeRemoved)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": devs})
}

// handleAdminWGDeviceUpdate — P2 egress: set/clear a device's egress
// opt-in. Body: {"egress_hub_id": <id>} or {"egress_hub_id": null}.
// Cross-hub egress is allowed only when the egress hub advertises at
// least one subnet (0.0.0.0/0 never crosses hubs, so opting into a hub
// that only declares the default route would be a silent no-op — reject
// with a clear message instead).
func (p *Plugin) handleAdminWGDeviceUpdate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	dev, err := p.getWGDeviceByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if dev == nil || dev.RemovedAt != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var req struct {
		EgressHubID *int64 `json:"egress_hub_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	if req.EgressHubID != nil {
		eh, err := p.getWGHubByID(*req.EgressHubID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
			return
		}
		if eh == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "egress hub not found"})
			return
		}
		if len(eh.AdvertisedRoutes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hub has no advertised routes"})
			return
		}
		if eh.ID != dev.HubID && len(subnetRoutes(eh.AdvertisedRoutes)) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cross-hub egress requires subnet routes; 0.0.0.0/0 full tunnel only works via the device's own hub"})
			return
		}
	}
	if err := p.updateWGDeviceEgressHub(id, req.EgressHubID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	dev, _ = p.getWGDeviceByID(id)
	c.JSON(http.StatusOK, gin.H{"device": dev})
}

func (p *Plugin) handleAdminWGDeviceRemove(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	dev, err := p.getWGDeviceByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := p.markWGDeviceRemoved(id, time.Now().UTC()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	// If this was the hub device, clear hub binding too — admin
	// force-remove implies the operator wants the slot reclaimable.
	if hub, _ := p.getWGHubByID(dev.HubID); hub != nil && hub.BoundDeviceID != nil && *hub.BoundDeviceID == dev.ID {
		_ = p.clearWGHubBind(hub.ID, time.Now().UTC())
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- admin: sites ----

func (p *Plugin) handleAdminWGSiteList(c *gin.Context) {
	sites, err := p.listWGSites()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sites": sites})
}

// hubEndpointFor picks the hub endpoint a spoke should dial. Spokes behind the
// SAME NAT as the hub (their server-observed public IP == the IP the hub's
// public endpoint resolves to) usually cannot hairpin the public address; hand
// them the hub device's LAN endpoint (wg_devices.wg_endpoint of the bound hub
// device, e.g. 192.168.11.197:1639) instead. Everyone else gets hub.Endpoint.
// Seen 2026-08-17: cloud VMs on the hub's LAN handshake never via the public
// endpoint, instantly via the LAN one.
func (p *Plugin) hubEndpointFor(hub *WGHub, dev *WGDevice) string {
	ep := hub.Endpoint
	if hub == nil || dev == nil || hub.BoundDeviceID == nil || dev.WGEndpoint == "" || ep == "" {
		return ep
	}
	hubHost, _, err := net.SplitHostPort(ep)
	if err != nil {
		return ep
	}
	devHost, _, err := net.SplitHostPort(dev.WGEndpoint)
	if err != nil {
		return ep
	}
	if !hubEndpointResolvesTo(hubHost, devHost) {
		return ep
	}
	hd, err := p.getWGDeviceByID(*hub.BoundDeviceID)
	if err != nil || hd == nil || hd.WGEndpoint == "" {
		return ep
	}
	if h, _, err := net.SplitHostPort(hd.WGEndpoint); err == nil {
		if ip := net.ParseIP(h); ip != nil && ip.IsPrivate() {
			return hd.WGEndpoint
		}
	}
	return ep
}

var hubResolveCache = struct {
	sync.Mutex
	m map[string]hubResolveEntry
}{m: map[string]hubResolveEntry{}}

type hubResolveEntry struct {
	ips []string
	at  time.Time
}

// hubEndpointResolvesTo: does hub host (name or literal) resolve to ip? DNS is
// cached 5 minutes — /v1/peers is polled by every spoke.
func hubEndpointResolvesTo(host, ip string) bool {
	if host == ip {
		return true
	}
	if net.ParseIP(host) != nil {
		return false
	}
	hubResolveCache.Lock()
	e, ok := hubResolveCache.m[host]
	hubResolveCache.Unlock()
	if !ok || time.Since(e.at) > 5*time.Minute {
		ips, err := net.LookupHost(host)
		if err != nil {
			ips = nil
		}
		e = hubResolveEntry{ips: ips, at: time.Now()}
		hubResolveCache.Lock()
		hubResolveCache.m[host] = e
		hubResolveCache.Unlock()
	}
	for _, x := range e.ips {
		if x == ip {
			return true
		}
	}
	return false
}

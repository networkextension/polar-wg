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
	"net/http"
	"strconv"
	"strings"
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
	Token        string           `json:"token,omitempty"`
	TokenExpires *time.Time       `json:"token_expires,omitempty"`
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
	// Hub peer (only if hub is bound AND caller isn't the hub itself).
	if hub.Configured() && (hub.BoundDeviceID == nil || *hub.BoundDeviceID != dev.ID) {
		otherIdx, err := p.otherSiteIndicesInHub(hub.ID, dev.SiteID)
		if err != nil {
			return nil, fmt.Errorf("other site indices: %w", err)
		}
		allowedExtra := make([]string, 0, len(otherIdx)+1)
		// Hub's own /24 (computed from mesh_cidr's network address).
		if mesh, err := parseMeshCIDR(hub.MeshCIDR); err == nil {
			allowedExtra = append(allowedExtra, mesh.ipnet.String())
		}
		_ = otherIdx // multi-/24 hub allocation deferred to a future PR
		peerOut = append(peerOut, wgPeerResponse{
			Pubkey:       hub.Pubkey,
			WGIP:         hub.WGIP,
			Endpoint:     hub.Endpoint,
			SiteSlug:     "hub",
			AllowedExtra: allowedExtra,
		})
	}
	return &wgRegisterResponse{
		DeviceID:     dev.DeviceID,
		DeviceIP:     dev.DeviceIP,
		SiteSlug:     dev.SiteSlug,
		HubSlug:      hub.Slug,
		MeshCIDR:     hub.MeshCIDR,
		Hub: wgHubResponse{
			Slug:     hub.Slug,
			Pubkey:   hub.Pubkey,
			Endpoint: hub.Endpoint,
			WGIP:     hub.WGIP,
		},
		Peers:        peerOut,
		KeepaliveSec: hub.KeepaliveSec,
		RefreshSec:   hub.RefreshSec,
		TokenExpires: dev.TokenExpiresAt,
	}, nil
}

func firstLANIP(cidr string) string {
	if i := strings.Index(cidr, "/"); i > 0 {
		return cidr[:i]
	}
	return cidr
}

// ---- /v1/hub/peers ----

type wgHubPeerEntry struct {
	Pubkey   string `json:"pubkey"`
	WGIP     string `json:"wg_ip"` // includes /32
	Hostname string `json:"hostname,omitempty"`
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
	version := strings.TrimSpace(c.Param("version"))
	if version == "" {
		latest, err := p.getLatestWGBundle()
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
	script, err := renderWGInstallScript(wgInstallScriptInput{
		Server:        wgServerBaseURL(c),
		BundleVersion: version,
		MeshCIDR:      meshCIDR,
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
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	existing.Label = req.Label
	existing.Endpoint = req.Endpoint
	if req.WGIP != "" {
		existing.WGIP = req.WGIP
	}
	if req.MeshCIDR != "" {
		existing.MeshCIDR = req.MeshCIDR
	}
	if req.KeepaliveSec > 0 {
		existing.KeepaliveSec = req.KeepaliveSec
	}
	if req.RefreshSec > 0 {
		existing.RefreshSec = req.RefreshSec
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

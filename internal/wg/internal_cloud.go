package wg

// /internal/v1/wg-tokens* — sibling-plugin token lifecycle for machine
// enrolment (polar-cloud: a freshly created VM joins a dedicated hub at first
// boot with a one-shot device token that cloud-svc seeds into the guest, and is
// released again when the VM is destroyed).
//
//   POST /internal/v1/wg-tokens              {hub_slug, label, ttl_sec} → mint role=device token
//   GET  /internal/v1/wg-tokens/:id          token state + the device that consumed it (overlay IP)
//   POST /internal/v1/wg-tokens/:id/release  soft-remove the device (frees IP/pubkey/d_index) + revoke token
//
// Auth: loopback-only, same gate + rationale as /internal/v1/wg-devices/link
// and polar-hosts' /internal/v1/hosts/enroll (sibling plugins run on the same
// host by deploy convention; nginx does not proxy /internal/*). No HMAC.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	internalTokenDefaultTTL = 24 * time.Hour
	internalTokenMaxTTL     = 30 * 24 * time.Hour
	internalTokenCreatedBy  = "internal:cloud"
)

type internalTokenMintReq struct {
	HubSlug string `json:"hub_slug"`
	Label   string `json:"label"`
	TTLSec  int64  `json:"ttl_sec"`
}

func internalHubOut(h *WGHub) gin.H {
	return gin.H{"id": h.ID, "slug": h.Slug, "endpoint": h.Endpoint, "mesh_cidr": h.MeshCIDR,
		"pubkey": h.Pubkey, "wg_ip": h.WGIP, "keepalive_sec": h.KeepaliveSec, "refresh_sec": h.RefreshSec}
}

func (p *Plugin) handleInternalWGTokenMint(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	var req internalTokenMintReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	req.HubSlug = strings.TrimSpace(req.HubSlug)
	req.Label = strings.TrimSpace(req.Label)
	if req.HubSlug == "" || req.Label == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hub_slug and label required"})
		return
	}
	hub, err := p.getWGHubBySlug(req.HubSlug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if hub == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "hub_not_found"})
		return
	}
	if !hub.Configured() {
		// Same semantics as /v1/register's 424: nobody has installed the hub yet.
		c.JSON(http.StatusFailedDependency, gin.H{"error": "hub_not_configured", "hub": hub.Slug})
		return
	}
	ttl := internalTokenDefaultTTL
	if req.TTLSec > 0 {
		ttl = time.Duration(req.TTLSec) * time.Second
	}
	if ttl > internalTokenMaxTTL {
		ttl = internalTokenMaxTTL
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	tok, err := p.createWGTokenForDevice(req.Label, internalTokenCreatedBy, hub.ID, &exp, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token_id":   tok.ID,
		"token":      tok.Plaintext,
		"expires_at": exp,
		"hub":        internalHubOut(hub),
	})
}

func (p *Plugin) internalTokenParam(c *gin.Context) (*WGToken, bool) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return nil, false
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return nil, false
	}
	tok, err := p.getWGTokenByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return nil, false
	}
	if tok == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "token_not_found"})
		return nil, false
	}
	return tok, true
}

func (p *Plugin) internalTokenDevice(tok *WGToken) (*WGDevice, error) {
	if tok.ConsumedByDeviceID == nil {
		return nil, nil
	}
	return p.getWGDeviceByID(*tok.ConsumedByDeviceID)
}

func internalDeviceOut(d *WGDevice) gin.H {
	if d == nil {
		return nil
	}
	return gin.H{"id": d.ID, "device_id": d.DeviceID, "device_ip": d.DeviceIP, "pubkey": d.Pubkey,
		"hostname": d.Hostname, "host_id": d.HostID, "os": d.OS, "wg_endpoint": d.WGEndpoint,
		"last_seen_at": d.LastSeenAt, "removed": d.RemovedAt != nil, "removed_at": d.RemovedAt}
}

func (p *Plugin) handleInternalWGTokenGet(c *gin.Context) {
	tok, ok := p.internalTokenParam(c)
	if !ok {
		return
	}
	dev, err := p.internalTokenDevice(tok)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token_id":   tok.ID,
		"label":      tok.Label,
		"hub_id":     tok.HubID,
		"expires_at": tok.ExpiresAt,
		"consumed":   tok.ConsumedAt != nil,
		"revoked":    tok.RevokedAt != nil,
		"device":     internalDeviceOut(dev),
	})
}

// release = the VM is gone: free its overlay slot and make the token dead.
// Idempotent — safe to call on an already-released / never-consumed token.
func (p *Plugin) handleInternalWGTokenRelease(c *gin.Context) {
	tok, ok := p.internalTokenParam(c)
	if !ok {
		return
	}
	now := time.Now().UTC()
	dev, err := p.internalTokenDevice(tok)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	out := gin.H{"token_id": tok.ID, "removed_device_id": nil, "device_ip": "", "revoked": true}
	if dev != nil {
		if dev.RemovedAt == nil {
			if err := p.markWGDeviceRemoved(dev.ID, now); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
				return
			}
			// Mirrors admin force-remove: a released hub device also unbinds the hub.
			if hub, _ := p.getWGHubByID(dev.HubID); hub != nil && hub.BoundDeviceID != nil && *hub.BoundDeviceID == dev.ID {
				_ = p.clearWGHubBind(hub.ID, now)
			}
		}
		out["removed_device_id"] = dev.DeviceID
		out["device_ip"] = dev.DeviceIP
	}
	if tok.RevokedAt == nil {
		if err := p.revokeWGToken(tok.ID, now); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
			return
		}
	}
	c.JSON(http.StatusOK, out)
}

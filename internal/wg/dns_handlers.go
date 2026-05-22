package wg

// MagicDNS — server-side hostname → wg_ip lookup per hub.
//
// Operator workflow:
//   1. wgctl-agent (on each device) periodically GETs /v1/dns/<hub_slug>.
//   2. Server returns a zone-file-style mapping of hostname.<hub_slug>.wg → wg_ip
//      for all live devices in that hub.
//   3. agent writes /etc/resolver/<hub_slug>.wg on macOS, pointing the
//      `.wg` suffix at a local DNS responder it stands up; the responder
//      consults the cached zone.
//
// Polar-side scope (this file): expose the zone over HTTP. The client
// half (the local resolver) is wg-mac repo work; tracked in their
// PRODUCT_MODULES.md follow-ups.
//
// Auth: same Bearer device-token shape as /v1/peers. Caller's hub_id
// scopes the response — devices only see their own mesh.

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// wgDNSEntry is one host record in a hub's zone.
type wgDNSEntry struct {
	Hostname string `json:"hostname"`
	WGIP     string `json:"wg_ip"`
	DeviceID string `json:"device_id"`
}

type wgDNSResponse struct {
	HubSlug    string       `json:"hub_slug"`
	Suffix     string       `json:"suffix"`      // e.g. "pudong.wg" — devices append this to hostname
	Entries    []wgDNSEntry `json:"entries"`
	Rev        string       `json:"rev"`         // ETag-style change tracker (max(updated_at|created_at))
	RefreshSec int          `json:"refresh_sec"` // suggested re-poll interval
}

// validHostnameRE accepts the same characters the client will use for a
// DNS label (RFC 1123 host names: letters, digits, hyphen, dot). We
// downcase + strip anything else server-side so a host calling itself
// "Bob's Mac 🍎" still gets a usable entry as "bobs-mac".
var validHostnameRE = regexp.MustCompile(`[^a-z0-9.-]+`)

// sanitizeHostname downcases + strips invalid chars + trims leading/
// trailing hyphens. Empty result fall back to "dev-<n>" (handled by caller).
func sanitizeHostname(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = validHostnameRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// GET /v1/dns/:hub_slug — bearer auth, scope must match.
func (p *Plugin) handleWGDNSZone(c *gin.Context) {
	dev, ok := p.extractDeviceBearer(c)
	if !ok {
		return
	}
	requested := strings.TrimSpace(c.Param("hub_slug"))
	hub, err := p.getWGHubByID(dev.HubID)
	if err != nil || hub == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hub lookup failed"})
		return
	}
	// Refuse if caller is asking about a different hub than its own.
	if requested != "" && requested != hub.Slug {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-hub DNS query not allowed"})
		return
	}
	devices, err := p.listWGDevicesInHub(hub.ID, 0) // include caller in zone
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The caller's own row needs to be in the zone too (listWGDevicesInHub
	// excludes by ID); fetch it separately. Edge: a hub device sees
	// itself at <hub.slug>.<hub.slug>.wg = <hub.wg_ip>.
	if dev != nil {
		devices = append(devices, *dev)
	}
	entries := make([]wgDNSEntry, 0, len(devices))
	seen := make(map[string]bool, len(devices))
	var revTS time.Time
	for _, d := range devices {
		hn := sanitizeHostname(d.Hostname)
		if hn == "" {
			hn = fmt.Sprintf("dev-%d", d.ID)
		}
		// Avoid duplicate FQDNs by appending -<id> on collision.
		if seen[hn] {
			hn = fmt.Sprintf("%s-%d", hn, d.ID)
		}
		seen[hn] = true
		entries = append(entries, wgDNSEntry{
			Hostname: hn,
			WGIP:     d.DeviceIP,
			DeviceID: d.DeviceID,
		})
		if d.CreatedAt.After(revTS) {
			revTS = d.CreatedAt
		}
		if d.LastSeenAt != nil && d.LastSeenAt.After(revTS) {
			revTS = *d.LastSeenAt
		}
	}
	c.JSON(http.StatusOK, wgDNSResponse{
		HubSlug:    hub.Slug,
		Suffix:     hub.Slug + ".wg",
		Entries:    entries,
		Rev:        fmt.Sprintf("%d-%d", revTS.UnixNano(), len(entries)),
		RefreshSec: hub.RefreshSec,
	})
}

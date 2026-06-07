package wg

// hub_status_handlers.go — `GET /api/admin/wg-hub-status`.
//
// Joins the in-memory cache (fed by hub_status_poll.go) against
// wg_hubs by pubkey and returns one entry per registered hub. Status
// is null when no sample has arrived for that hub's pubkey, stale=true
// when the cached sample is older than the freshness TTL.
//
// Admin-only — same dock-introspection middleware as the other
// /api/admin/wg-* routes.

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type wgHubStatusEntry struct {
	HostID     string         `json:"host_id"`
	Iface      string         `json:"iface"`
	RecordedAt time.Time      `json:"recorded_at"`
	Stale      bool           `json:"stale"`
	PeerCount  int            `json:"peer_count"`
	ListenPort int            `json:"listen_port,omitempty"`
	Peers      []any          `json:"peers,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

type wgHubStatusRow struct {
	ID       int64             `json:"id"`
	Slug     string            `json:"slug"`
	Label    string            `json:"label"`
	Pubkey   string            `json:"pubkey"`
	Endpoint string            `json:"endpoint"`
	WGIP     string            `json:"wg_ip"`
	Status   *wgHubStatusEntry `json:"status"`
}

type wgHubStatusResponse struct {
	Hubs []wgHubStatusRow `json:"hubs"`
}

func (p *Plugin) handleAdminWGHubStatus(c *gin.Context) {
	hubs, err := p.listWGHubs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	// pubkey → host_id for live devices, so each hub-status peer (keyed only
	// by pubkey from the live wgctl poll) can carry a host_id for the
	// WG→Hosts UI deep-link. Best-effort: a DB blip just leaves peers
	// without host_id rather than failing the whole status call.
	hostIDByPubkey, err := p.wgDeviceHostIDsByPubkey()
	if err != nil {
		hostIDByPubkey = nil
	}
	out := wgHubStatusResponse{Hubs: make([]wgHubStatusRow, 0, len(hubs))}
	for _, h := range hubs {
		row := wgHubStatusRow{
			ID:       h.ID,
			Slug:     h.Slug,
			Label:    h.Label,
			Pubkey:   h.Pubkey,
			Endpoint: h.Endpoint,
			WGIP:     h.WGIP,
		}
		if p.hubStatus != nil && h.Pubkey != "" {
			if sample, ok, stale := p.hubStatus.lookup(h.Pubkey); ok {
				row.Status = buildHubStatusEntry(sample, stale)
				enrichPeersWithHostID(row.Status, hostIDByPubkey)
			}
		}
		out.Hubs = append(out.Hubs, row)
	}
	c.JSON(http.StatusOK, out)
}

// buildHubStatusEntry pulls the well-known peer-status fields out of
// the agent's opaque `data` payload. Everything else lands in `Extra`
// so UI can grow without backend changes.
func buildHubStatusEntry(s wgHubStatusSample, stale bool) *wgHubStatusEntry {
	out := &wgHubStatusEntry{
		HostID:     s.HostID,
		Iface:      s.Iface,
		RecordedAt: s.RecordedAt,
		Stale:      stale,
	}
	if s.Data == nil {
		return out
	}
	if v, ok := numFromAny(s.Data["peer_count"]); ok {
		out.PeerCount = int(v)
	}
	if v, ok := numFromAny(s.Data["listen_port"]); ok {
		out.ListenPort = int(v)
	}
	if peers, ok := s.Data["peers"].([]any); ok {
		out.Peers = peers
	}
	// Stash anything else (sampled_at, iface_public_key, …) under
	// Extra so the UI / future tooling has access without us having
	// to grow this struct every time.
	extra := map[string]any{}
	for k, v := range s.Data {
		switch k {
		case "peer_count", "listen_port", "peers":
			continue
		}
		extra[k] = v
	}
	if len(extra) > 0 {
		out.Extra = extra
	}
	return out
}

// enrichPeersWithHostID stamps host_id onto each peer map (keyed by its
// public_key) so the topology / device list can deep-link a live peer to its
// polar-hosts host without a second client-side join. No-op when the map is
// empty or a peer's pubkey has no linked device. doc/arch/wg-host-crosslink.md.
func enrichPeersWithHostID(entry *wgHubStatusEntry, hostIDByPubkey map[string]string) {
	if entry == nil || len(hostIDByPubkey) == 0 {
		return
	}
	for _, raw := range entry.Peers {
		peer, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pk, _ := peer["public_key"].(string)
		if pk == "" {
			continue
		}
		if hid, found := hostIDByPubkey[pk]; found && hid != "" {
			peer["host_id"] = hid
		}
	}
}

func numFromAny(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

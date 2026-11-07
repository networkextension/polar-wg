package wg

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Native Linux agents already send this status on their authenticated heartbeat.
// Keep the latest bound-hub sample across service restarts and dock cache polls.
type wgHeartbeatStatus struct {
	Schema   int               `json:"schema"`
	Role     string            `json:"role"`
	Iface    string            `json:"iface"`
	WGListen int               `json:"wg_listen"`
	IfaceUp  bool              `json:"iface_up"`
	Peers    []wgHeartbeatPeer `json:"peers"`
}

type wgHeartbeatPeer struct {
	Pubkey           string `json:"pubkey"`
	WGIP             string `json:"wg_ip"`
	Endpoint         string `json:"endpoint"`
	AllowedIPs       string `json:"allowed_ips"`
	LastHandshakeSec *int64 `json:"last_handshake_sec"`
	RXBytes          uint64 `json:"rx_bytes"`
	TXBytes          uint64 `json:"tx_bytes"`
	KeepaliveSec     *int   `json:"keepalive_sec"`
}

func (p *Plugin) ensureHubHeartbeatColumns() error {
	_, err := p.DB.Exec(`ALTER TABLE wg_devices
		ADD COLUMN IF NOT EXISTS last_status_json JSONB,
		ADD COLUMN IF NOT EXISTS last_status_at TIMESTAMPTZ`)
	return err
}

func (p *Plugin) storeHubHeartbeatStatus(dev *WGDevice, h *WGHub, status *wgHeartbeatStatus, now time.Time) error {
	// A spoke must never replace its hub's authoritative sample.
	if status == nil || status.Schema != 1 || status.Role != "hub" || status.Iface == "" || h == nil || h.BoundDeviceID == nil || *h.BoundDeviceID != dev.ID || h.Pubkey != dev.Pubkey {
		return nil
	}
	if len(status.Peers) > 4096 {
		return fmt.Errorf("hub status exceeds peer limit")
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = p.DB.Exec(`UPDATE wg_devices SET last_status_json=$2, last_status_at=$3 WHERE id=$1 AND removed_at IS NULL`, dev.ID, string(data), now)
	return err
}

func (p *Plugin) hubHeartbeatSample(h WGHub) (wgHubStatusSample, bool, error) {
	if h.BoundDeviceID == nil {
		return wgHubStatusSample{}, false, nil
	}
	var raw []byte
	var recorded time.Time
	err := p.DB.QueryRow(`SELECT last_status_json, last_status_at FROM wg_devices
		WHERE id=$1 AND pubkey=$2 AND removed_at IS NULL AND last_status_json IS NOT NULL AND last_status_at IS NOT NULL`, *h.BoundDeviceID, h.Pubkey).Scan(&raw, &recorded)
	if err == sql.ErrNoRows {
		return wgHubStatusSample{}, false, nil
	}
	if err != nil {
		return wgHubStatusSample{}, false, err
	}
	var status wgHeartbeatStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return wgHubStatusSample{}, false, err
	}
	return heartbeatHubSample(h.Pubkey, status, recorded, time.Now()), true, nil
}

func heartbeatHubSample(pubkey string, status wgHeartbeatStatus, recorded, now time.Time) wgHubStatusSample {
	elapsed := int64(now.Sub(recorded) / time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	peers := make([]any, 0, len(status.Peers))
	for _, p := range status.Peers {
		allowed := p.AllowedIPs
		if allowed == "" && p.WGIP != "" {
			allowed = p.WGIP
			if !strings.Contains(allowed, "/") {
				allowed += "/32"
			}
		}
		peer := map[string]any{"public_key": p.Pubkey, "wg_ip": p.WGIP, "endpoint": p.Endpoint, "allowed_ips": allowed, "bytes_rx": p.RXBytes, "bytes_tx": p.TXBytes}
		// Missing handshake stays absent; 0 is a valid fresh handshake age.
		if p.LastHandshakeSec != nil {
			peer["handshake_age_sec"] = *p.LastHandshakeSec + elapsed
		}
		if p.KeepaliveSec != nil {
			peer["keepalive_sec"] = *p.KeepaliveSec
		}
		peers = append(peers, peer)
	}
	return wgHubStatusSample{Iface: status.Iface, IfacePublicKey: pubkey, RecordedAt: recorded, Data: map[string]any{
		"peer_count": len(peers), "listen_port": status.WGListen, "peers": peers, "source": "heartbeat", "iface_up": status.IfaceUp,
	}}
}

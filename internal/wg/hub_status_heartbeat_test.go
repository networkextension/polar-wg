package wg

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNativeHeartbeatMapsToHubStatus(t *testing.T) {
	// Actual Linux heartbeat field names differ from the UI/dock contract.
	var req wgHeartbeatRequest
	err := json.Unmarshal([]byte(`{"status":{"schema":1,"role":"hub","iface":"wgc0","iface_up":true,"wg_listen":1632,"peers":[{"pubkey":"live","wg_ip":"100.64.0.4","last_handshake_sec":0,"rx_bytes":1234,"tx_bytes":5678,"keepalive_sec":25},{"pubkey":"offline","wg_ip":"100.64.0.10/32","last_handshake_sec":null,"rx_bytes":0,"tx_bytes":0}]}}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	recorded := time.Now().UTC().Add(-30 * time.Second)
	sample := heartbeatHubSample("hub-key", *req.Status, recorded, recorded.Add(30*time.Second))
	entry := buildHubStatusEntry(sample, false)
	live := entry.Peers[0].(map[string]any)
	if entry.RecordedAt.IsZero() || entry.ListenPort != 1632 || entry.PeerCount != 2 {
		t.Fatalf("invalid sample: %+v", entry)
	}
	if live["handshake_age_sec"] != int64(30) || live["allowed_ips"] != "100.64.0.4/32" || live["bytes_rx"] != uint64(1234) || live["bytes_tx"] != uint64(5678) || live["keepalive_sec"] != 25 {
		t.Fatalf("bad peer mapping: %+v", live)
	}
	offline := entry.Peers[1].(map[string]any)
	if _, ok := offline["handshake_age_sec"]; ok {
		t.Fatalf("never-handshaken peer acquired a handshake: %+v", offline)
	}
	if offline["allowed_ips"] != "100.64.0.10/32" {
		t.Fatalf("double prefix: %+v", offline)
	}
}

func TestSpokeCannotOverwriteHubStatus(t *testing.T) {
	// A nil DB intentionally makes accidental persistence fail this test.
	p := &Plugin{}
	id := int64(26)
	h := &WGHub{BoundDeviceID: &id, Pubkey: "hub"}
	status := &wgHeartbeatStatus{Schema: 1, Role: "hub", Iface: "wgc0"}
	if err := p.storeHubHeartbeatStatus(&WGDevice{ID: 46, Pubkey: "spoke"}, h, status, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := p.storeHubHeartbeatStatus(&WGDevice{ID: 26, Pubkey: "old-hub-key"}, h, status, time.Now()); err != nil {
		t.Fatal(err)
	}
}

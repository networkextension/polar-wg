package wg

// Unit tests for hubStatusCache. Mirrors the dock-side
// wg_peer_cache_test.go shape: upsert + replace + lookup + freshness.

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHubStatusCache_UpsertAndLookup(t *testing.T) {
	c := newHubStatusCache(30 * time.Second)
	now := time.Now().UTC()
	c.upsert(wgHubStatusSample{
		HostID:         "host-a",
		Iface:          "wg0",
		IfacePublicKey: "PUBKEY-1",
		RunID:          11,
		RecordedAt:     now,
		Data:           map[string]any{"peer_count": float64(3)},
	})
	got, ok, stale := c.lookup("PUBKEY-1")
	if !ok {
		t.Fatalf("lookup miss for known pubkey")
	}
	if stale {
		t.Fatalf("fresh sample reported stale")
	}
	if got.HostID != "host-a" || got.Iface != "wg0" {
		t.Fatalf("lookup payload mismatch: %+v", got)
	}
}

func TestHubStatusCache_LookupMisses(t *testing.T) {
	c := newHubStatusCache(30 * time.Second)
	if _, ok, _ := c.lookup(""); ok {
		t.Fatalf("empty pubkey should miss")
	}
	if _, ok, _ := c.lookup("UNKNOWN"); ok {
		t.Fatalf("unknown pubkey should miss")
	}
}

func TestHubStatusCache_UpsertEmptyPubkeyIgnored(t *testing.T) {
	c := newHubStatusCache(30 * time.Second)
	c.upsert(wgHubStatusSample{IfacePublicKey: "", HostID: "h"})
	if c.size() != 0 {
		t.Fatalf("empty pubkey should be dropped, size=%d", c.size())
	}
}

func TestHubStatusCache_UpsertReplacesSamePubkey(t *testing.T) {
	c := newHubStatusCache(30 * time.Second)
	c.upsert(wgHubStatusSample{IfacePublicKey: "P", Data: map[string]any{"peer_count": float64(1)}})
	c.upsert(wgHubStatusSample{IfacePublicKey: "P", Data: map[string]any{"peer_count": float64(7)}})
	if c.size() != 1 {
		t.Fatalf("expected one entry after upsert-overwrite, got %d", c.size())
	}
	got, _, _ := c.lookup("P")
	if pc, _ := got.Data["peer_count"].(float64); pc != 7 {
		t.Fatalf("peer_count=%v, want 7", pc)
	}
}

func TestHubStatusCache_StalenessThreshold(t *testing.T) {
	c := newHubStatusCache(10 * time.Second) // freshTTL = 20s
	// Recorded 1m ago — should be flagged stale.
	c.upsert(wgHubStatusSample{
		IfacePublicKey: "STALE",
		RecordedAt:     time.Now().Add(-1 * time.Minute),
	})
	_, ok, stale := c.lookup("STALE")
	if !ok {
		t.Fatalf("stale sample shouldn't disappear, just be flagged")
	}
	if !stale {
		t.Fatalf("expected stale=true for 60s-old sample with 20s TTL")
	}
}

func TestHubStatusCache_ReplaceDropsMissing(t *testing.T) {
	c := newHubStatusCache(30 * time.Second)
	c.upsert(wgHubStatusSample{IfacePublicKey: "OLD"})
	c.upsert(wgHubStatusSample{IfacePublicKey: "KEEP"})
	// Replace with only KEEP — OLD should vanish.
	c.replace([]wgHubStatusSample{{IfacePublicKey: "KEEP", HostID: "h2"}})
	if _, ok, _ := c.lookup("OLD"); ok {
		t.Fatalf("replace() should drop entries not in the new set")
	}
	if _, ok, _ := c.lookup("KEEP"); !ok {
		t.Fatalf("KEEP should survive replace")
	}
}

func TestDecodeHubStatusSample_DockFormat(t *testing.T) {
	// Matches the exact shape dock's wg_peer_status_handlers.go emits.
	raw := json.RawMessage(`{
		"host_id": "host-x",
		"iface": "wg0",
		"iface_public_key": "PUBKEY-9",
		"run_id": 42,
		"recorded_at": "2026-05-27T10:11:12.000Z",
		"data": {"peer_count": 3, "listen_port": 51820, "peers": []}
	}`)
	s, ok := decodeHubStatusSample(raw)
	if !ok {
		t.Fatalf("decode should succeed")
	}
	if s.IfacePublicKey != "PUBKEY-9" || s.RunID != 42 {
		t.Fatalf("decoded sample mismatch: %+v", s)
	}
	want, _ := time.Parse(time.RFC3339, "2026-05-27T10:11:12Z")
	if !s.RecordedAt.Equal(want) {
		t.Fatalf("recorded_at parse mismatch: got %v want %v", s.RecordedAt, want)
	}
	if pc, _ := s.Data["peer_count"].(float64); pc != 3 {
		t.Fatalf("peer_count = %v, want 3", pc)
	}
}

func TestHubStatusPollInterval_EnvClamp(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset → default 30s", "", 30 * time.Second},
		{"valid → as-given", "45", 45 * time.Second},
		{"below min → default", "2", 30 * time.Second},
		{"above max → clamped 300", "9999", 300 * time.Second},
		{"garbage → default", "abc", 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hubStatusPollEnv, tc.env)
			if got := hubStatusPollInterval(); got != tc.want {
				t.Fatalf("hubStatusPollInterval(%q)=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestBuildHubStatusEntry_FieldSplit(t *testing.T) {
	s := wgHubStatusSample{
		HostID:     "h",
		Iface:      "wg0",
		RecordedAt: time.Now().UTC(),
		Data: map[string]any{
			"peer_count":  float64(2),
			"listen_port": float64(51820),
			"peers":       []any{map[string]any{"public_key": "p1"}},
			"sampled_at":  "2026-05-27T10:11:12Z",
		},
	}
	out := buildHubStatusEntry(s, false)
	if out.PeerCount != 2 {
		t.Fatalf("peer_count=%d, want 2", out.PeerCount)
	}
	if out.ListenPort != 51820 {
		t.Fatalf("listen_port=%d, want 51820", out.ListenPort)
	}
	if len(out.Peers) != 1 {
		t.Fatalf("peers len=%d, want 1", len(out.Peers))
	}
	if _, ok := out.Extra["sampled_at"]; !ok {
		t.Fatalf("expected extra.sampled_at present")
	}
	if _, ok := out.Extra["peer_count"]; ok {
		t.Fatalf("peer_count shouldn't leak into extra")
	}
}

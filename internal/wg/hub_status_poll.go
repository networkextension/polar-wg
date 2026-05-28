package wg

// hub_status_poll.go — polls dock's `GET /internal/v1/wg-peer-status`
// every ~30s, caches the latest sample per hub iface pubkey in memory.
//
// The agent emits `skill.event{metric, wg_peer_status}` frames from
// each WireGuard hub box; dock (≥ polar-dock#345) caches the last
// frame per (host_id, iface). This plugin doesn't talk to the agent
// directly — we just consume dock's read endpoint and join each
// sample's iface_public_key against wg_hubs.pubkey to render a per-hub
// status table in the admin UI.
//
// Cache shape:
//   key   = iface_public_key (base64) — matches wg_hubs.pubkey
//   value = latest wgHubStatusSample
//
// Stale entries (recorded_at older than 2 * poll_interval) are
// filtered at read time, so a hub that stops reporting shows up as
// "stale" in the UI rather than ghost-fresh data.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	hubStatusPollEnv             = "POLAR_WG_HUB_STATUS_POLL_SEC"
	hubStatusPollDefaultSec      = 30
	hubStatusPollMinSec          = 5
	hubStatusPollMaxSec          = 300
	hubStatusDockPath            = "/internal/v1/wg-peer-status"
	hubStatusNotFoundLogInterval = 60 * time.Second
)

// wgHubStatusSample is one cached sample, keyed by iface pubkey.
// Mirrors the dock response entry verbatim — `Data` is the agent's
// payload (peer_count, peers[], listen_port, …) opaque to this layer.
type wgHubStatusSample struct {
	HostID         string         `json:"host_id"`
	Iface          string         `json:"iface"`
	IfacePublicKey string         `json:"iface_public_key"`
	RunID          int64          `json:"run_id"`
	RecordedAt     time.Time      `json:"recorded_at"`
	Data           map[string]any `json:"data"`
}

// hubStatusCache — thread-safe map of iface_public_key → latest sample.
type hubStatusCache struct {
	mu      sync.RWMutex
	entries map[string]wgHubStatusSample
	// freshTTL — read-time cutoff. Entries older than this are skipped
	// by lookup(); we don't actively delete them because upsert() will
	// overwrite on next refresh. Set to 2 * poll interval so one missed
	// fetch doesn't immediately blink hubs out.
	freshTTL time.Duration
}

func newHubStatusCache(pollInterval time.Duration) *hubStatusCache {
	return &hubStatusCache{
		entries:  map[string]wgHubStatusSample{},
		freshTTL: 2 * pollInterval,
	}
}

// upsert stores the latest sample for an iface pubkey. Silently
// no-ops on empty pubkey so a malformed dock frame never clobbers
// a good cache entry.
func (c *hubStatusCache) upsert(s wgHubStatusSample) {
	if s.IfacePublicKey == "" {
		return
	}
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}
	c.mu.Lock()
	c.entries[s.IfacePublicKey] = s
	c.mu.Unlock()
}

// replace atomically swaps the entry set — used after a successful
// poll so hubs that stopped reporting drop out of the map (rather
// than relying purely on the freshness TTL).
func (c *hubStatusCache) replace(samples []wgHubStatusSample) {
	next := make(map[string]wgHubStatusSample, len(samples))
	for _, s := range samples {
		if s.IfacePublicKey == "" {
			continue
		}
		if s.RecordedAt.IsZero() {
			s.RecordedAt = time.Now().UTC()
		}
		next[s.IfacePublicKey] = s
	}
	c.mu.Lock()
	c.entries = next
	c.mu.Unlock()
}

// lookup returns the sample for an iface pubkey and a stale flag.
// stale=true when the sample exists but its RecordedAt is older than
// freshTTL — caller (the UI handler) reports it as stale so the
// operator can tell "hub hasn't reported in N minutes" from
// "hub never reported".
func (c *hubStatusCache) lookup(pubkey string) (wgHubStatusSample, bool, bool) {
	if pubkey == "" {
		return wgHubStatusSample{}, false, false
	}
	c.mu.RLock()
	s, ok := c.entries[pubkey]
	c.mu.RUnlock()
	if !ok {
		return wgHubStatusSample{}, false, false
	}
	stale := time.Since(s.RecordedAt) > c.freshTTL
	return s, true, stale
}

// size — diagnostic only (used by tests).
func (c *hubStatusCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// hubStatusPollInterval reads POLAR_WG_HUB_STATUS_POLL_SEC, clamped
// to [5, 300]; falls back to 30s on missing/invalid.
func hubStatusPollInterval() time.Duration {
	v := os.Getenv(hubStatusPollEnv)
	if v == "" {
		return hubStatusPollDefaultSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < hubStatusPollMinSec {
		return hubStatusPollDefaultSec * time.Second
	}
	if n > hubStatusPollMaxSec {
		n = hubStatusPollMaxSec
	}
	return time.Duration(n) * time.Second
}

// startHubStatusPoll wires p.hubStatus + spins the background goroutine.
// Idempotent guard: if p.hubStatus already exists (test setup), reuse it.
func (p *Plugin) startHubStatusPoll(ctx context.Context) {
	interval := hubStatusPollInterval()
	if p.hubStatus == nil {
		p.hubStatus = newHubStatusCache(interval)
	}
	go p.hubStatusPollLoop(ctx, interval)
}

func (p *Plugin) hubStatusPollLoop(ctx context.Context, interval time.Duration) {
	// One immediate fetch so the UI has data on first load (within
	// ~1s of plugin start) instead of waiting a full poll interval.
	p.fetchHubStatusOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastNotFoundLog time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			notFound := p.fetchHubStatusOnce(ctx)
			// Throttle the 404 log to once per minute: while the dock
			// hasn't been upgraded to PR #345 yet, the endpoint returns
			// 404 every poll, which would otherwise spam the log.
			if notFound && time.Since(lastNotFoundLog) >= hubStatusNotFoundLogInterval {
				log.Printf("wg: hub-status: dock endpoint %s returned 404 (dock not yet upgraded to PR #345?)", hubStatusDockPath)
				lastNotFoundLog = time.Now()
			}
		}
	}
}

// fetchHubStatusOnce — one round-trip to dock. Returns true if dock
// responded 404 (so the caller can throttle that specific log line);
// other errors are logged inline at INFO and don't surface.
func (p *Plugin) fetchHubStatusOnce(ctx context.Context) (notFound bool) {
	if p.Dock == nil {
		return false
	}
	_ = ctx // dock SDK uses its own HTTP client + timeout; ctx is for shape parity.
	resp, err := p.Dock.Do(http.MethodGet, hubStatusDockPath, nil)
	if err != nil {
		log.Printf("wg: hub-status: fetch failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("wg: hub-status: dock HTTP %d: %s", resp.StatusCode, truncBody(body))
		return false
	}
	var payload struct {
		Samples []json.RawMessage `json:"samples"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		log.Printf("wg: hub-status: decode failed: %v", err)
		return false
	}
	samples := make([]wgHubStatusSample, 0, len(payload.Samples))
	for _, raw := range payload.Samples {
		s, ok := decodeHubStatusSample(raw)
		if !ok {
			continue
		}
		samples = append(samples, s)
	}
	p.hubStatus.replace(samples)
	pks := make([]string, 0, len(samples))
	for _, s := range samples {
		pk := s.IfacePublicKey
		if len(pk) > 12 {
			pk = pk[:12] + "…"
		}
		pks = append(pks, pk)
	}
	log.Printf("wg: hub-status: fetched %d samples from dock; cache size %d; pubkeys=%v", len(samples), p.hubStatus.size(), pks)
	return false
}

// decodeHubStatusSample handles dock's recorded_at being a string —
// json.Unmarshal won't auto-parse "2026-..." into time.Time without
// the layout matching, so we decode loose first then coerce.
func decodeHubStatusSample(raw json.RawMessage) (wgHubStatusSample, bool) {
	var loose struct {
		HostID         string         `json:"host_id"`
		Iface          string         `json:"iface"`
		IfacePublicKey string         `json:"iface_public_key"`
		RunID          int64          `json:"run_id"`
		RecordedAt    string          `json:"recorded_at"`
		Data           map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &loose); err != nil {
		return wgHubStatusSample{}, false
	}
	ts, err := parseHubStatusTime(loose.RecordedAt)
	if err != nil {
		// Fall back to now() so the entry isn't dropped just because
		// of a clock-format mismatch.
		ts = time.Now().UTC()
	}
	return wgHubStatusSample{
		HostID:         loose.HostID,
		Iface:          loose.Iface,
		IfacePublicKey: loose.IfacePublicKey,
		RunID:          loose.RunID,
		RecordedAt:     ts,
		Data:           loose.Data,
	}, true
}

// parseHubStatusTime accepts dock's millisecond-precision RFC 3339
// layout (the format wg_peer_status_handlers.go emits) and also the
// generic time.RFC3339Nano so future dock changes don't break us.
func parseHubStatusTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	layouts := []string{
		"2006-01-02T15:04:05.000Z",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

func truncBody(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

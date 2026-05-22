package wg

// Stale-device garbage collection for the wg-mac mesh.
//
// A device that hasn't heart-beated in WGStaleDeviceTTL (default 7d)
// is auto-marked removed_at = now(). The hub it was bound to has its
// pubkey + bound_device_id cleared so a fresh hub-token can reclaim
// the slot. This keeps the admin Devices tab from filling up with
// ghost entries after laptops get unplugged for vacations / rebuilds.
//
// Runs in a single goroutine started from server bootstrap (app.go),
// cancellable via the parent context. Best-effort: any DB error is
// logged and the loop tries again on the next tick.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"time"
)

// Default thresholds — overridable at deploy time via env so operators
// can tune without a rebuild.
const (
	wgStaleDeviceTTLDefault = 7 * 24 * time.Hour // 7 days
	wgStaleGCIntervalDefault = 1 * time.Hour
)

// envIntDefault parses an integer env var, returning fallback on
// missing/invalid input. Shared helper for pool sizing + GC tunables.
func envIntDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func wgStaleDeviceTTL() time.Duration {
	if v := os.Getenv("WG_STALE_DEVICE_TTL_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return wgStaleDeviceTTLDefault
}

func wgStaleGCInterval() time.Duration {
	if v := os.Getenv("WG_STALE_GC_INTERVAL_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return wgStaleGCIntervalDefault
}

// startWGStaleDeviceGC kicks off the background sweeper. Returns
// immediately. Loop exits when ctx is cancelled.
func (p *Plugin) startWGStaleDeviceGC(ctx context.Context) {
	if p.DB == nil {
		return
	}
	interval := wgStaleGCInterval()
	ttl := wgStaleDeviceTTL()
	log.Printf("wg-mac: starting stale-device GC (TTL=%s, interval=%s)", ttl, interval)
	go func() {
		// Initial delay so we don't spike DB right at startup.
		t := time.NewTimer(2 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := p.sweepStaleWGDevices(ctx, ttl)
				if err != nil {
					log.Printf("wg-mac: stale-device GC error: %v", err)
					p.metrics.recordWGStaleGC("error", 0)
				} else if n > 0 {
					log.Printf("wg-mac: stale-device GC marked %d device(s) removed", n)
					p.metrics.recordWGStaleGC("ok", n)
				} else {
					p.metrics.recordWGStaleGC("ok", 0)
				}
				t.Reset(interval)
			}
		}
	}()
}

// sweepStaleWGDevices marks removed_at on devices that haven't been
// seen within ttl. Also clears any hub binding pointing at a freshly-
// removed device so the next hub-token can rebind.
//
// SQL: a device is "stale" if either
//   - last_seen_at IS NOT NULL AND last_seen_at < now() - ttl, OR
//   - last_seen_at IS NULL AND created_at < now() - ttl (never reported)
func (p *Plugin) sweepStaleWGDevices(ctx context.Context, ttl time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-ttl)
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Collect ids first so we can also reset hub bindings in the same txn.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, hub_id FROM wg_devices
		  WHERE removed_at IS NULL
		    AND (
		         (last_seen_at IS NOT NULL AND last_seen_at < $1)
		      OR (last_seen_at IS NULL AND created_at < $1)
		    )`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	type victim struct {
		id    int64
		hubID int64
	}
	victims := make([]victim, 0)
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.hubID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		victims = append(victims, v)
	}
	_ = rows.Close()
	if len(victims) == 0 {
		return 0, tx.Commit()
	}

	// Mark removed.
	for _, v := range victims {
		if _, err := tx.ExecContext(ctx,
			`UPDATE wg_devices SET removed_at = now() WHERE id = $1 AND removed_at IS NULL`,
			v.id,
		); err != nil {
			return 0, err
		}
		// If the victim was a hub's bound device, clear the binding so
		// admin (or a fresh hub-token register) can rebind.
		if _, err := tx.ExecContext(ctx,
			`UPDATE wg_hubs
			    SET pubkey = '', bound_device_id = NULL, updated_at = now()
			  WHERE bound_device_id = $1`,
			v.id,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(victims), nil
}

// sampleWGTokenExpiry counts tokens that will expire within 7d / 30d
// so operators can spot upcoming auth failures before they happen.
// Cheap (2 indexed queries); piggy-backs on the same 10s sample tick.
func (p *Plugin) sampleWGTokenExpiry() {
	if p.DB == nil || p.metrics == nil {
		return
	}
	now := time.Now().UTC()
	cases := []struct {
		within string
		when   time.Time
	}{
		{"7d", now.Add(7 * 24 * time.Hour)},
		{"30d", now.Add(30 * 24 * time.Hour)},
	}
	for _, c := range cases {
		var n int
		if err := p.DB.QueryRow(
			`SELECT COUNT(*) FROM wg_devices
			  WHERE removed_at IS NULL
			    AND token_expires_at IS NOT NULL
			    AND token_expires_at < $1`,
			c.when,
		).Scan(&n); err == nil {
			p.metrics.wgTokensExpiringSoon.WithLabelValues(c.within).Set(float64(n))
		}
	}
}

// Silence the unused-import lint when this file is alone in a build
// that doesn't include sql.* helpers. (Kept defensively.)
var _ = sql.ErrNoRows

package wg

// Assets glue (doc/arch/blob-storage-to-assets-migration.md): wg bundle
// blobs live exclusively in the central polar-assets catalog. Bundles are
// platform-owned (WorkspaceID=nil) and public (install.sh fetches them
// unauthenticated). Upload single-writes to assets; download streams from
// assets. No wg-svc-local blob storage (the transitional dual-read +
// backfill were removed at cutover).

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	sdk "github.com/networkextension/polar-sdk"
)

// randHex returns 2n lowercase hex chars of CSPRNG output — used to make
// a unique bundle version label when the operator doesn't supply one.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ensureBundleAssetColumn adds wg_bundles.asset_id if it's missing. The
// table predates the assets catalog (created in the dock era), so this
// is the migration step. Idempotent.
func (p *Plugin) ensureBundleAssetColumn() error {
	_, err := p.DB.Exec(`ALTER TABLE wg_bundles ADD COLUMN IF NOT EXISTS asset_id BIGINT`)
	return err
}

// ensureBundleOSArchColumns adds the os/arch dimension to wg_bundles so
// per-platform bundles can coexist. Idempotent. Existing (macOS-only) rows
// default to os='darwin', arch=” (universal). "latest" + version-uniqueness
// move from global to per-(os,arch).
func (p *Plugin) ensureBundleOSArchColumns() error {
	stmts := []string{
		`ALTER TABLE wg_bundles ADD COLUMN IF NOT EXISTS os TEXT NOT NULL DEFAULT 'darwin'`,
		`ALTER TABLE wg_bundles ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT ''`,
		`DROP INDEX IF EXISTS ux_wg_bundles_latest`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_wg_bundles_latest_platform ON wg_bundles(os, arch) WHERE is_latest = TRUE`,
		`ALTER TABLE wg_bundles DROP CONSTRAINT IF EXISTS wg_bundles_version_key`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_wg_bundles_version_platform ON wg_bundles(version, os, arch)`,
	}
	for _, s := range stmts {
		if _, err := p.DB.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// ensureEgressColumns adds the P2 egress columns: wg_hubs.advertised_routes_json
// (operator-declared egress CIDRs) + wg_devices.egress_hub_id (per-device
// opt-in). Idempotent. See doc/wg-multi-hub-routing.md 出口 section.
func (p *Plugin) ensureEgressColumns() error {
	stmts := []string{
		`ALTER TABLE wg_hubs ADD COLUMN IF NOT EXISTS advertised_routes_json JSONB`,
		`ALTER TABLE wg_devices ADD COLUMN IF NOT EXISTS egress_hub_id BIGINT REFERENCES wg_hubs(id) ON DELETE SET NULL`,
		// DNS/HTTP-proxy push (doc/wg-dns-proxy-push-design.md): operator policy
		// distributed to devices in the /v1/peers response.
		`ALTER TABLE wg_hubs ADD COLUMN IF NOT EXISTS policy_json JSONB`,
		// isolated devices (cloud VMs behind a host NAT that blocks guest↔guest
		// traffic) never take part in same-site LAN-direct peering: hub-only.
		`ALTER TABLE wg_devices ADD COLUMN IF NOT EXISTS isolated BOOLEAN NOT NULL DEFAULT FALSE`,
	}
	for _, s := range stmts {
		if _, err := p.DB.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// normWGOS canonicalizes an OS string. "" stays "" (caller defaults to darwin).
func normWGOS(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "darwin", "macos", "mac", "osx":
		return "darwin"
	case "windows", "win":
		return "windows"
	default:
		return t
	}
}

// normWGArch canonicalizes an arch string. "" = universal/any.
func normWGArch(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "x86_64", "amd64", "x64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func (p *Plugin) setWGBundleAssetID(id, assetID int64) error {
	_, err := p.DB.Exec(`UPDATE wg_bundles SET asset_id = $2 WHERE id = $1`, id, assetID)
	return err
}

// getWGBundleAssetID returns (assetID, true) if the row has a non-NULL
// asset_id, else (0, false).
func (p *Plugin) getWGBundleAssetID(id int64) (int64, bool, error) {
	var a sql.NullInt64
	if err := p.DB.QueryRow(`SELECT asset_id FROM wg_bundles WHERE id = $1`, id).Scan(&a); err != nil {
		return 0, false, err
	}
	if !a.Valid {
		return 0, false, nil
	}
	return a.Int64, true, nil
}

// streamBundleFromAssets serves the bundle from the central assets
// catalog. Returns false (no body written) when the row has no asset_id
// or the fetch fails, so the caller can emit an error.
func (p *Plugin) streamBundleFromAssets(c *gin.Context, b *WGBundle) bool {
	assetID, ok, err := p.getWGBundleAssetID(b.ID)
	if err != nil || !ok {
		return false
	}
	resp, err := p.Dock.AssetDownload(&sdk.AssetMeta{ID: assetID})
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false
	}
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, bundleFilename(b)))
	c.DataFromReader(http.StatusOK, resp.ContentLength, "application/gzip", resp.Body, nil)
	return true
}

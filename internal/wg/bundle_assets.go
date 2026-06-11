package wg

// Assets migration (doc/arch/blob-storage-to-assets-migration.md): wg
// bundle blobs are moving from wg-svc-local disk to the central
// polar-assets catalog. This file holds the dual-write / dual-read /
// backfill glue. Bundles are platform-owned (WorkspaceID=nil) and
// public (install.sh fetches them unauthenticated).

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	sdk "github.com/networkextension/polar-sdk"
)

// backfillBundleAssetsOnce migrates any local-only bundle blobs into the
// central assets catalog on startup. Idempotent: rows that already carry
// an asset_id are skipped, so after the one-time migration it's a few
// cheap DB reads and zero dock calls. Run in a goroutine from Start().
func (p *Plugin) backfillBundleAssetsOnce() {
	bundles, err := p.listWGBundles()
	if err != nil {
		log.Printf("wg: bundle backfill: list: %v", err)
		return
	}
	migrated := 0
	for i := range bundles {
		b := &bundles[i]
		if _, ok, _ := p.getWGBundleAssetID(b.ID); ok {
			continue
		}
		abs, err := p.resolveWGBundleBlobPath(b)
		if err != nil {
			log.Printf("wg: bundle backfill: %s: %v (skip)", b.Version, err)
			continue
		}
		assetID, err := p.uploadBundleToAssets(b, abs)
		if err != nil {
			log.Printf("wg: bundle backfill: %s: assets upload: %v", b.Version, err)
			continue
		}
		if err := p.setWGBundleAssetID(b.ID, assetID); err != nil {
			log.Printf("wg: bundle backfill: %s: set asset_id: %v", b.Version, err)
			continue
		}
		migrated++
		log.Printf("wg: bundle backfill: migrated %s -> asset %d", b.Version, assetID)
	}
	if migrated > 0 {
		log.Printf("wg: bundle backfill: migrated %d bundle(s) to assets", migrated)
	}
}

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
// default to os='darwin', arch='' (universal). "latest" + version-uniqueness
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

// uploadBundleToAssets registers the local blob in the central assets
// catalog (platform-owned, public, content-addressed by the bundle's
// own sha256). Returns the catalog asset id.
func (p *Plugin) uploadBundleToAssets(b *WGBundle, localAbs string) (int64, error) {
	f, err := os.Open(localAbs)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	meta, err := p.Dock.AssetUpload(sdk.AssetUploadInput{
		Kind:       "package",
		Name:       "wg-bundles/" + b.Version,
		Version:    "v1",
		Visibility: "public",
		Mime:       "application/gzip",
		Metadata:   map[string]any{"wg_bundle_version": b.Version},
	}, f)
	if err != nil {
		return 0, err
	}
	return meta.ID, nil
}

// streamBundleFromAssets serves the bundle from the central assets
// catalog. Returns false (caller falls back to local) when the row has
// no asset_id, or the fetch fails — so a flaky assets-svc degrades to
// the local blob rather than erroring.
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
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="wg-mac-%s.tar.gz"`, sanitizeFilename(b.Version)))
	c.DataFromReader(http.StatusOK, resp.ContentLength, "application/gzip", resp.Body, nil)
	return true
}

// handleAdminWGBundleBackfillAssets uploads any local-only bundle blobs
// into the assets catalog + records asset_id. Idempotent: rows that
// already have asset_id are skipped, and assets dedups by sha256.
func (p *Plugin) handleAdminWGBundleBackfillAssets(c *gin.Context) {
	bundles, err := p.listWGBundles()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	migrated, skipped, failed := 0, 0, 0
	errs := []string{}
	for i := range bundles {
		b := &bundles[i]
		if _, ok, _ := p.getWGBundleAssetID(b.ID); ok {
			skipped++
			continue
		}
		abs, rerr := p.resolveWGBundleBlobPath(b)
		if rerr != nil {
			failed++
			errs = append(errs, b.Version+": "+rerr.Error())
			continue
		}
		assetID, uerr := p.uploadBundleToAssets(b, abs)
		if uerr != nil {
			failed++
			errs = append(errs, b.Version+": "+uerr.Error())
			continue
		}
		if serr := p.setWGBundleAssetID(b.ID, assetID); serr != nil {
			failed++
			errs = append(errs, b.Version+": "+serr.Error())
			continue
		}
		migrated++
	}
	c.JSON(http.StatusOK, gin.H{
		"migrated": migrated, "skipped": skipped, "failed": failed, "errors": errs,
	})
}

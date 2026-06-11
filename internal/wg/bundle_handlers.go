package wg

// /api/admin/wg-bundles upload + /v1/bundle download.
//
// Storage is the central polar-assets catalog (single-write): uploads go
// straight to assets via the SDK, never to wg-svc-local disk. Legacy
// rows with a file:// blob_uri still read from local disk until the
// one-time backfill migrates them. See
// doc/arch/blob-storage-to-assets-migration.md (polar-dock).

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	sdk "github.com/networkextension/polar-sdk"
)

func (p *Plugin) wgBundleBlobDirAbs() string {
	return filepath.Join(p.UploadDir, "wg-bundles")
}

// POST /api/admin/wg-bundles/upload — multipart form.
// Fields:
//   file     (required) the wg-mac tarball
//   version  (optional) e.g. "20260517-e360fd07"; defaults to ts+sha[:8]
//   notes    (optional)
//   set_latest (optional, "1" to immediately flip is_latest)
func (p *Plugin) handleAdminWGBundleUpload(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	header, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required (multipart field 'file')"})
		return
	}
	version := strings.TrimSpace(c.PostForm("version"))
	notes := strings.TrimSpace(c.PostForm("notes"))
	setLatest := c.PostForm("set_latest") == "1"
	// Platform of this bundle (packaging is polar-wg-app's job; we just tag it).
	// Default darwin/'' keeps the original macOS-only upload flow working.
	bundleOS := normWGOS(c.PostForm("os"))
	if bundleOS == "" {
		bundleOS = "darwin"
	}
	bundleArch := normWGArch(c.PostForm("arch"))
	if version == "" {
		version = time.Now().UTC().Format("20060102") + "-" + randHex(4)
	}

	src, err := header.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "open uploaded file: " + err.Error()})
		return
	}
	defer src.Close()

	// Single-write: stream straight into the central assets catalog. No
	// wg-svc-local copy — the asset platform owns the bytes. The catalog
	// key is unique per bundle version; the bytes are content-addressed
	// (deduped) by sha256 inside assets.
	meta, err := p.Dock.AssetUpload(sdk.AssetUploadInput{
		Kind:       "package",
		Name:       "wg-bundles/" + version,
		Version:    "v1",
		Visibility: "public",
		Mime:       "application/gzip",
		Metadata:   map[string]any{"wg_bundle_version": version},
	}, src)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "assets upload failed: " + err.Error()})
		return
	}
	if meta.SizeBytes == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is empty"})
		return
	}
	sum := meta.SHA256

	// sha256 dedup at the wg layer (assets already deduped the bytes).
	if existing, err := p.getWGBundleBySHA(sum); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db: " + err.Error()})
		return
	} else if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":    "bundle with this sha256 already exists",
			"existing": existing,
		})
		return
	}
	b := &WGBundle{
		Version:       version,
		OS:            bundleOS,
		Arch:          bundleArch,
		BlobURI:       "asset://" + strconv.FormatInt(meta.ID, 10),
		BlobSHA256:    sum,
		SizeBytes:     meta.SizeBytes,
		Notes:         notes,
		AddedByUserID: userIDStr,
	}
	out, err := p.insertWGBundle(b)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "version or sha256 already exists",
				"version": version,
				"sha256":  sum,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db: " + err.Error()})
		return
	}
	if err := p.setWGBundleAssetID(out.ID, meta.ID); err != nil {
		log.Printf("wg: bundle %s set asset_id=%d: %v", out.Version, meta.ID, err)
	}
	if setLatest {
		if err := p.setWGBundleLatest(out.ID); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"bundle":  out,
				"warning": "uploaded but failed to mark latest: " + err.Error(),
			})
			return
		}
		out.IsLatest = true
	}
	c.JSON(http.StatusOK, gin.H{"bundle": out})
}

func (p *Plugin) handleAdminWGBundleList(c *gin.Context) {
	bundles, err := p.listWGBundles()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"bundles": bundles})
}

func (p *Plugin) handleAdminWGBundleSetLatest(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := p.setWGBundleLatest(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (p *Plugin) handleAdminWGBundleDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	b, err := p.getWGBundleByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if b == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := p.deleteWGBundle(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	// Best-effort blob unlink; ignore failure (file may already be gone
	// or shared with another bundle row in a hypothetical future).
	if abs, err := p.resolveWGBundleBlobPath(b); err == nil {
		_ = os.Remove(abs)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GET /v1/bundle (302 → /v1/bundle/<latest>) and GET /v1/bundle/:version.
// Unauthenticated by design: install.sh has no creds yet at curl-bash
// time. Bundle contents are public; the join secret is the --token=
// arg the operator passes to the script.

// reqOSArch reads + normalizes the ?os=/&arch= query params (OS defaults to
// darwin so the original macOS bundles resolve with no params).
func reqOSArch(c *gin.Context) (os, arch string) {
	os = normWGOS(c.Query("os"))
	if os == "" {
		os = "darwin"
	}
	return os, normWGArch(c.Query("arch"))
}

func (p *Plugin) handleWGBundleLatest(c *gin.Context) {
	os, arch := reqOSArch(c)
	latest, err := p.getLatestWGBundleFor(os, arch)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if latest == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no latest bundle for %s/%s — admin must upload + mark latest", os, archOrAny(arch))})
		return
	}
	// Preserve the platform on the redirect so the versioned download resolves
	// the same (os, arch) bundle.
	c.Redirect(http.StatusFound, fmt.Sprintf("/v1/bundle/%s?os=%s&arch=%s", latest.Version, os, latest.Arch))
}

func archOrAny(a string) string {
	if a == "" {
		return "any"
	}
	return a
}

func (p *Plugin) handleWGBundleDownload(c *gin.Context) {
	version := strings.TrimSpace(c.Param("version"))
	if version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version required"})
		return
	}
	os, arch := reqOSArch(c)
	b, err := p.getWGBundleForVersion(version, os, arch)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if b == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no bundle %s for %s/%s", version, os, archOrAny(arch))})
		return
	}
	// Dual-read: prefer the central assets catalog; fall back to the local
	// blob if the bundle hasn't been migrated yet (or assets is down).
	if p.streamBundleFromAssets(c, b) {
		return
	}
	abs, err := p.resolveWGBundleBlobPath(b)
	if err != nil {
		c.JSON(http.StatusGone, gin.H{"error": err.Error()})
		return
	}
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, bundleFilename(b)))
	c.File(abs)
}

// bundleFilename names the download by platform, e.g. wg-darwin-arm64-<ver>.tar.gz
// (arch omitted when universal).
func bundleFilename(b *WGBundle) string {
	mid := b.OS
	if b.Arch != "" {
		mid += "-" + b.Arch
	}
	return fmt.Sprintf("wg-%s-%s.tar.gz", mid, sanitizeFilename(b.Version))
}

// resolveWGBundleBlobPath canonicalizes the row's file:// URI and
// confirms it lives under the wg-bundles dir. Defends against hand-
// crafted DB rows pointing at /etc/passwd.
func (p *Plugin) resolveWGBundleBlobPath(b *WGBundle) (string, error) {
	if !strings.HasPrefix(b.BlobURI, "file://") {
		return "", errors.New("blob is not locally stored (remote URI)")
	}
	abs := strings.TrimPrefix(b.BlobURI, "file://")
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("bundle blob file missing on disk")
	}
	if !strings.HasPrefix(canon, p.wgBundleBlobDirAbs()+string(filepath.Separator)) {
		return "", errors.New("blob path outside the wg-bundles dir")
	}
	return canon, nil
}

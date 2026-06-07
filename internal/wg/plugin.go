// Package wg is the wg-mac control plane plugin. Phase 1-D-1 moved the
// /v1/* device handlers + /api/admin/wg-* admin CRUD + GC sweeper out
// of dock into this package; dock still has its own copy and still
// serves all traffic (no nginx change yet). Phase 1-D-2 flips nginx
// and removes the dock copies.
//
// Design:
//   - Each plugin owns its DB. polar_wg is a separate Postgres
//     database (same instance OK). Cross-domain user/team lookups
//     go via the SDK's HMAC client (sdk.Client).
//   - HTTP server is a plain gin engine — wg-svc's cmd/main.go
//     wires it up. The plugin doesn't manage process lifecycle.
//   - Heartbeat goroutine pings dock every 60s so /admin-plugins.html
//     shows fresh state and so plugin GC (Phase 1-D) can spot dead
//     workers.
//   - Headscale integration stays with dock through Phase 1-D-1. The
//     /api/admin/wg-tokens handler here returns no tailscale_authkey;
//     operators who need the Tailscale PreAuthKey continue using
//     dock's path until Phase 1-D-2 resolves Headscale's location.
package wg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/networkextension/polar-sdk"
)

// Plugin holds the runtime state for wg-svc.
type Plugin struct {
	DB         *sql.DB
	Dock       *sdk.Client
	Name       string
	Listen     string
	Ver        string
	UploadDir  string // wg-svc-local bundle blob root (e.g. /Users/local/wg-svc-data)
	MetricsTok string // Bearer token for /metrics; empty = endpoint 404

	metrics   *wgMetrics
	startedAt time.Time

	// hubStatus caches the latest `wg_peer_status` sample per hub
	// iface pubkey, polled from dock's /internal/v1/wg-peer-status.
	// Populated by startHubStatusPoll() in Start(ctx).
	hubStatus *hubStatusCache
}

type Config struct {
	DBDSN        string
	DockBase     string
	PluginName   string
	PluginToken  string
	Listen       string
	BuildVersion string
	UploadDir    string
	MetricsToken string
}

// New opens the DB pool, builds the dock SDK client, verifies the HMAC
// handshake via /internal/v1/ping, registers metrics. Caller must then
// call RegisterRoutes(engine) + Start(ctx).
func New(ctx context.Context, cfg Config) (*Plugin, error) {
	cfg.PluginName = strings.TrimSpace(cfg.PluginName)
	if cfg.PluginName == "" {
		cfg.PluginName = "wg"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("wg.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("wg.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("wg.New: PluginToken required (plaintext from /admin-plugins.html)")
	}
	if strings.TrimSpace(cfg.UploadDir) == "" {
		return nil, errors.New("wg.New: UploadDir required (wg-svc-local data root)")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_wg: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_wg: %w", err)
	}

	dock := sdk.NewClient(cfg.DockBase, cfg.PluginName, sdk.DeriveHMACKey(cfg.PluginToken))
	resp, err := dock.Do(http.MethodGet, "/internal/v1/ping", nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dock ping: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = db.Close()
		return nil, fmt.Errorf("dock /ping rejected: HTTP %d (check PLUGIN_TOKEN + plugin_modules row)", resp.StatusCode)
	}

	p := &Plugin{
		DB:         db,
		Dock:       dock,
		Name:       cfg.PluginName,
		Listen:     cfg.Listen,
		Ver:        cfg.BuildVersion,
		UploadDir:  cfg.UploadDir,
		MetricsTok: cfg.MetricsToken,
		metrics:    newWGMetrics(),
		startedAt:  time.Now(),
	}
	// Assets migration: ensure wg_bundles.asset_id exists (the table
	// predates the assets catalog). Non-fatal — bundles still serve from
	// local disk if this fails.
	if err := p.ensureBundleAssetColumn(); err != nil {
		log.Printf("wg: ensure asset_id column: %v", err)
	}
	return p, nil
}

// RegisterRoutes attaches the wg plugin's HTTP surface to an existing
// gin engine.
//
// Public /v1/* — token / Bearer auth handled inside each handler.
// Admin /api/admin/wg-* — requireAdminViaDock middleware (Dock auth).
// /healthz + /metrics for ops.
func (p *Plugin) RegisterRoutes(r gin.IRouter) {
	r.GET("/healthz", p.handleHealthz)
	r.GET("/metrics", p.handleMetricsExposition)

	// /v1/* — JOIN_PROTOCOL device-facing surface.
	v1 := r.Group("/v1")
	{
		v1.POST("/register", p.handleWGRegister)
		v1.GET("/peers", p.handleWGPeers)
		v1.GET("/hub/peers", p.handleWGHubPeers)
		v1.POST("/heartbeat", p.handleWGHeartbeat)
		v1.POST("/leave", p.handleWGLeave)
		v1.POST("/token/refresh", p.handleWGTokenRefresh)
		v1.GET("/install", p.handleWGInstallScript)
		v1.GET("/install/:version", p.handleWGInstallScript)
		v1.GET("/bundle", p.handleWGBundleLatest)
		v1.GET("/bundle/:version", p.handleWGBundleDownload)
		v1.GET("/dns/:hub_slug", p.handleWGDNSZone)
	}

	// /internal/v1/* — loopback-only, posted by dock on the same host.
	// No HMAC (nginx doesn't proxy /internal/*; handler re-checks loopback).
	internal := r.Group("/internal/v1")
	{
		internal.POST("/wg-devices/link", p.handleInternalWGDeviceLink)
	}

	// /api/admin/wg-* — admin CRUD, Dock-auth-gated.
	admin := r.Group("/api/admin", p.requireAdminViaDock())
	{
		admin.GET("/wg-hubs", p.handleAdminWGHubList)
		admin.PUT("/wg-hubs/:id", p.handleAdminWGHubUpdate)
		admin.DELETE("/wg-hubs/:id", p.handleAdminWGHubDelete)
		admin.POST("/wg-hubs/:id/reset-bind", p.handleAdminWGHubResetBind)
		admin.GET("/wg-tokens", p.handleAdminWGTokenList)
		admin.POST("/wg-tokens", p.handleAdminWGTokenCreate)
		admin.POST("/wg-tokens/:id/revoke", p.handleAdminWGTokenRevoke)
		admin.GET("/wg-devices", p.handleAdminWGDeviceList)
		admin.DELETE("/wg-devices/:id", p.handleAdminWGDeviceRemove)
		admin.GET("/wg-sites", p.handleAdminWGSiteList)
		admin.GET("/wg-hub-status", p.handleAdminWGHubStatus)
		admin.POST("/wg-bundles/upload", p.handleAdminWGBundleUpload)
		admin.GET("/wg-bundles", p.handleAdminWGBundleList)
		admin.POST("/wg-bundles/:id/set-latest", p.handleAdminWGBundleSetLatest)
		admin.DELETE("/wg-bundles/:id", p.handleAdminWGBundleDelete)
		// Assets migration: upload any local-only bundle blobs into the
		// central assets catalog + record asset_id (idempotent).
		admin.POST("/wg-bundles/backfill-assets", p.handleAdminWGBundleBackfillAssets)
	}
}

// Start kicks off background work: heartbeat to dock + stale-device GC.
// Returns when ctx is cancelled.
func (p *Plugin) Start(ctx context.Context) {
	go p.heartbeatLoop(ctx)
	go p.backfillBundleAssetsOnce() // self-migrate any local-only bundles to assets
	p.startWGStaleDeviceGC(ctx)
	p.startHubStatusPoll(ctx)
	p.startHubLocalSelfPoll(ctx)
}

func (p *Plugin) Close() error {
	if p.DB != nil {
		return p.DB.Close()
	}
	return nil
}

func (p *Plugin) handleHealthz(c *gin.Context) {
	dbOK := true
	if err := p.DB.PingContext(c.Request.Context()); err != nil {
		dbOK = false
	}
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{
		"plugin":         p.Name,
		"version":        p.Ver,
		"uptime_seconds": int64(time.Since(p.startedAt).Seconds()),
		"db_ok":          dbOK,
		"go":             runtime.Version(),
	})
}

// handleMetricsExposition is the Prometheus scrape endpoint. Same
// token-gate pattern as dock: empty MetricsTok = 404 the endpoint
// entirely (refuse to expose internals); set the env var to opt in.
func (p *Plugin) handleMetricsExposition(c *gin.Context) {
	if p.MetricsTok == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if c.GetHeader("Authorization") != "Bearer "+p.MetricsTok {
		c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	promhttp.HandlerFor(p.metrics.registry, promhttp.HandlerOpts{}).ServeHTTP(c.Writer, c.Request)
}

// heartbeatLoop pings dock once at startup, then every 60s. Failures
// log + continue; dock's plugin GC tolerates one missed beat.
func (p *Plugin) heartbeatLoop(ctx context.Context) {
	p.beat(ctx)
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.beat(ctx)
		}
	}
}

// wgUIRoutes — sidebar entries this plugin contributes.
var wgUIRoutes = []sdk.UIRoute{
	{Path: "/wg-tokens.html", Label: "WG Mesh", Icon: "network", AdminOnly: true, Order: 75},
}

func (p *Plugin) beat(_ context.Context) {
	err := p.Dock.Heartbeat(sdk.HeartbeatOpts{
		Version:       p.Ver,
		Endpoint:      p.Listen,
		UptimeSeconds: int64(time.Since(p.startedAt).Seconds()),
		UIRoutes:      wgUIRoutes,
	})
	if err != nil {
		log.Printf("wg: heartbeat failed: %v", err)
	}
}

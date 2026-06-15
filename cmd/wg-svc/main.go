// Command wg-svc is the wg-mac control plane plugin binary. Phase 1-C
// ships the skeleton — DB pool + dock heartbeat + /healthz. Phase 1-D
// adds the actual wg admin/peer/token endpoints.
//
// Env vars:
//
//	POLAR_WG_DB_DSN       postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg?sslmode=disable
//	POLAR_DOCK_BASE       http://127.0.0.1:8080
//	POLAR_PLUGIN_NAME     wg                 (matches plugin_modules row)
//	POLAR_PLUGIN_TOKEN    polar_plugin_…     (plaintext from /admin-plugins.html)
//	POLAR_WG_LISTEN       127.0.0.1:8090     (HTTP listen addr)
//	POLAR_WG_VERSION      git-sha or "0.0.1" (cosmetic; appears in /admin-plugins.html)
//	POLAR_WG_METRICS_TOKEN bearer token for /metrics; unset = endpoint 404
//
// The plaintext PLUGIN_TOKEN is stored ONLY in the plugin's env (chmod
// 600 .env file recommended). Dock never sees it after creation —
// only the sha256 hash in plugin_modules.plugin_key_hash.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"

	"github.com/networkextension/polar-wg/internal/wg"
)

func main() {
	cfg := wg.Config{
		DBDSN:        envOrDefault("POLAR_WG_DB_DSN", "postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg?sslmode=disable"),
		DockBase:     envOrDefault("POLAR_DOCK_BASE", "http://127.0.0.1:8080"),
		PluginName:   envOrDefault("POLAR_PLUGIN_NAME", "wg"),
		PluginToken:  os.Getenv("POLAR_PLUGIN_TOKEN"),
		Listen:       envOrDefault("POLAR_WG_LISTEN", "127.0.0.1:8090"),
		BuildVersion: envOrDefault("POLAR_WG_VERSION", "0.0.1"),
		MetricsToken: os.Getenv("POLAR_WG_METRICS_TOKEN"),
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		log.Fatal("POLAR_PLUGIN_TOKEN unset — get plaintext from /admin-plugins.html (one-time print at row creation)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plugin, err := wg.New(ctx, cfg)
	if err != nil {
		log.Fatalf("wg.New: %v", err)
	}
	defer plugin.Close()

	gin.SetMode(envOrDefault("GIN_MODE", gin.ReleaseMode))
	r := gin.New()
	r.Use(gin.Recovery())
	plugin.RegisterRoutes(r)
	plugin.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("wg-svc listening on %s (dock=%s, name=%s, ver=%s)",
			cfg.Listen, cfg.DockBase, cfg.PluginName, cfg.BuildVersion)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("wg-svc: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("wg-svc: shutdown: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

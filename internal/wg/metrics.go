package wg

// metrics.go — wg-svc's own Prometheus registry. Mirrors dock's
// `internal/app/dock/metrics.go` wg-prefixed metrics 1:1 so existing
// dashboards / alert rules keep working after the cutover.

import (
	"github.com/prometheus/client_golang/prometheus"
)

type wgMetrics struct {
	registry *prometheus.Registry

	devicesAlive       *prometheus.GaugeVec  // per hub slug
	hubsBound          prometheus.Gauge      // count of hubs with bound_device_id != NULL
	registerTotal      *prometheus.CounterVec // role x result
	heartbeatTotal     *prometheus.CounterVec // hub slug x result
	staleGCTotal       *prometheus.CounterVec // result (ok/error)
	staleGCRemoved       prometheus.Counter
	wgTokensExpiringSoon *prometheus.GaugeVec // labels: within (24h / 7d / 30d)
}

func newWGMetrics() *wgMetrics {
	m := &wgMetrics{registry: prometheus.NewRegistry()}
	m.devicesAlive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "polar_wg_devices_alive", Help: "Live devices per hub (removed_at IS NULL)."},
		[]string{"hub_slug"},
	)
	m.hubsBound = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "polar_wg_hubs_bound", Help: "Hubs that have a bound device."},
	)
	m.registerTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "polar_wg_register_total", Help: "/v1/register outcomes."},
		[]string{"role", "result"},
	)
	m.heartbeatTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "polar_wg_heartbeat_total", Help: "/v1/heartbeat outcomes per hub."},
		[]string{"hub_slug", "result"},
	)
	m.staleGCTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "polar_wg_stale_gc_total", Help: "Stale-device GC runs."},
		[]string{"result"},
	)
	m.staleGCRemoved = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "polar_wg_stale_gc_removed_total", Help: "Devices marked removed by GC (cumulative)."},
	)
	m.wgTokensExpiringSoon = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "polar_wg_tokens_expiring_soon", Help: "Unconsumed tokens expiring within the labelled window."},
		[]string{"within"},
	)
	m.registry.MustRegister(
		m.devicesAlive, m.hubsBound, m.registerTotal, m.heartbeatTotal,
		m.staleGCTotal, m.staleGCRemoved, m.wgTokensExpiringSoon,
	)
	return m
}

func labelOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func (m *wgMetrics) recordWGRegister(role, result string) {
	if m == nil {
		return
	}
	m.registerTotal.WithLabelValues(labelOr(role, "unknown"), labelOr(result, "unknown")).Inc()
}

func (m *wgMetrics) recordWGHeartbeat(hubSlug, result string) {
	if m == nil {
		return
	}
	m.heartbeatTotal.WithLabelValues(labelOr(hubSlug, "unknown"), labelOr(result, "unknown")).Inc()
}

func (m *wgMetrics) recordWGStaleGC(result string, removed int) {
	if m == nil {
		return
	}
	m.staleGCTotal.WithLabelValues(labelOr(result, "unknown")).Inc()
	if removed > 0 {
		m.staleGCRemoved.Add(float64(removed))
	}
}

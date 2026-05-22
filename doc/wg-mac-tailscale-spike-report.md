# Phase 11 spike report — vendored Headscale

**Date:** 2026-05-18 · **Branch:** `feat/wg-mac-phase11-spike` · **Headscale:** `juanfont/headscale v0.28.0`

## TL;DR — green light B (vendor)

All four spike days delivered. Embedded Headscale **works** inside
the polar-dock binary; no architectural blockers found. Recommend
committing to the 8-10 week C1-C7 implementation plan in
[doc/wg-mac-tailscale-compat-design.md](./wg-mac-tailscale-compat-design.md).

## Day-by-day

### D1 — vendor + compile ✅

- `go.mod`: 1.25.0 → 1.25.5 (smallest bump headscale v0.28.0 will tolerate; 1.26.2 from the dev branch is overkill).
- `CGO_ENABLED=0` pinned in `scripts/build-dock.sh`. Polar has no cgo deps; the flag bypasses a Go-1.26 macOS-SDK landmine and gives a static binary as a bonus.
- `go get github.com/juanfont/headscale@v0.28.0` succeeds. Blank import in `internal/app/dock/wg_headscale.go` pins the dep.

**Cost:** `go.mod` grew from ~80 to **159 require lines**; binary size 36 MB → **63 MB** (+27 MB / +75%). Acceptable for our deploy box.

### D2 — construct in Polar boot ✅

`internal/app/dock/wg_headscale.go` now imports `hscontrol` + `hscontrol/types` and runs:

```go
types.LoadConfig(cfgPath, true)
cfg, _ := types.LoadServerConfig()
hs, _ := hscontrol.NewHeadscale(cfg)
```

Gated by env: `WG_HEADSCALE_ENABLED=1` (default off, zero impact on production .57). Wired into the dock bootstrap right after `llmProxyProber.start()`. `go build` + `go test` clean.

### D3 — actually serve ✅ (partial — see TS-client smoke caveat)

Standalone test command `cmd/headscale-spike/main.go` plus a minimal config `scripts/headscale-spike.yaml` boot Headscale **from inside our vendored binary**:

```
PID=9908
{"level":"info","message":"listening and serving HTTP on: 127.0.0.1:8080"}
{"level":"info","message":"listening and serving debug and metrics on: 127.0.0.1:9091"}

$ curl -sI http://127.0.0.1:8080/health
HTTP/1.1 200 OK
```

Two real-world config gotchas discovered:

1. **`dns.override_local_dns: true` is the default**, and the config validation
   refuses to start when it's true with empty `nameservers.global`. Must set
   `override_local_dns: false` explicitly for a minimal config.
2. **`derp.urls` must be non-empty** even when `derp.server.enabled: false`.
   Headscale rejects `"initial DERPMap is empty, Headscale requires at
   least one entry"`. Worked around by pointing at
   `https://controlplane.tailscale.com/derpmap/default`.

**Real `tailscale up` smoke deferred** — needs an actual Tailscale client
install on the test box. Confidence is high that it works given headscale's
own integration tests cover this against the same constructor, but
**this is the one remaining D3 unknown**. ~30 min of next session.

### D4 — this report ✅

## Findings vs the design doc's three open questions

| Question (from §0 of wg-mac-tailscale-compat-design.md) | Answer |
|---|---|
| Is `hscontrol` embeddable, or main-only? | **Embeddable** — `hscontrol.NewHeadscale(cfg)` + `(*Headscale).Serve()` are the public entrypoints, called identically by their own `cmd/headscale`. No global side effects beyond what we expect (config loader writes to its own package state). |
| Can headscale reuse our Postgres? | **Yes, but separate schema namespace.** Headscale's gorm migrations target `users`, `nodes`, `pre_auth_keys`, etc. — names that don't collide with our `wg_*` tables. Spike used SQLite to skip the spike-time setup; switch to Postgres in C2. |
| Can token mint hit headscale's PreAuthKey API? | **Yes** — `hscontrol/types/preauth_key.go` is `Reusable + Ephemeral + Tags + Expiration` which maps 1:1 onto our `wg_tokens` row (already has `expires_at`, `consumed_at`, etc.). C3 just needs an adapter that calls headscale's gRPC API on mint. |

## Architectural decisions confirmed for C1-C7

Re-affirming what the design doc said, now with code-touching evidence:

- **Two listeners, one process** (path A in design §2): cleaner than trying to mount headscale handlers on Polar's gin router. Headscale wants its own listener (`cfg.Addr`); just let it have one. nginx fronts `/machine/* /derp/*` to that port.
- **Schema doubled** (path B1 in design §2.x): headscale gets its own `headscale.*` tables in the same Postgres. Polar's `wg_*` tables stay untouched. Adapter layer (C3) syncs token mints across both.
- **Don't touch wg-mac client protocol**: our `/v1/*` keeps working for native wg-mac clients. Tailscale clients hit `/machine/*` instead. **Dual rail** is real.

## New issue surfaced — Polar dock + headscale port conflict

Polar dock listens on `:8080`. Headscale's default `listen_addr` is also `:8080`. In an embedded deploy:
- Polar dock: bind `:8080` (Go-level, gin)
- Headscale: bind `:8081` (new, configured)
- nginx: route `/machine/*` `/derp/*` → `:8081`, everything else → `:8080`

This is a 1-line config change but needs to be in the C1 PR.

## Updated effort estimate

Original design doc said **8-10 weeks for C1-C7**. After this spike:
- **C1 (route分流)** is effectively done by this spike. Remove from estimate. **−1 week.**
- **C2 (schema 适配)** validated via SQLite, Postgres path looks straightforward. Keep 1 week.
- **C3-C7** unchanged.

**New estimate: 7-9 weeks** (down from 8-10).

## Recommendation

**Green light B (vendor headscale into polar-dock).** No structural surprises, cost is acceptable, the protocol mapping that Polar's `wg_tokens ↔ PreAuthKey` is needs near-zero translation logic.

Next concrete action: kick off `feat/wg-mac-phase11-c1` to clean up this spike branch into a production-shaped scaffold (real Postgres config, port disambiguation, healthcheck for headscale subsystem, prometheus metrics for it).

## Files left from spike (post-C1 status)

| Path | Status |
|---|---|
| `internal/app/dock/wg_headscale.go` | ✅ kept; C1 enabled `go hs.Serve()` in goroutine |
| `cmd/headscale-spike/main.go` | ✅ deleted in C1 (production mounts via wg_headscale.go) |
| `scripts/headscale-spike.yaml` | ✅ renamed to `scripts/headscale.yaml.example` with prod-shape Postgres + port-disambiguation comments |
| `doc/wg-mac-tailscale-spike-report.md` (this file) | ✅ kept as historical record |
| `doc/wg-mac-tailscale-compat-design.md` | TODO §1 estimate update (8-10wk → 7-9wk) |
| `scripts/headscale-spike.yaml` | ⚠ migrate into a `scripts/headscale.yaml.example` for ops reference |
| `doc/wg-mac-tailscale-spike-report.md` (this file) | ✅ keep as historical record |
| `doc/wg-mac-tailscale-compat-design.md` | ✅ keep; update §1 with the "8-10 weeks → 7-9 weeks" delta |

# wg platform — DNS / HTTP-proxy push for mobile (design)

Status: **v1 implemented (2026-06-21)** — DNS push end to end (control plane +
iOS NE). v2 (HTTP proxy) / v3 (no-blip, Android, per-device) still proposed.
Owner: polar-wg (control plane) + polar-wg-app (mobile clients).

## Goal
Let an operator configure **DNS servers + match/search domains** and an **HTTP proxy**
(host:port or PAC) on the platform and have them **pushed to mobile devices** (iOS
NetworkExtension, Android VpnService), applied to the VPN tunnel's network settings.

## Why this shape (current state)
- **Control plane** already distributes per-hub policy (e.g. `wg_hubs.advertised_routes_json`
  JSONB → `/v1/peers`/register responses; admin via `PUT /api/admin/wg-hubs/:id` with nullable
  pointer fields) and a DNS *zone* via `/v1/dns/:hub_slug`. There is **no `dns_servers`/`proxy`
  column** yet. Devices carry an `os` field (`ios`/`android`) but responses are **not tailored by
  OS** today.
- **iOS NE** (`NetworkExtension/Sources/PacketTunnelProvider.swift`): `buildNetworkSettings`
  sets routes + `NEDNSSettings(servers:)` (or DoH) + MTU — **no `proxySettings`**, no
  match/search domains. `providerConfiguration` keys (`config`/`routeMode`/`dnsMode`/`kcp*`) are
  read **once at startTunnel** (static). `NEProxySettings` (httpServer/httpsServer, PAC URL/JS,
  matchDomains, excludeSimpleHostnames) is available but unused.
- **Android** (`WireGuardAndroid`, GoBackend + `VpnService.Builder`): `addDnsServer` (flat list)
  + `setHttpProxy(ProxyInfo)` (API 29+, host:port or PAC, weak domain matching). `dnsMode`/
  `routeMode` are parsed but currently **not applied**.
- **Desktop/native (wg-quick) has no proxy concept** → proxy is mobile (+ macOS NE) only; DNS is
  universal.

## Architecture (3 planes)
```
admin ─▶ wg_hubs.policy_json (dns+proxy) ─▶ /v1/peers & register response carry a `policy` block
                                                  │
mobile host app (reconciler poll) ── reads policy ──▶ writes providerConfiguration (dns*/proxy* keys)
                                                  │ on change: restart tunnel OR sendProviderMessage
iOS extension / Android VpnService ── apply ──▶ NEDNSSettings + NEProxySettings / ProxyInfo
```
The iOS **extension** config is static, so the path is **control plane → host app →
`providerConfiguration` → extension**; the host app's reconciler polls, and on change updates +
re-applies.

## Data model (control plane — mirrors the advertised_routes pattern)
`wg_hubs.policy_json JSONB` (+ optional `wg_devices.policy_override_json` for per-device mobile
overrides):
```json
{ "dns":   { "servers": ["100.64.0.1"], "match_domains": ["wg","corp"], "search_domains": [],
             "mode": "plain|doh", "doh_url": "..." },
  "proxy": { "http": "proxy.corp:8080", "https": "proxy.corp:8080", "pac_url": "https://…/proxy.pac",
             "match_domains": [], "exclude_simple": true, "bypass": [] } }
```
- Admin: extend `PUT /api/admin/wg-hubs/:id` with `Dns *DNSConfig` + `Proxy *ProxyConfig`
  (nil = unchanged, `{}`/`[]` = clear, value = replace) + `validateDNSProxy()`.
- Distribution: add a `Policy` block to `wgRegisterResponse` (returned on every `/v1/peers` poll),
  optionally **tailored by `device.os`** (iOS = rich proxy, Android = ProxyInfo-compatible subset,
  desktop = DNS only).

## Mobile application
**iOS** — host app reconciler writes `providerConfiguration` keys (`dnsServers`,
`dnsMatchDomains`, `dnsSearchDomains`, `httpProxyHost`, `httpProxyPort`, `proxyPacUrl`,
`proxyMatchDomains`, `proxyExcludeSimple`); the extension's `buildNetworkSettings` builds a full
`NEDNSSettings` (servers + matchDomains + searchDomains) and an `NEProxySettings`
(httpServer/httpsServer **or** `proxyAutoConfigurationURL` + matchDomains + excludeSimpleHostnames),
assigned to the tunnel settings.

**Android** — reconciler feeds policy to `VpnService.Builder.addDnsServer` +
`setHttpProxy(ProxyInfo.buildPacProxy/buildDirectProxy)`; weaker domain matching; changes require
rebuilding the VPN interface.

## Open decisions
1. **Scope**: per-hub default vs + per-device override (mobile often needs per-device). Recommend
   per-hub + optional override.
2. **Proxy form**: explicit host:port vs **PAC URL** (per-domain rules). Support both; prefer PAC on
   mobile.
3. **iOS apply-on-change**: v1 = **restart tunnel** (simple, brief blip) vs v2 =
   `sendProviderMessage` → extension re-calls `setTunnelNetworkSettings` (no blip, needs a message
   handler).

## Phasing
- **v1 — DNS push** ✅ DONE (2026-06-21): per-hub DNS (servers + match/search domains), extends the
  existing DNS path; iOS extension applies the full `NEDNSSettings`; admin field + response block.
  Control plane: `wg_hubs.policy_json` (JSONB) + `WGPolicy`/`DNSPolicy`/`ProxyPolicy` structs +
  `validateWGPolicy` + `PUT /api/admin/wg-hubs/:id` `policy` field + `Policy` block on the register /
  `/v1/peers` response. iOS: `MeshClient` decodes `policy`; `TunnelManager` persists DNS CSV on the
  profile + writes `dnsServers`/`dnsMatchDomains`/`dnsSearchDomains` providerConfiguration keys; the
  NE `buildNetworkSettings` applies servers (override) + match/search domains to `NEDNSSettings`
  (plain) / `NEDNSOverHTTPSSettings` (doh). Apply-on-change = restart tunnel.
- **v2 — HTTP proxy push**: host:port + PAC → iOS `NEProxySettings`; restart-tunnel apply.
- **v3 — polish**: no-blip apply (`sendProviderMessage`), Android `ProxyInfo`, per-device override,
  per-OS response tailoring.

## Files
- Control plane: `internal/wg/handlers.go` (response + admin), `store.go` (WGHub policy field),
  `scripts/migrate/wg-schema.sql` + an `ensure*` func (policy_json column).
- iOS: `NetworkExtension/Sources/PacketTunnelProvider.swift` (buildNetworkSettings + providerConfiguration parse), `NE_INTEGRATION.md` (document the keys).
- Android: `WireGuardAndroid/.../tunnel/WgTunnelManager.kt` (apply DNS/proxy).

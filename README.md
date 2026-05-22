# polar-wg

WireGuard mesh control plane for the [Polar](https://github.com/networkextension/Polar) platform.

Issues tokens, registers wg-mac client devices, allocates `100.64.0.0/10` mesh IPs per role (hub/device), pushes per-peer config, supports multi-hub topology, MagicDNS, NAT traversal, Windows + macOS clients, and Tailscale compat via embedded Headscale.

## Status

Phase 3b shipped on the `zen.4950.store` deploy box. Polar dock has already cut over (POLAR_WG_REMOTE=true); this binary is canonical.

## Install

Standalone deploy on macOS (launchd):

```bash
# build for the target box
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/wg-svc ./cmd/wg-svc

# rsync to box
rsync -avz /tmp/wg-svc local@<deploy-box>:/Users/local/.local/bin/

# on the box:
cp scripts/launchd/wg-svc.env.sample ~/wg-svc.env  # edit secrets
chmod 600 ~/wg-svc.env
cp scripts/launchd/polar.wg-svc.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/polar.wg-svc.plist
```

Environment (see `scripts/launchd/wg-svc.env.sample`):
- `POLAR_DOCK_URL` — dock base URL (e.g. `http://127.0.0.1:8080`)
- `POLAR_PLUGIN_TOKEN` — issued by dock admin at `/admin-plugins.html`
- `POLAR_WG_DB_DSN` — Postgres DSN for `polar_wg` DB
- `POLAR_WG_LISTEN` — bind addr (default `127.0.0.1:8090`)
- `POLAR_WG_BLOB_DIR` — bundle storage path

## Architecture

- HMAC-signed plugin → dock auth via `github.com/networkextension/polar-sdk`
- Independent Postgres DB (`polar_wg`); schema in `scripts/migrate/wg-schema.sql`
- Per-hub topology with role-aware allocator (`internal/wg/alloc.go`)
- Embedded Headscale for Tailscale-compatible clients (opt-in via env)
- Prometheus `/metrics` (bearer-token gated)

## Related

- [Polar dock](https://github.com/networkextension/Polar)
- [polar-sdk](https://github.com/networkextension/polar-sdk)
- [doc/wg-mac-api.md](doc/wg-mac-api.md) — REST API spec
- [doc/wg-svc-extraction.md](doc/wg-svc-extraction.md) — Phase 1-C/D extraction story

## License

MIT

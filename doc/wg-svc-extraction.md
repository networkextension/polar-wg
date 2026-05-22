# wg-svc extraction runbook

Phase 1-C ships the skeleton (this PR). Phase 1-D performs the
actual handler migration + traffic cutover. This doc is the
operator + developer reference for both.

## What Phase 1-C ships

| Path | Purpose |
|---|---|
| `cmd/wg-svc/main.go` | Standalone binary; opens polar_wg + dock SDK + serves `/healthz`. |
| `internal/plugins/wg/plugin.go` | `Plugin` struct: DB pool, heartbeat loop, `/healthz` handler. |
| `internal/plugins/sdk/client.go` | In-tree HMAC client (will become `polar-plugin-sdk-go` in Phase 3). |
| `scripts/migrate/wg-schema.sql` | Polar_wg end-state schema (Phase 2 shape). |
| `scripts/migrate/wg-data.sh` | Idempotent dump + load of wg_* rows from `ideamesh` → `polar_wg`. |

Dock is unchanged. Dock still serves `/api/admin/wg-*` and `/v1/{register,peers,heartbeat,leave,bundle,install}`. wg-svc only proves the architecture: it talks to dock, dock recognises it (`/admin-plugins.html` shows a fresh heartbeat), and it owns its own DB.

## What Phase 1-D ships

1. **Move handlers** from `internal/app/dock/wg_*.go` into `internal/plugins/wg/`. They become methods on `Plugin` (which carries DB + Dock SDK) instead of `*Server`.
2. **Replace cross-domain lookups** with SDK calls:
   - `wg_tokens.created_by_user_id` rendered as a username → `dock.UserGet`
   - `wg_bundles.added_by_user_id` → same
   - Admin auth on `/api/admin/wg-*` → `dock.AuthVerify(token)` + role check
3. **Cutover**:
   - `nginx`: add `location /api/wg/ { proxy_pass http://127.0.0.1:8090; }` + `location ~ ^/v1/(register|peers|heartbeat|leave|bundle|install) { proxy_pass http://127.0.0.1:8090; }`
   - Set `POLAR_WG_REMOTE=true` in dock env — dock returns 404 from its wg routes, telling nginx "use the upstream" (the route in nginx is registered *before* the dock-fallback, so the flag is belt-and-braces).
4. **One release of bake time**, then delete `internal/app/dock/wg_*.go` + drop the `wg_*` tables in `ideamesh` (after a `pg_dump` backup).

## Apply Phase 1-C on the deploy box

```bash
# 0. ssh in
ssh -p 5722 local@127.0.0.1

# 1. create the database (one-time, as the cluster superuser)
PSQL='/Applications/Postgres.app/Contents/Versions/latest/bin/psql'
$PSQL -d postgres -c "CREATE DATABASE polar_wg OWNER ideamesh;"

# 2. apply schema
cd ~/github/Polar-
$PSQL "postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg" \
    -f scripts/migrate/wg-schema.sql

# 3. dry-run the data migration (read-only, shows source counts)
./scripts/migrate/wg-data.sh

# 4. perform the migration (truncates target, loads from source)
./scripts/migrate/wg-data.sh --apply

# 5. provision a plugin row via the admin UI
#    https://dev.4950.store/admin-plugins.html → "New plugin"
#    name: "wg"  display: "WireGuard mesh"  endpoint: "http://127.0.0.1:8090"
#    capture the plaintext token (45 chars, polar_plugin_…) — shown once.

# 6. cross-build wg-svc on the dev box, rsync to deploy
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/wg-svc ./cmd/wg-svc
rsync -avz -e 'ssh -p 5722' /tmp/wg-svc local@127.0.0.1:/Users/local/.local/bin/wg-svc

# 7. start wg-svc (env-file, chmod 600)
cat > ~/wg-svc.env <<EOF
POLAR_WG_DB_DSN=postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg?sslmode=disable
POLAR_DOCK_BASE=http://127.0.0.1:8080
POLAR_PLUGIN_NAME=wg
POLAR_PLUGIN_TOKEN=polar_plugin_…  # from step 5
POLAR_WG_LISTEN=127.0.0.1:8090
POLAR_WG_VERSION=$(git rev-parse --short HEAD)
EOF
chmod 600 ~/wg-svc.env
env $(cat ~/wg-svc.env | xargs) /Users/local/.local/bin/wg-svc &

# 8. smoke
curl -s http://127.0.0.1:8090/healthz | jq
# expect: {"plugin":"wg","db_ok":true,...}

# 9. confirm dock sees the heartbeat
#    open /admin-plugins.html — the wg row should show last_heartbeat
#    within the last minute. Expect green/fresh status.
```

If `/healthz` returns `db_ok:false` → wg-svc can reach polar_wg but a query failed. Check `psql polar_wg` works as the same user.

If wg-svc exits at startup with `dock /ping rejected: HTTP 401` → the HMAC key derivation is off. Most likely the operator pasted the wrong column — `PLUGIN_TOKEN` is the *plaintext* shown once at provisioning, not the row's `plugin_key_hash`. Re-rotate via `/admin-plugins.html` → "Rotate key".

## Phase 1-D-1 — shipped

Handlers moved into `internal/plugins/wg/` (PR #282). Dock unchanged; both processes register the same routes, but only dock receives traffic (nginx hasn't moved yet).

## Phase 1-D-2 — cutover (this PR)

| Change | Where |
|---|---|
| Skip wg route registration when `POLAR_WG_REMOTE=true` | `internal/app/dock/app.go` |
| Wire `POLAR_WG_REMOTE` env from main → Config.WGRemote | `cmd/dock/main.go` |
| Nginx location snippet for `/v1/*` + `/api/admin/wg-*` → `:8090` | `scripts/nginx/wg-svc-snippet.conf` |
| launchd plist + wrapper + env-sample + setup script | `scripts/launchd/polar.wg-svc.plist`, `polar-wg-svc-launch.sh`, `wg-svc.env.sample`, `setup-wg-svc.sh` |

### Cutover runbook (apply on .57 after merge)

```bash
# 0. Pre-flight: confirm wg-svc Phase 1-D-1 is healthy.
ssh -p 5722 local@127.0.0.1 'curl -s http://127.0.0.1:8090/healthz'
# expect: {"db_ok":true,...}

# 1. Install wg-svc launchd plist (idempotent). First run bootstraps
#    the env file from sample; subsequent runs leave it alone.
ssh -p 5722 local@127.0.0.1 'cd ~/github/Polar- && bash scripts/launchd/setup-wg-svc.sh'

# 2. Drop the nginx snippet + include from the vhost.
ssh -p 5722 local@127.0.0.1 'sudo install -m 0644 \
    ~/github/Polar-/scripts/nginx/wg-svc-snippet.conf \
    /opt/homebrew/etc/nginx/snippets/wg-svc.conf'
# Add `include /opt/homebrew/etc/nginx/snippets/wg-svc.conf;` to the
# vhost above the general /api + /v1 blocks. Then:
ssh -p 5722 local@127.0.0.1 'sudo nginx -t && sudo brew services restart nginx'

# 3. Flip dock — add POLAR_WG_REMOTE=true to ~/polar-dock.env, kickstart.
ssh -p 5722 local@127.0.0.1 'echo "POLAR_WG_REMOTE=true" >> ~/polar-dock.env && \
    launchctl kickstart -k gui/$(id -u)/polar.dock'

# 4. Smoke. /v1/peers + /api/admin/wg-hubs should now hit wg-svc through
#    nginx; dock should 404 those paths if you bypass nginx.
ssh -p 5722 local@127.0.0.1 'bash -s' <<'EOF'
echo "--- via nginx (should hit wg-svc) ---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" -k https://localhost:8443/v1/peers
echo "--- via dock direct (should 404 — flag works) ---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" http://127.0.0.1:8080/v1/peers
echo "--- wg-svc direct ---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" http://127.0.0.1:8090/v1/peers
EOF
```

### Cutover DoD

- ✓ wg-svc launchd plist loaded, survives `launchctl unload polar.wg-svc && launchctl load polar.wg-svc`
- ✓ wg-svc auto-starts on box reboot
- ✓ `/v1/*` via nginx returns wg-svc responses
- ✓ `/api/admin/wg-hubs` via nginx returns wg-svc responses (admin UI still works)
- ✓ `/v1/peers` direct to `:8080` returns dock 404 (flag honored)
- ✓ Dock's plugin GC + everything-else still works
- ✓ One existing wg device successfully heartbeats through the new path

### Headscale temporary degradation

Phase 1-D-1's wg-svc `/api/admin/wg-tokens` does **not** issue a Tailscale auth key alongside the wg-mac token (the embedded Headscale process stays in dock). After cutover the UI will mint wg-mac tokens but `tailscale_authkey` is missing from the response — operators who need a Headscale PreAuthKey can:

- Mint manually via `headscale preauthkeys create` (CLI on dock host), or
- Wait for Phase 1-D-3 which restores it via `/internal/v1/headscale/mint-authkey`.

This is acceptable for a one-release bake window.

## Rollback

If wg-svc misbehaves after cutover:

1. Remove the `include /opt/homebrew/etc/nginx/snippets/wg-svc.conf;` from the vhost, `sudo nginx -s reload`.
2. Remove `POLAR_WG_REMOTE=true` from `~/polar-dock.env`, `launchctl kickstart -k gui/$(id -u)/polar.dock`.
3. Traffic is back on dock's in-tree path within ~5s. dock's `ideamesh.wg_*` tables are still authoritative — polar_wg may have rows ahead but those can be re-imported manually (or accepted as lost) since the cutover window is short.

The Phase 1-D-1 wg code in dock stays intact through this whole window. Only after one full release without rollback does Phase 1-D-3 delete it.

## Phase 1-D-3 — final cleanup (do not start until bake passes)

- [ ] PR: delete `internal/app/dock/wg_*.go` + remove `WGRemote` flag (dock never serves wg again)
- [ ] PR: add `/internal/v1/headscale/mint-authkey` to dock + wire wg-svc's token-create back to it (restore Tailscale key)
- [ ] One-shot SQL: backup + `DROP TABLE wg_heartbeats, wg_devices, wg_bundles, wg_tokens, wg_sites, wg_hubs;` in `ideamesh`

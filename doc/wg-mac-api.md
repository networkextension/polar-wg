# wg-mac public API reference

Phase 2 — multi-hub control plane served by Polar dock at the deploy
host. All `/v1/*` endpoints are over HTTPS, JSON request/response,
no cookies. Authentication varies per endpoint:

- `POST /v1/register` — bearer-style join token (one-time) in the body.
- `POST /v1/heartbeat`, `POST /v1/leave`, `POST /v1/token/refresh`,
  `GET /v1/peers`, `GET /v1/hub/peers` — **device token** issued by
  `/v1/register`. Carried via:
  - `Authorization: Bearer <token>` header
  - `X-Device-Id: <dev_…>` header (the `device_id` from register)
- `GET /v1/install`, `GET /v1/install/:version`, `GET /v1/bundle`,
  `GET /v1/bundle/:version` — unauthenticated.

Base URL (production): `https://zen.4950.store`

Common errors:
- `401` — auth missing/wrong/expired (token revoked, bearer + X-Device-Id mismatch, etc.)
- `400` — invalid JSON body
- `500` — server error (DB down, etc.)

---

## 1. `POST /v1/register`

Bootstrap a new device into a mesh. Token role determines whether
this device is the **hub** (first machine of a mesh) or a regular
**device** (joins an already-online hub). The admin chose the role
when minting the token.

### Request

```http
POST /v1/register HTTP/1.1
Content-Type: application/json
```

```json
{
  "token":     "polar_wg_…",            // required
  "pubkey":    "TqbeoU9mc…=",           // required, wg public key (base64)
  "hostname":  "yarshure-mac",          // optional, freeform display
  "os":        "darwin",                // optional
  "arch":      "arm64",                 // optional
  "agent_ver": "wg-mac-20260517-e360fd07",  // optional
  "lan_addrs": [                        // optional, used for site detection
    { "iface": "en0",  "cidr": "192.168.11.79/24" },
    { "iface": "en10", "cidr": "10.0.0.42/24" }
  ],
  "wg_listen": 51820,                   // optional, default 51820
  "site_slug": ""                       // optional, override auto site
}
```

### Response 200

```json
{
  "device_id":     "dev_a8f3…",         // opaque; pass back on every subsequent call
  "device_ip":     "10.88.0.5",         // your wg IP (write into [Interface].Address)
  "site_id":       "pudong:site_1",     // site you landed in (slug)
  "hub_slug":      "pudong",
  "role":          "device",            // "hub" | "device" — branch on this
  "mesh_cidr":     "10.88.0.0/24",
  "hub": {
    "slug":     "pudong",
    "pubkey":   "DwyGEhX…=",
    "endpoint": "zen.4950.store:51820",
    "wg_ip":    "10.88.0.1"
  },
  "peers": [
    {                                   // LAN-direct peer in same site
      "pubkey":   "1isIQrxH…=",
      "wg_ip":    "10.88.0.3",
      "endpoint": "192.168.11.79:51820",
      "site_id":  "pudong:site_1",
      "hostname": "yarshure-dev"
    },
    {                                   // hub (cross-site forwarder)
      "pubkey":   "DwyGEhX…=",
      "wg_ip":    "10.88.0.1",
      "endpoint": "zen.4950.store:51820",
      "site_id":  "hub",
      "allowed_extra": [ "10.88.0.0/24" ]   // CIDRs to route via hub
    }
  ],
  "keepalive_sec": 25,
  "refresh_sec":   300,                 // poll /v1/peers every N seconds
  "token":         "polar_wg_…",        // current device token (echoed)
  "token_expires": "2026-08-17T00:00:00Z"   // null if non-expiring
}
```

**Client-side branching by `role`:**

- `role == "hub"`:
  - `device_ip` is `<mesh_cidr>.1` (the hub IP).
  - Write conf with `Address = <device_ip>/24` (or wider — matches mesh).
  - Enable IP forwarding: `sysctl -w net.inet.ip.forwarding=1` (macOS).
  - Use `/v1/hub/peers` (not `/v1/peers`) to refresh the hub's `[Peer]` list.
- `role == "device"` (default):
  - Write conf with `Address = <device_ip>/32`.
  - Render peers per §5 of `JOIN_PROTOCOL.md` (hub + LAN-direct).
  - Use `/v1/peers` to refresh.

### Errors

| Status | Body `error` | Meaning |
|---|---|---|
| 400 | `invalid_input` | malformed JSON or missing `token`/`pubkey` |
| 401 | `invalid_token` | unknown/expired/revoked token, or token has no `hub_id` (was minted before Phase 2) |
| 409 | `token_already_bound` | token already consumed by a different device |
| 409 | `pubkey_already_registered` | same `pubkey` registered with a different token |
| 409 | `hub_already_bound` | role=hub token whose target hub is already claimed; admin must "reset bind" first |
| 424 | `hub_not_configured` | role=device token whose target hub hasn't been brought online yet (with `hint`) |
| 507 | site exhausted | the device's site has no free `d_index` in `[2,254]` |

**Idempotent re-register:** running install.sh again with the *same
token* + *same pubkey* returns the existing row (no re-allocation,
just refreshes hostname/agent_ver/lan_addrs). Safe to retry.

---

## 2. `GET /v1/peers` — device refresh

Polled every `refresh_sec` (default 300s) to pick up peer changes
(new joins, IPs rotating, devices leaving).

### Request

```http
GET /v1/peers HTTP/1.1
Authorization: Bearer <device-token>
X-Device-Id:   <dev_id>
```

### Response 200

Same shape as `POST /v1/register` (no `token` in the response).

```json
{
  "device_id":     "dev_a8f3…",
  "device_ip":     "10.88.0.5",
  "site_id":       "pudong:site_1",
  "hub_slug":      "pudong",
  "mesh_cidr":     "10.88.0.0/24",
  "hub":           { … },
  "peers":         [ … ],
  "keepalive_sec": 25,
  "refresh_sec":   300,
  "token_expires": "2026-08-17T00:00:00Z"
}
```

Source of truth: client overwrites `/etc/wireguard/<iface>.conf` each
refresh from this response.

### Errors

| Status | Body `error` | Meaning |
|---|---|---|
| 401 | `missing bearer token` | no `Authorization` header |
| 401 | `missing X-Device-Id` | no `X-Device-Id` header |
| 401 | `invalid device token` | token doesn't match any live device |
| 401 | `token does not match X-Device-Id` | header pair inconsistent |
| 401 | `token expired` | `token_expires_at` in the past |

---

## 3. `GET /v1/hub/peers` — hub-only flat peer list

For the hub's own daemon. Returns ALL active devices in this hub's
mesh, flattened (no LAN/hub split). Hub uses this to rewrite its own
`[Peer]` list — every device gets `AllowedIPs = <device_wg_ip>/32`.

Auth: same as `/v1/peers`, but the caller must be the device currently
bound to this hub (`wg_hubs.bound_device_id`). Non-hub calls get 403.

### Request

```http
GET /v1/hub/peers HTTP/1.1
Authorization: Bearer <hub-device-token>
X-Device-Id:   <dev_id_of_hub>
```

### Response 200

```json
{
  "peers": [
    { "pubkey": "TqbeoU…", "wg_ip": "10.88.0.2/32", "hostname": "yarshure-mac" },
    { "pubkey": "9yRYL…",  "wg_ip": "10.88.0.3/32", "hostname": "another-mac" }
  ],
  "rev":         "1779015200000000000-2",   // ETag-style; skip rewrite if unchanged
  "refresh_sec": 300
}
```

`rev` is `<max(created_at|last_seen_at) UnixNano>-<peer count>`.
Implementation detail; treat as opaque — just compare with last seen.

### Errors

| Status | Body `error` | Meaning |
|---|---|---|
| 401 | (same as /v1/peers) | auth failure |
| 403 | `not the hub for this mesh` | caller is a regular device, not the bound hub |

---

## 4. `POST /v1/heartbeat`

Optional, but recommended. Drives server-side online/stale tracking
and lets the admin UI show last-seen timestamps.

### Request

```http
POST /v1/heartbeat HTTP/1.1
Authorization: Bearer <device-token>
X-Device-Id:   <dev_id>
Content-Type:  application/json
```

```json
{
  "lan_addrs":  [ { "iface": "en0", "cidr": "192.168.11.42/24" } ],  // optional, detect roam
  "wg_endpoint": "203.0.113.5:51820",                                 // optional, public observed peer
  "stats": {                                                          // all fields optional
    "rx_bytes":            1234567,
    "tx_bytes":            9876543,
    "last_handshake_sec":  42
  }
}
```

### Response 200

Empty body.

Server side-effects: append a row to `wg_heartbeats`, mirror
`last_seen_at` + `wg_endpoint` + `lan_addrs` onto the device row.

---

## 5. `POST /v1/leave`

Voluntary deregister. Server marks the device removed; the hub (if
caller IS the hub) also gets `pubkey` + `bound_device_id` cleared so
a new hub-token install can reclaim the slot.

### Request

```http
POST /v1/leave HTTP/1.1
Authorization: Bearer <device-token>
X-Device-Id:   <dev_id>
```

(Body is ignored; server identifies the device from the bearer.)

### Response 200

Empty body.

---

## 6. `POST /v1/token/refresh`

Rotate the device token before `token_expires` to keep the device
authenticated. Old token invalidated immediately upon a successful
refresh response — write the new token to disk first, then stop using
the old.

### Request

```http
POST /v1/token/refresh HTTP/1.1
Authorization: Bearer <device-token>
X-Device-Id:   <dev_id>
```

### Response 200

```json
{
  "token":   "polar_wg_…",                  // new bearer
  "expires": "2026-11-17T00:00:00Z"         // null if the original was non-expiring
}
```

If you let the token expire before refreshing, you must re-register
with a freshly-issued admin token (re-run install.sh).

---

## 7. `GET /v1/install` / `GET /v1/install/:version`

Unauthenticated. Returns a `text/x-shellscript` Bash installer with
the server URL + bundle version baked in at render time. Operator
runs it as:

```bash
curl -sSL https://zen.4950.store/v1/install | sudo bash -s -- --token=<TOKEN>
```

Optional flags consumed by the script:
- `--hostname=NAME`   — override the reported hostname.
- `--site=SLUG`       — override site auto-detection.

`/v1/install/:version` lets the caller pin a specific bundle version
(e.g. `/v1/install/20260517-abc12345`) instead of `latest`.

---

## 8. `GET /v1/bundle` / `GET /v1/bundle/:version`

Unauthenticated. Streams the `wg-mac` tarball.

- `GET /v1/bundle` → `302 Location: /v1/bundle/<latest>`.
- `GET /v1/bundle/:version` → `200`, `Content-Type: application/gzip`,
  `Content-Disposition: attachment; filename="wg-mac-<version>.tar.gz"`,
  `Cache-Control: public, max-age=31536000, immutable`.

`Content-Length` is set; install.sh can sha256-verify if needed.

---

## End-to-end smoke recipe

```bash
SERVER=https://zen.4950.store

# 1. Hub bootstrap. Admin minted role=hub token, fed slug+endpoint+cidr.
HUB_TOKEN=polar_wg_…
HUB_REGISTER=$(curl -fsSL -X POST $SERVER/v1/register \
  -H 'Content-Type: application/json' \
  -d '{"token":"'"$HUB_TOKEN"'","pubkey":"<HUB_PUB>","wg_listen":51820,
       "hostname":"hub-mac","os":"darwin","arch":"arm64"}')
echo "$HUB_REGISTER" | jq .
HUB_DEVICE_ID=$(echo "$HUB_REGISTER" | jq -r .device_id)
HUB_DEVICE_TOKEN=$(echo "$HUB_REGISTER" | jq -r .token)
# Expect: role="hub", device_ip="10.88.0.1"

# 2. Device join. Admin minted role=device token under same hub.
DEV_TOKEN=polar_wg_…
DEV_REGISTER=$(curl -fsSL -X POST $SERVER/v1/register \
  -H 'Content-Type: application/json' \
  -d '{"token":"'"$DEV_TOKEN"'","pubkey":"<DEV_PUB>","wg_listen":51820,
       "hostname":"yarshure-mac","os":"darwin","arch":"arm64",
       "lan_addrs":[{"iface":"en0","cidr":"192.168.1.10/24"}]}')
echo "$DEV_REGISTER" | jq .
DEV_DEVICE_ID=$(echo "$DEV_REGISTER" | jq -r .device_id)
DEV_DEVICE_TOKEN=$(echo "$DEV_REGISTER" | jq -r .token)
# Expect: role="device", device_ip="10.88.0.2", peers[] contains the hub.

# 3. Device polls.
curl -fsSL -H "Authorization: Bearer $DEV_DEVICE_TOKEN" \
            -H "X-Device-Id: $DEV_DEVICE_ID" \
     "$SERVER/v1/peers" | jq .

# 4. Hub polls its flat list.
curl -fsSL -H "Authorization: Bearer $HUB_DEVICE_TOKEN" \
            -H "X-Device-Id: $HUB_DEVICE_ID" \
     "$SERVER/v1/hub/peers" | jq .

# 5. Heartbeat (device).
curl -fsSL -X POST -H "Authorization: Bearer $DEV_DEVICE_TOKEN" \
                    -H "X-Device-Id: $DEV_DEVICE_ID" \
                    -H 'Content-Type: application/json' \
     -d '{"wg_endpoint":"203.0.113.5:51820","stats":{"rx_bytes":1000,"tx_bytes":2000,"last_handshake_sec":15}}' \
     "$SERVER/v1/heartbeat"

# 6. Device leaves.
curl -fsSL -X POST -H "Authorization: Bearer $DEV_DEVICE_TOKEN" \
                    -H "X-Device-Id: $DEV_DEVICE_ID" \
     "$SERVER/v1/leave"
```

---

## Related docs

- `doc/JOIN_PROTOCOL.md` — design notes + IP plan + state machine
- `doc/playbook.md §12` — operator + admin workflows
- `internal/app/dock/wg_handlers.go` — handler source of truth

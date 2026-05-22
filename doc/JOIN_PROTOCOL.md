# wg-mac join protocol — design v0

Tailscale-style onboarding for the wgctl/wg_core stack:

```
curl -sSL https://join.example/install | sudo bash -s -- --token=...
```

After ~10 s the device is online in the mesh, with a wg IP, a `wgc0`
interface managed by launchd, and a peer list that combines LAN-direct
neighbors and a hub-routed fallback for cross-network peers.

This doc nails down: the HTTP contract the Go control server must
implement, the IP plan, the hybrid routing algorithm, and the client
state machine.

---

## 1. IP plan

Single shared /16, sliced by network "site":

```
10.88.0.0/16   mesh
   10.88.0.0/24   hub site   — control plane runs here; one IP per
                              hub instance (10.88.0.1 = primary hub)
   10.88.<S>.0/24  site S    — per-LAN allocation
                              S ∈ [1, 254]; assigned by server
   device IP      10.88.<S>.<D>/32   D ∈ [2, 254]
```

A "site" = a LAN (one NAT'd public IP, one /24 of LAN IPs). The server
groups devices into sites either by:

- **explicit** `site_id` in the register payload (if the operator labels),
- or **auto**: hash of `{public_ip, lan_cidr}` reported by the device.

Hub itself is a peer in every device's [Peer] list, used for cross-site
forwarding (see §5).

---

## 2. HTTP API

All requests are JSON over HTTPS. Server URL is baked into install.sh
at hosting time, so the client doesn't need a discovery step.

### First-register-wins hub election (v0.2)

Convention: **the first device to call `/v1/register` against a fresh
control plane becomes the hub** ("老大哥"). The server, on seeing an
empty `wg_hub` row at registration time:

1. Allocates that device `device_ip = 10.88.0.1/32`, `site_id = "hub"`.
2. Populates `wg_hub.pubkey` + `wg_hub.endpoint` from the device's
   submitted `pubkey` + observed source `host:wg_listen`.
3. Returns the response with `role: "hub"` (new field, see below).
4. From then on, this device's pubkey IS the hub.

Subsequent `/v1/register` calls see a populated `wg_hub`, follow the
normal device path, and the hub appears as a [Peer] in their conf.

Failure modes:
- Hub deregisters via `/v1/leave` → control plane MUST also clear
  `wg_hub` so the next register reclaims the role.
- Hub goes silent for `> hub_takeover_sec` (config, default 24 h with
  no heartbeat) → server may flag for manual takeover; not automatic
  (avoid split-brain).

Client implications (handled by install.sh / wgctl):
- After register, branch on `role`:
  - `role == "hub"`: render conf with `Address = 10.88.0.1/24`, no
    initial [Peer] list, enable `net.inet.ip.forwarding=1`, install a
    poll-hub-peers agent that rewrites the conf from `/v1/hub/peers`.
  - `role == "device"` (default): render conf with hub peer + LAN
    peers as in §5.

### POST /v1/register

First-time join. Token is consumed (or rotated, depending on policy).

Request:
```json
{
  "token":     "k7w-zX9p-q…",        // platform-issued
  "pubkey":    "TqbeoU9mc…=",         // wg public key (base64)
  "hostname":  "yarshure-mac",        // for human display
  "os":        "darwin",
  "arch":      "arm64",
  "agent_ver": "wg-mac-20260517-e360fd07",
  "lan_addrs": [                      // for site detection + LAN reach
    {"iface": "en0",  "cidr": "192.168.11.79/24"},
    {"iface": "en10", "cidr": "10.0.0.42/24"}
  ],
  "wg_listen": 51820                  // UDP port wg_core listens on
}
```

Response 200:
```json
{
  "device_id":   "dev_a8f3…",         // opaque, returned on subsequent calls
  "device_ip":   "10.88.5.7",         // assigned wg IP
  "site_id":     "site_5",
  "role":        "device",            // "hub" iff this caller is the
                                       // first registered ever (see
                                       // §2 "first-register-wins hub")
  "mesh_cidr":   "10.88.0.0/16",
  "hub": {
    "pubkey":    "DwyGEhX…=",
    "endpoint":  "vpn.example.net:51820",   // public DDNS / static IP
    "wg_ip":     "10.88.0.1"
  },
  "peers": [                          // per-device tailored list, see §5
    {
      "pubkey":   "1isIQrxH…=",
      "wg_ip":    "10.88.5.3",
      "endpoint": "192.168.11.79:51820",   // LAN-direct
      "site_id":  "site_5",
      "hostname": "yarshure-dev"
    },
    {
      "pubkey":   "DwyGEhX…=",
      "endpoint": "vpn.example.net:51820",  // hub
      "site_id":  "hub",
      "allowed_extra": [                   // cross-site subnets routed via hub
        "10.88.0.0/24",
        "10.88.7.0/24"
      ]
    }
  ],
  "keepalive_sec": 25,
  "refresh_sec":   300                // next /v1/peers poll interval
}
```

Errors:
- `401 invalid_token`
- `409 pubkey_already_registered` (idempotent retry with same token + pubkey returns 200)
- `429 token_exhausted`
- `500` with `{"error": "…"}` body

### GET /v1/peers

Polled periodically by client to refresh peer list (new joins, IP
rotations, removed devices). Auth via `Authorization: Bearer <token>` AND
`X-Device-Id: <device_id>` returned at register.

Response 200 (same peer shape as /register):
```json
{
  "device_ip":    "10.88.5.7",        // unchanged unless rotated
  "peers":        [...],
  "hub":          {...},
  "keepalive_sec": 25,
  "refresh_sec":   300,
  "token_expires": "2026-06-17T00:00:00Z"  // null if non-expiring
}
```

The server is the source of truth — the client overwrites its conf with
each refresh.

### POST /v1/heartbeat

Optional lightweight liveness signal. Server uses this to mark "online"
vs "stale" devices.

Request:
```json
{
  "device_id":   "dev_a8f3…",
  "lan_addrs":   [...],                // detect roam
  "wg_endpoint": "1.2.3.4:51820",      // public observed peer
  "stats": {                           // optional
    "rx_bytes": 1234567,
    "tx_bytes": 9876543,
    "last_handshake_sec": 42
  }
}
```

Response 200, no body. Drives the server-side "is device alive" view.

### POST /v1/leave

Voluntary deregister. Server marks device removed; subsequent /v1/peers
on remaining devices will no longer include it.

```json
{ "device_id": "dev_a8f3…", "token": "…" }
```

### POST /v1/token/refresh

For rotating tokens (platform-managed). Client sends current token, gets
back a new one. Old token invalidated immediately.

```json
{ "device_id": "dev_a8f3…", "token": "<current>" }
→ { "token": "<new>", "expires": "2026-06-17T00:00:00Z" }
```

If client misses the rotation window (old token has expired), it must
re-register with a fresh platform-issued token — exactly the same as a
fresh install.

### GET /v1/hub/peers

Hub-only endpoint, called by the hub's periodic agent to refresh its
own `[Peer]` list. Auth via `Authorization: Bearer <hub-token>` AND
`X-Device-Id: <hub-device-id>` returned from /v1/register when the
caller was elected hub.

Server validates the caller is the current hub (matches `wg_hub.pubkey`
implicitly via device_id), then returns ALL active devices flattened:

```json
{
  "peers": [
    { "pubkey": "TqbeoU…", "wg_ip": "10.88.1.2/32", "hostname": "yarshure-mac" },
    { "pubkey": "9yRYL…",  "wg_ip": "10.88.1.3/32", "hostname": "another-mac" },
    ...
  ],
  "rev":          "etag-or-monotonic",   // for change detection
  "refresh_sec":  30
}
```

Hub agent uses `rev` to skip rewrite when unchanged. On change:
overwrite `/etc/wireguard/<hub-iface>.conf` with the new peer list and
`launchctl kickstart -k system/com.wireguard.wg-mac.<hub-iface>`.

### GET /v1/install (or /install.sh)

Static endpoint serving the install.sh script. Operator can rotate the
embedded server URL by re-uploading.

### GET /v1/bundle (or /bundle.tar.gz)

Static endpoint serving the latest wg-mac tarball. Install.sh fetches
this. Server returns `Content-Type: application/gzip` and ideally
`Content-Length` so the client can verify.

---

## 3. Token lifecycle (platform-managed)

Server tracks each token's lifecycle:

```
issued ──┐
         │  POST /v1/register
         ▼
       bound to device  ──┐
         │                │  POST /v1/token/refresh
         │                ▼
         │              rotated ─┐
         │                       │  POST /v1/leave  OR
         │                       │  POST /v1/token/refresh ...
         │                       ▼
         └─────────► expired / revoked
```

- A fresh device join requires a token in state "issued".
- After successful /register, the token is bound to {device_id, pubkey}.
  Re-using the same token on a different device → 401.
- Periodic refresh: client calls /v1/token/refresh well before
  `token_expires`. Suggested 80% of TTL.
- If refresh fails (revoked / expired): wg_core keeps running, client
  marks state `degraded`, retries every 5 min. Operator must re-issue
  a token out-of-band (e.g. SSO portal).

This matches your "可能定期刷新" requirement.

---

## 4. Client state on disk

```
/etc/wgctl/
  config.json                       chmod 0600
    {
      "server":     "https://join.example",
      "device_id":  "dev_a8f3…",
      "token":      "…",
      "token_expires": "2026-06-17T00:00:00Z",
      "wg_ip":      "10.88.5.7",
      "site_id":    "site_5",
      "last_refresh": "2026-05-17T03:42:00Z"
    }

/etc/wireguard/wgc0.conf             rendered from /v1/peers response
                                     (replaces hand-written wg0.conf
                                     in the join-flow path)

/var/log/wgctl-agent.log             refresh + heartbeat events
```

The wg secret (PrivateKey) stays in `/etc/wireguard/wgc0.conf` and is
generated by `wgctl genkey` once on first install. It is never sent to
the server — only the corresponding public key is.

---

## 5. Hybrid peer-list computation (server side)

Given device D in site S, the server emits:

```
peers(D) = LAN_peers(S) + [hub]
```

Where:

- **LAN_peers(S)** = all alive devices in site S except D.
  Each entry has the peer's LAN-direct endpoint (its reported
  `lan_addrs` + `wg_listen`) and AllowedIPs = `<peer_wg_ip>/32`.

- **hub** entry: the central forwarder.
  - `endpoint`: public DDNS or static IP of the hub.
  - `allowed_extra`: union of all `10.88.<S'>.0/24` for S' ≠ S, plus
    `10.88.0.0/24` (the hub's own subnet, so hub services are reachable).

Client renders /etc/wireguard/wgc0.conf as:

```ini
[Interface]
PrivateKey = <local>
Address    = 10.88.5.7/32
ListenPort = 51820

# LAN peer 1
[Peer]
PublicKey  = 1isIQrxH…=
Endpoint   = 192.168.11.79:51820
AllowedIPs = 10.88.5.3/32
PersistentKeepalive = 25

# … more LAN peers …

# hub (cross-site forwarder)
[Peer]
PublicKey  = DwyGEhX…=
Endpoint   = vpn.example.net:51820
AllowedIPs = 10.88.0.0/24, 10.88.7.0/24, 10.88.9.0/24
PersistentKeepalive = 25
```

Routing on D:
- Packet to 10.88.5.3 → wg cryptokey routing hits LAN peer 1 entry → direct.
- Packet to 10.88.7.42 → matches hub's AllowedIPs `10.88.7.0/24` → encrypted to hub → hub IP-forwards within its own AllowedIPs trie to the actual device.

Hub requirements:
- `net.inet.ip.forwarding=1`
- Its own [Peer] list contains every device, each with AllowedIPs =
  `<device_wg_ip>/32`. (Hub gets the union from the same API.)
- NAT optional: needed only if mesh devices also want internet egress
  via hub. Pure mesh-internal traffic needs forwarding only.

A device entering or leaving site S triggers a peer-list refresh on
every other device in S, and on the hub. Strategy: client polls
`/v1/peers` every `refresh_sec` (server-tunable, default 300 s); server
may also push via long-polling if it wants instant updates (v2).

---

## 6. install.sh shape

Client-side bootstrap, served by control server. ~120 lines bash.

```bash
#!/bin/bash
# Usage: curl -sSL <server>/install | sudo bash -s -- --token=<TOKEN> [--hostname=NAME]
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "must run as root"; exit 1; }

SERVER="https://__SERVER_PLACEHOLDER__"      # substituted at upload time
TOKEN=""; HOSTNAME_OVERRIDE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --token=*)    TOKEN="${1#*=}";;
    --hostname=*) HOSTNAME_OVERRIDE="${1#*=}";;
    *) echo "unknown arg: $1" >&2; exit 1;;
  esac
  shift
done
[[ -n "$TOKEN" ]] || { echo "--token=<TOKEN> required"; exit 1; }

# 1. Fetch + extract bundle
TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT
echo "==> downloading bundle"
curl -fsSL "$SERVER/v1/bundle" -o "$TMP/bundle.tar.gz"
mkdir -p "$TMP/wg-mac"
tar xzf "$TMP/bundle.tar.gz" -C "$TMP/wg-mac" --strip-components=1

# 2. Install binaries via existing install.sh (WG_SKIP_BUILD=1)
echo "==> installing binaries"
WG_SKIP_BUILD=1 bash "$TMP/wg-mac/scripts/install.sh" wgc0 \
    >/dev/null 2>&1 || true                  # bootstraps without conf yet —
                                             # we'll write conf next then kickstart

# 3. Generate keypair + state
mkdir -p /etc/wgctl
chmod 0700 /etc/wgctl
PRIV=$(/usr/local/bin/wgctl genkey)
PUB=$(echo "$PRIV" | /usr/local/bin/wgctl pubkey)

# 4. Collect facts
HOSTNAME_REPORT="${HOSTNAME_OVERRIDE:-$(scutil --get LocalHostName 2>/dev/null || hostname)}"
LAN_JSON=$(ifconfig | awk '
  /^[a-z]/ { iface=$1; sub(/:/,"",iface) }
  /inet / && $2 !~ /^127\./ && $2 !~ /^169\.254\./ {
    n = split($4, oct, "."); mask = $4    # /sbin/ifconfig prints netmask hex; convert separately
    printf "{\"iface\":\"%s\",\"cidr\":\"%s/%s\"}\n", iface, $2, "24"
  }' | paste -sd, -)

WG_LISTEN=51820  # default; can be overridden via env

# 5. Register
echo "==> registering with control plane"
RESP=$(curl -fsSL -X POST "$SERVER/v1/register" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<JSON
{
  "token": "$TOKEN",
  "pubkey": "$PUB",
  "hostname": "$HOSTNAME_REPORT",
  "os": "darwin",
  "arch": "$(uname -m)",
  "agent_ver": "$(cat $TMP/wg-mac/VERSION 2>/dev/null || echo unknown)",
  "lan_addrs": [$LAN_JSON],
  "wg_listen": $WG_LISTEN
}
JSON
)")

# 6. Parse response, render conf, kickstart
python3 - "$PRIV" "$WG_LISTEN" <<PY
import json, os, sys
priv, listen = sys.argv[1], sys.argv[2]
resp = json.loads(os.environ["RESP"])
out = open("/etc/wireguard/wgc0.conf", "w")
out.write(f"""[Interface]
PrivateKey = {priv}
Address    = {resp['device_ip']}/32
ListenPort = {listen}

""")
for p in resp["peers"]:
    extras = p.get("allowed_extra", [])
    aips = [p.get("wg_ip") + "/32"] if p.get("wg_ip") else []
    aips += extras
    out.write(f"""[Peer]
PublicKey  = {p['pubkey']}
Endpoint   = {p['endpoint']}
AllowedIPs = {", ".join(aips)}
PersistentKeepalive = {resp.get('keepalive_sec', 25)}

""")
out.close()
os.chmod("/etc/wireguard/wgc0.conf", 0o600)

state = {
  "server":     os.environ["SERVER"],
  "device_id":  resp["device_id"],
  "token":      os.environ["TOKEN"],
  "wg_ip":      resp["device_ip"],
  "site_id":    resp["site_id"],
}
import json
open("/etc/wgctl/config.json", "w").write(json.dumps(state, indent=2))
os.chmod("/etc/wgctl/config.json", 0o600)
PY

# 7. Kickstart launchd
launchctl kickstart -k system/com.wireguard.wg-mac.wgc0

echo "==> done. wg_ip=$(jq -r .wg_ip /etc/wgctl/config.json)"
echo "    sudo wgctl show wgc0"
```

Notes:
- Uses Python only for JSON munging (macOS ships python3 in CLT).
- Env vars are exported just before the heredoc; abbreviated above.
- All paths under /etc/wireguard/ + /etc/wgctl/ are root-only.

---

## 7. wgctl agent (periodic refresh)

A small daemon, or a launchd-scheduled `wgctl refresh` command:

```bash
# /Library/LaunchDaemons/com.wireguard.wgctl-agent.plist
# Runs every refresh_sec; rewrites /etc/wireguard/wgc0.conf if peer list
# changed; kickstarts wg_core to pick up new config.

wgctl refresh
```

`wgctl refresh` behavior:
1. Read /etc/wgctl/config.json
2. If token expires within 24 h → POST /v1/token/refresh
3. GET /v1/peers (with Bearer token)
4. Compare returned peer set against current /etc/wireguard/wgc0.conf
5. If changed: rewrite conf, kickstart launchd, log to
   /var/log/wgctl-agent.log
6. POST /v1/heartbeat with stats from `wgctl show wgc0`

This is the second wgctl subcommand to add — small, ~150 lines C, or
move to a Go agent if the team prefers.

---

## 8. Open items

| Item | Suggestion | Why |
|------|-----------|-----|
| Bundle signing | minisign or cosign signature alongside tar.gz; install.sh verifies before running | curl-pipe-bash is fine for trusted server but signing closes MITM gap |
| Server TLS | LetsEncrypt + DDNS on the public hub | bearer tokens over plaintext = bad |
| ipv6 in mesh | mesh_cidr += `fd88::/32`; device gets `/128` | matches tailscale conventions |
| Magic DNS | hub runs DNS at 10.88.0.1, hostnames → wg_ip | optional, post-v1 |
| NAT traversal for two devices both behind NAT | DERP-style relay, or coturn | hybrid topology already routes those through hub today, so v1 doesn't need it |
| wgctl_max_peers | bump from 8 to 64 (currently `#define WG_MAX_PEERS 8` in wg_core.c) | mesh growth |
| Token rotation jitter | server staggers refresh_sec per device | avoids thundering herd |

---

## 9. Implementation order suggested

### v0.1 (initial, partly done)
1. (Go) Stand up minimal control server: `/v1/register`, `/v1/peers`,
   `/v1/install`, `/v1/bundle`. Stub `/v1/heartbeat` + `/v1/token/refresh`. ✓
2. (C, wg_core) Bump `WG_MAX_PEERS` from 8 to 64. ✓
3. (Shell) Write `install.sh` per §6, hosted at `<server>/v1/install`. ✓
4. (Nginx) Expose `/v1/*` to public — currently dock-only, route not
   yet proxied. Pending.
5. (Go) Site allocator: normalize `firstLANCIDR` to network address so
   devices on the same /24 get the same site. Currently uses raw device
   CIDR which produces a site-per-device. Pending.

### v0.2 (first-register-wins hub election — *current branch*)
6. (Go) On `/v1/register` with empty `wg_hub`: assign caller `role=hub`,
   `device_ip=10.88.0.1`, populate `wg_hub.pubkey/endpoint` from caller's
   pubkey + observed `source_ip:wg_listen`. Pending.
7. (Go) New `/v1/hub/peers` endpoint per §2. Pending.
8. (Go) `/v1/leave` for hub also clears `wg_hub` so the next caller
   reclaims the role. Pending.
9. (Shell) `install.sh` branches on `role`:
    - hub: render conf at 10.88.0.1/24, enable IP forwarding, install
      hub-agent.plist.
    - device: existing behavior. Pending after server changes land.
10. (C, wgctl) New `wgctl hub-refresh` subcommand (mac side): polls
    `/v1/hub/peers`, rewrites hub iface conf, kickstarts. Pending.
11. (Plist) `com.wireguard.wgctl-hub-agent.plist` runs `wgctl hub-refresh`
    every `refresh_sec`. Pending.

### v0.3 (post-hub)
12. (C, wgctl) `wgctl refresh` for device-side peer-list sync (§7).
13. (Plist) `com.wireguard.wgctl-agent.plist` for device-side refresh.
14. (Go) Heartbeat ingestion + alive/stale tracking.
15. (Go) Cross-site hub `allowed_extra` computation per §5.

The Mac side (4 + 9 + 10 + 12 in wgctl/install.sh) waits on the Go side
items above before it can integrate. Until then, manual hub conf on the
first device, and clients fall back to "no hub, LAN-direct only" mode.

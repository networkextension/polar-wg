#!/bin/bash
# join-linux.sh — onboard a Linux (x86_64 / arm64) host into the mesh.
#
# Linux peer to scripts/join.sh. The control-plane protocol is identical
# (doc/JOIN_PROTOCOL.md); only the host integration differs:
#
#   macOS join.sh            Linux join-linux.sh
#   ───────────────────      ───────────────────────────────
#   user-space wg_core    →  in-kernel WireGuard via wg-quick
#   launchd plist         →  systemd: wg-quick@<iface> + a refresh timer
#   scutil / ifconfig     →  hostname / ip(8)
#   /etc/wgctl/config.json→  /etc/wgctl/<iface>.json  (per-iface from the
#                            start — no single-file clobber, multiple
#                            ifaces coexist; cf. wgctl-agent.sh's design)
#
# Linux ships WireGuard in the kernel, so there is NO wg_core / wgctl
# binary and NO bundle download — we drive the stock wireguard-tools.
#
# Usage:
#   sudo bash scripts/join-linux.sh --token=<TOKEN> --server=https://join.example
#
# Optional args:
#   --hostname=NAME   override the registered hostname (default: hostname -s)
#   --listen=PORT     wg UDP listen port (default: 1632, matches the mesh)
#   --iface=NAME      logical iface name (default: wgc0)
#   --reinstall       re-register even if /etc/wgctl/<iface>.json exists
#   --host-id=ID      polar-hosts host.id for the wg↔hosts cross-link
#                     (auto-read from ~/.polar/agent.toml if omitted)

set -euo pipefail

# ── parse args ───────────────────────────────────────────────────────────────
SERVER="__SERVER_PLACEHOLDER__"
TOKEN=""
HOSTNAME_OVERRIDE=""
SITE_SLUG=""
# Mesh-wide UDP port. The control plane advertises each node's endpoint as
# <ip>:<wg_listen>, so this MUST match what peers are told to dial — the macOS
# join.sh uses 1632, so Linux nodes do too. A mismatch (e.g. the old 51820
# default) means peers dial a port nothing listens on → handshake never lands.
WG_LISTEN=1632
IFACE="wgc0"
REINSTALL=0
HOST_ID=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --token=*)    TOKEN="${1#*=}";;
        --hostname=*) HOSTNAME_OVERRIDE="${1#*=}";;
        --site=*)     SITE_SLUG="${1#*=}";;
        --listen=*)   WG_LISTEN="${1#*=}";;
        --iface=*)    IFACE="${1#*=}";;
        --reinstall)  REINSTALL=1;;
        --server=*)   SERVER="${1#*=}";;
        --host-id=*)  HOST_ID="${1#*=}";;
        *) echo "unknown arg: $1" >&2; exit 1;;
    esac
    shift
done

# host_id ties this wg device to its polar-hosts host row, lighting up the
# bidirectional wg_devices↔hosts cross-link at register time (vs the fragile
# hello-backfill that needs the agent to read+match the same wg pubkey). If
# not passed explicitly, read it from the local polar-agent config when present
# — most wg nodes also run polar-agent, which wrote host_id there at enroll.
if [[ -z "$HOST_ID" ]]; then
    for cfg in "$HOME/.polar/agent.toml" /root/.polar/agent.toml /home/*/.polar/agent.toml; do
        [[ -r "$cfg" ]] || continue
        HOST_ID=$(sed -n 's/^[[:space:]]*host_id[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg" | head -1)
        [[ -n "$HOST_ID" ]] && { echo "==> using host_id from $cfg"; break; }
    done
fi

[[ $EUID -eq 0 ]] || { echo "must run as root (use sudo)" >&2; exit 1; }
[[ -n "$TOKEN" ]] || { echo "--token=<TOKEN> required" >&2; exit 1; }
[[ "$SERVER" != "__SERVER_PLACEHOLDER__" ]] || {
    echo "SERVER not set; pass --server=https://..." >&2; exit 1; }
SERVER="${SERVER%/}"

# Validate iface name the same way install.sh / wgctl do, before it
# reaches a conf path or a systemd unit name.
[[ "$IFACE" =~ ^[a-z0-9_-]{1,15}$ ]] || {
    echo "error: invalid iface '$IFACE' (lowercase a-z, 0-9, _-, 1-15 chars)" >&2
    exit 1; }

# ── dependency check ─────────────────────────────────────────────────────────
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; MISSING=1; }; }
MISSING=0
need wg; need wg-quick; need curl; need python3; need ip; need systemctl
if [[ $MISSING -eq 1 ]]; then
    echo >&2
    echo "install on Debian/Ubuntu:  sudo apt install wireguard-tools curl python3" >&2
    echo "         on RHEL/Fedora:    sudo dnf install wireguard-tools curl python3" >&2
    exit 1
fi

# Token prefix sanity (mirror of join.sh) — redirect a pasted Tailscale key.
case "$TOKEN" in
    polar_wg_*) ;;
    tskey-*)
        cat >&2 <<TS
✗ This looks like a Tailscale PreAuthKey, not a wg-mac token.
  To onboard with the official Tailscale client instead:

      tailscale up --login-server=${SERVER} --authkey=${TOKEN}

  See ${SERVER}/v1/install for the native variant.
TS
        exit 2;;
    *) echo "warning: token does not start with polar_wg_ — server will be the final arbiter" >&2;;
esac

STATE_DIR=/etc/wgctl
STATE_FILE="$STATE_DIR/$IFACE.json"
CONF="/etc/wireguard/$IFACE.conf"
RENDER_HELPER=/usr/local/sbin/wgctl-render-linux
REFRESH_HELPER=/usr/local/sbin/wgctl-refresh-linux
HB_HELPER=/usr/local/sbin/wgctl-hb-linux

# Honor existing install unless --reinstall. Per-iface, so a second iface
# joins cleanly without touching the first (unlike macOS join.sh).
if [[ -f "$STATE_FILE" && $REINSTALL -eq 0 ]]; then
    echo "$STATE_FILE exists; pass --reinstall to overwrite" >&2
    exit 1
fi

# ── generate keypair ─────────────────────────────────────────────────────────
echo "==> generating Curve25519 keypair"
umask 077
PRIV=$(wg genkey)
PUB=$(echo "$PRIV" | wg pubkey)

# ── collect lan_addrs ────────────────────────────────────────────────────────
HOSTNAME_REPORT="${HOSTNAME_OVERRIDE:-$(hostname -s 2>/dev/null || hostname)}"

# Global-scope IPv4 with /prefix. We call `ip` from INSIDE python via
# subprocess (not `ip | python3 <<PY`): a heredoc steals python's stdin,
# so the piped form silently reads nothing — and can wedge `ip` on a full
# pipe with no reader. `ip -o` is one line per addr, supported on every
# iproute2 (unlike `-j` JSON on older builds). Skip 127/8, 169.254/16,
# and our own wg ifaces. Any failure → [].
echo "==> collecting LAN interfaces"
LAN_ADDRS_JSON=$(python3 <<'PY'
import json, re, subprocess
out = []
try:
    txt = subprocess.run(["ip", "-o", "-4", "addr", "show", "scope", "global"],
                         capture_output=True, text=True, timeout=10).stdout
except Exception:
    txt = ""
for line in txt.splitlines():
    # e.g. "2: eth0    inet 192.168.1.10/24 brd ... scope global eth0"
    parts = line.split()
    if len(parts) < 4 or parts[2] != "inet":
        continue
    dev = parts[1]
    if dev.startswith(("wgc", "wg")):
        continue
    if not re.match(r"^\d+\.\d+\.\d+\.\d+/\d+$", parts[3]):
        continue
    if parts[3].startswith(("127.", "169.254.")):
        continue
    out.append({"iface": dev, "cidr": parts[3]})
print(json.dumps(out))
PY
)

ARCH_RAW=$(uname -m)
case "$ARCH_RAW" in
    x86_64|amd64)  ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *)             ARCH="$ARCH_RAW" ;;
esac

# ── POST /v1/register ────────────────────────────────────────────────────────
echo "==> registering with control plane $SERVER"
REQ_JSON=$(TOKEN="$TOKEN" PUB="$PUB" HOSTNAME_REPORT="$HOSTNAME_REPORT" \
    ARCH="$ARCH" LAN="$LAN_ADDRS_JSON" WG_LISTEN="$WG_LISTEN" SITE_SLUG="$SITE_SLUG" \
    HOST_ID="$HOST_ID" \
    python3 <<'PY'
import json, os
body = {
    "token":     os.environ["TOKEN"],
    "pubkey":    os.environ["PUB"],
    "hostname":  os.environ["HOSTNAME_REPORT"],
    "os":        "linux",
    "arch":      os.environ["ARCH"],
    "agent_ver": "join-linux",
    "lan_addrs": json.loads(os.environ["LAN"]),
    "wg_listen": int(os.environ["WG_LISTEN"]),
    "site_slug": os.environ["SITE_SLUG"],
}
hid = os.environ.get("HOST_ID", "").strip()
if hid:
    body["host_id"] = hid  # cross-link to polar-hosts; server stamps wg_devices.host_id
print(json.dumps(body))
PY
)

RESP=$(curl -fsSL --retry 3 --connect-timeout 15 --max-time 60 \
    -X POST "$SERVER/v1/register" \
    -H 'Content-Type: application/json' -d "$REQ_JSON") || {
    echo "register failed (curl exit $?); response:" >&2; echo "$RESP" >&2; exit 1; }

# ── lay down render + refresh helpers ────────────────────────────────────────
# The conf renderer is shared by the initial join and every refresh tick,
# so the rendering logic lives in exactly one place. It reads the state
# file (for priv key + iface + listen) and a /register|/peers response on
# stdin, and atomically rewrites <iface>.conf.
echo "==> installing $RENDER_HELPER and $REFRESH_HELPER"
cat > "$RENDER_HELPER" <<'RENDER'
#!/usr/bin/env python3
# wgctl-render-linux <iface> — read a /register|/peers JSON response on
# stdin, rewrite /etc/wireguard/<iface>.conf atomically (0600).
import json, os, sys, tempfile

iface = sys.argv[1]
state = json.load(open(f"/etc/wgctl/{iface}.json"))
resp  = json.load(sys.stdin)

# Address carries the mesh prefix (not /32) so the kernel installs the
# mesh route — same rule as the macOS path (see join.sh comment).
mesh_prefix = (resp.get("mesh_cidr") or "10.88.0.0/24").split("/")[-1]
device_ip   = resp.get("device_ip") or state["wg_ip"]
ka          = resp.get("keepalive_sec", 25)

lines = [
    "# managed by wgctl-refresh-linux — do not edit by hand",
    "[Interface]",
    f"PrivateKey = {state['priv']}",
    f"Address    = {device_ip}/{mesh_prefix}",
    f"ListenPort = {state['listen']}",
    "",
]
for p in resp.get("peers", []):
    # wg_ip arrives bare from /v1/peers ("10.88.5.3") but prefixed from
    # /v1/hub/peers ("10.88.1.2/32") — normalize to a single /32 host route.
    wg_ip = p.get("wg_ip")
    host = (wg_ip if "/" in wg_ip else wg_ip + "/32") if wg_ip else ""
    aips = ([host] if host else []) + (p.get("allowed_extra") or [])
    if not aips:
        continue
    block = ["[Peer]", f"PublicKey  = {p['pubkey']}"]
    # Roaming spokes (the hub's /v1/hub/peers view) carry no endpoint: the hub
    # only responds, it never dials them, so emit neither Endpoint nor
    # keepalive. Peers we DO dial (hub, LAN-direct) carry one → add both.
    ep = p.get("endpoint")
    if ep:
        block.append(f"Endpoint   = {ep}")
    block.append(f"AllowedIPs = {', '.join(aips)}")
    if ep:
        block.append(f"PersistentKeepalive = {ka}")
    block.append("")
    lines += block

conf = f"/etc/wireguard/{iface}.conf"
d = os.path.dirname(conf)
with tempfile.NamedTemporaryFile("w", dir=d, delete=False,
                                 prefix=f".{iface}.conf.") as f:
    f.write("\n".join(lines))
    tmp = f.name
os.chmod(tmp, 0o600)
os.replace(tmp, conf)
RENDER
chmod 0755 "$RENDER_HELPER"

# Heartbeat body builder — prints the /v1/heartbeat JSON for <iface>, built
# from `wg show <iface> dump` + host facts. Mirrors the macOS wgctl-agent
# status block (doc/hub-status.md): for a hub the per-peer roster is the
# authoritative "who's online" view of the whole mesh.
cat > "$HB_HELPER" <<'HB'
#!/usr/bin/env python3
import json, os, subprocess, sys, time

iface = sys.argv[1]
try:
    state = json.load(open(f"/etc/wgctl/{iface}.json"))
except Exception:
    state = {}
role      = state.get("role", "device")
wg_listen = int(state.get("listen", 0) or 0)

def run(*a):
    try:
        return subprocess.run(a, capture_output=True, text=True, timeout=8).stdout
    except Exception:
        return ""

# `wg show <iface> dump` is TAB-separated:
#   line 0: privkey  pubkey  listen-port  fwmark            (interface)
#   line N: pubkey  psk  endpoint  allowed-ips  hs  rx  tx  keepalive  (peer)
now   = int(time.time())
dump  = run("wg", "show", iface, "dump").splitlines()
iface_up = bool(dump)
peers = []
for ln in dump[1:]:
    f = ln.split("\t")
    if len(f) < 8:
        continue
    pub, _psk, endpoint, aips, hs, rx, tx, _ka = f[:8]
    hs   = int(hs or 0)
    last = (now - hs) if hs else None
    wg_ip = ""
    for a in aips.split(","):
        a = a.strip()
        if a and a not in ("(none)", "none"):
            wg_ip = a.split("/")[0]; break
    peers.append({
        "pubkey": pub,
        "wg_ip": wg_ip,
        "endpoint": None if endpoint in ("(none)", "") else endpoint,
        "last_handshake_sec": last,
        "rx_bytes": int(rx or 0),
        "tx_bytes": int(tx or 0),
        "online": last is not None and last < 180,
    })

# lan_addrs — skip loopback / link-local / mesh ranges.
lan = []
for ln in run("ip", "-o", "-4", "addr", "show", "scope", "global").splitlines():
    p = ln.split()
    if len(p) < 4 or p[2] != "inet":
        continue
    dev, cidr = p[1], p[3]
    if dev.startswith("wg"):
        continue
    if cidr.split("/")[0].startswith(("127.", "169.254.", "10.88.", "100.64.")):
        continue
    lan.append({"iface": dev, "cidr": cidr})

# Public egress IP for wg_endpoint.
#
# This used to take the default route NIC's own address, which behind NAT is an
# RFC1918 address and never the egress IP — so an egress change was invisible to
# the control plane. Ask an external echo service instead, cached: ifconfig.co
# allows ~1 request/min per source IP and every device behind one NAT shares
# that budget. On total failure keep serving the last known address (stale beats
# nothing) but back-date the stamp so the retry comes sooner than a full TTL.
# File format matches the shell agents so all three can share the cache.
PUBIP_CACHE = "/etc/wgctl/public_ip"
PUBIP_TTL   = int(os.environ.get("WGCTL_PUBIP_TTL", "900"))
PUBIP_RETRY = int(os.environ.get("WGCTL_PUBIP_RETRY", "120"))
PUBIP_URLS  = os.environ.get(
    "WGCTL_PUBIP_URLS", "https://ifconfig.co/ip https://ifconfig.me/ip").split()

def _is_ipv4(s):
    p = s.split(".")
    return len(p) == 4 and all(q.isdigit() and 0 <= int(q) <= 255 for q in p)

def _write_pubip(stamp, ip):
    try:
        with open(PUBIP_CACHE + ".tmp", "w") as f:
            f.write("%d %s\n" % (stamp, ip))
        os.replace(PUBIP_CACHE + ".tmp", PUBIP_CACHE)
    except Exception:
        pass

def public_ip():
    now, stamp, cached = int(time.time()), 0, ""
    try:
        parts = open(PUBIP_CACHE).read().split()
        stamp, cached = int(parts[0]), (parts[1] if len(parts) > 1 else "")
    except Exception:
        pass
    if cached and now - stamp < PUBIP_TTL:
        return cached
    for url in PUBIP_URLS:
        ip = run("curl", "-4", "-fsS", "--noproxy", "*",
                 "--connect-timeout", "3", "--max-time", "5", url).strip()
        if _is_ipv4(ip):
            _write_pubip(now, ip)
            return ip
    _write_pubip(now - PUBIP_TTL + PUBIP_RETRY, cached)
    return cached

pub_ip = public_ip()

try:
    uptime = int(float(open("/proc/uptime").read().split()[0]))
except Exception:
    uptime = None
arch = run("uname", "-m").strip()
arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(arch, arch)

ages = [p["last_handshake_sec"] for p in peers if p["last_handshake_sec"] is not None]
stats = {
    "rx_bytes": sum(p["rx_bytes"] for p in peers),
    "tx_bytes": sum(p["tx_bytes"] for p in peers),
    "last_handshake_sec": min(ages) if ages else 0,
}
status = {
    "schema": 1, "role": role, "os": "linux", "arch": arch,
    "agent_ver": "join-linux", "iface": iface, "iface_up": iface_up,
    "uptime_sec": uptime, "wg_listen": wg_listen,
    "peer_count": len(peers),
    "peers_online": sum(1 for p in peers if p["online"]),
    "peers": peers,
}
print(json.dumps({
    "lan_addrs":   lan,
    "wg_endpoint": f"{pub_ip}:{wg_listen}" if pub_ip and wg_listen else "",
    "stats":       stats,
    "status":      status,
}))
HB
chmod 0755 "$HB_HELPER"

cat > "$REFRESH_HELPER" <<REFRESH
#!/bin/bash
# wgctl-refresh-linux <iface> — poll the role-correct peers endpoint
# (/v1/hub/peers for a hub, else /v1/peers), re-render the conf, and
# hot-reload via 'wg syncconf' (no interface flap). Called by the
# systemd timer; also runnable by hand. Self-evicts THIS iface on a 401.
set -euo pipefail
IFACE="\${1:?usage: wgctl-refresh-linux <iface>}"
STATE="/etc/wgctl/\$IFACE.json"
[[ -f "\$STATE" ]] || { echo "no state for \$IFACE" >&2; exit 0; }

read_field() { python3 -c "import json,sys; print(json.load(open('\$STATE')).get('\$1',''))"; }
SERVER=\$(read_field server)
DEVICE_ID=\$(read_field device_id)
TOKEN=\$(read_field token)
ROLE=\$(read_field role)

# A hub must pull the FULL mesh roster (/v1/hub/peers); a device gets only its
# site (/v1/peers). Mirrors the macOS wgctl-agent.sh role branch. Without this a
# hub renders from a device-scoped list on the first tick and loses the mesh.
case "\$ROLE" in
    hub) PEER_URL="\$SERVER/v1/hub/peers" ;;
    *)   PEER_URL="\$SERVER/v1/peers" ;;
esac

# Heartbeat: full status block (lan_addrs + per-peer roster + transfer stats),
# built by the hb helper from \`wg show <iface> dump\`. For a hub this roster is
# the authoritative whole-mesh "who's online" view (doc/hub-status.md). Any
# failure is ignored — the peer refresh below is the part that matters.
HB_BODY=\$($HB_HELPER "\$IFACE" 2>/dev/null || echo '{}')
curl -fsS --max-time 8 -X POST "\$SERVER/v1/heartbeat" \\
    -H "Authorization: Bearer \$TOKEN" -H "X-Device-Id: \$DEVICE_ID" \\
    -H 'Content-Type: application/json' -d "\$HB_BODY" \\
    >/dev/null 2>&1 || true

HTTP=\$(mktemp)
# -sS, NOT -fsS: with -f curl exits 22 on a 401 while still printing
# %{http_code}, so the || echo 000 appended a second line and \$CODE became
# "401\\n000" — matching neither branch, which made the eviction below dead
# code and pinned the agent to "keeping current conf" forever.
CODE=\$(curl -sS -o "\$HTTP" -w '%{http_code}' \\
    -H "Authorization: Bearer \$TOKEN" -H "X-Device-Id: \$DEVICE_ID" \\
    "\$PEER_URL" || echo 000)

if [[ "\$CODE" == "401" ]]; then
    echo "[\$IFACE] 401 invalid_device_token — self-evicting" >&2
    wg-quick down "\$IFACE" 2>/dev/null || true
    systemctl disable --now "wg-quick@\$IFACE" 2>/dev/null || true
    rm -f "/etc/wireguard/\$IFACE.conf" "\$STATE"
    rm -f "\$HTTP"; exit 0
fi
if [[ "\$CODE" != "200" ]]; then
    echo "[\$IFACE] \$PEER_URL HTTP \$CODE — keeping current conf" >&2
    rm -f "\$HTTP"; exit 0
fi

$RENDER_HELPER "\$IFACE" < "\$HTTP"
rm -f "\$HTTP"

# Reload peers without tearing the link down: strip wg-quick-only keys,
# then syncconf. If the iface isn't up yet, bring it up instead.
if wg show "\$IFACE" >/dev/null 2>&1; then
    wg syncconf "\$IFACE" <(wg-quick strip "\$IFACE")
else
    wg-quick up "\$IFACE"
fi
REFRESH
chmod 0755 "$REFRESH_HELPER"

# ── write state + initial conf ───────────────────────────────────────────────
echo "==> writing $STATE_FILE and $CONF"
mkdir -p "$STATE_DIR"; chmod 0700 "$STATE_DIR"
mkdir -p /etc/wireguard

PRIV="$PRIV" IFACE="$IFACE" LISTEN="$WG_LISTEN" RESP="$RESP" \
SERVER="$SERVER" TOKEN="$TOKEN" python3 <<'PY'
import json, os, tempfile
resp = json.loads(os.environ["RESP"])
state = {
    "server":        os.environ["SERVER"],
    "device_id":     resp["device_id"],
    "token":         os.environ["TOKEN"],
    "priv":          os.environ["PRIV"],   # local-only; file is 0600 in /etc/wgctl (0700)
    "wg_ip":         resp["device_ip"],
    "site_id":       resp.get("site_id"),
    "iface":         os.environ["IFACE"],
    "listen":        int(os.environ["LISTEN"]),
    "role":          resp.get("role", "device"),
    "token_expires": resp.get("token_expires"),
}
path = f"/etc/wgctl/{os.environ['IFACE']}.json"
with tempfile.NamedTemporaryFile("w", dir="/etc/wgctl", delete=False,
                                 prefix=".state.") as f:
    json.dump(state, f, indent=2)
    tmp = f.name
os.chmod(tmp, 0o600)
os.replace(tmp, path)
PY

# Render the initial conf from the /register response (same shape as /peers).
echo "$RESP" | "$RENDER_HELPER" "$IFACE"

# ── hub role: enable IPv4 forwarding so the hub routes between spokes ─────────
# Not done by wg-quick. Pure mesh-internal traffic needs forwarding only; NAT
# (spokes egress to the internet via the hub) is opt-in — uncomment + set the
# egress NIC below if you want that.
ROLE=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("role","device"))')
if [[ "$ROLE" == "hub" ]]; then
    echo "==> hub role: enabling net.ipv4.ip_forward"
    echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-wg-hub.conf
    sysctl -p /etc/sysctl.d/99-wg-hub.conf >/dev/null || true
    # Egress NAT (spokes exit to the internet / a datacenter subnet via this
    # hub) is now SEMI-AUTOMATIC: the platform opens egress by setting this
    # hub's advertised_routes, the server mirrors them in /v1/hub/peers, and
    # the refresh agent (wg-agent) stands up the MASQUERADE rule on the next
    # poll (and tears it down when egress is closed). Nothing to do at join;
    # see apply_egress_nat in skills/wg-mac-install/scripts/wg-agent.sh.
fi

# ── systemd: bring up the iface + install the refresh timer ──────────────────
echo "==> enabling wg-quick@$IFACE and the refresh timer"
systemctl enable --now "wg-quick@$IFACE"

# refresh_sec from the response drives the timer cadence (default 300s).
REFRESH_SEC=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("refresh_sec",300))')

cat > "/etc/systemd/system/wgctl-refresh@.service" <<UNIT
[Unit]
Description=Refresh WireGuard mesh peers for %i
After=network-online.target wg-quick@%i.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$REFRESH_HELPER %i
UNIT

cat > "/etc/systemd/system/wgctl-refresh@.timer" <<UNIT
[Unit]
Description=Periodic WireGuard mesh peer refresh for %i

[Timer]
OnBootSec=${REFRESH_SEC}s
OnUnitActiveSec=${REFRESH_SEC}s
Unit=wgctl-refresh@%i.service

[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now "wgctl-refresh@$IFACE.timer"

# ── summary ──────────────────────────────────────────────────────────────────
WG_IP=$(python3 -c "import json; print(json.load(open('$STATE_FILE'))['wg_ip'])")
cat <<DONE

  ✓ joined mesh (linux/$ARCH)
      device_ip:  $WG_IP
      iface:      $IFACE
      server:     $SERVER

  status:   sudo wg show $IFACE
  refresh:  sudo $REFRESH_HELPER $IFACE      (re-fetch peers now)
  timer:    systemctl status wgctl-refresh@$IFACE.timer
  leave:    sudo wg-quick down $IFACE && \\
            sudo systemctl disable --now wg-quick@$IFACE wgctl-refresh@$IFACE.timer && \\
            curl -fsSL -X POST "$SERVER/v1/leave" -H 'Content-Type: application/json' \\
                 -d "{\\"device_id\\":\\"$(python3 -c "import json;print(json.load(open('$STATE_FILE'))['device_id'])")\\",\\"token\\":\\"<token>\\"}" && \\
            sudo rm -f $CONF $STATE_FILE

DONE

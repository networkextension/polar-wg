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
#   --listen=PORT     wg UDP listen port (default: 51820)
#   --iface=NAME      logical iface name (default: wgc0)
#   --reinstall       re-register even if /etc/wgctl/<iface>.json exists

set -euo pipefail

# ── parse args ───────────────────────────────────────────────────────────────
SERVER="__SERVER_PLACEHOLDER__"
TOKEN=""
HOSTNAME_OVERRIDE=""
SITE_SLUG=""
WG_LISTEN=51820
IFACE="wgc0"
REINSTALL=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --token=*)    TOKEN="${1#*=}";;
        --hostname=*) HOSTNAME_OVERRIDE="${1#*=}";;
        --site=*)     SITE_SLUG="${1#*=}";;
        --listen=*)   WG_LISTEN="${1#*=}";;
        --iface=*)    IFACE="${1#*=}";;
        --reinstall)  REINSTALL=1;;
        --server=*)   SERVER="${1#*=}";;
        *) echo "unknown arg: $1" >&2; exit 1;;
    esac
    shift
done

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
    python3 <<'PY'
import json, os
print(json.dumps({
    "token":     os.environ["TOKEN"],
    "pubkey":    os.environ["PUB"],
    "hostname":  os.environ["HOSTNAME_REPORT"],
    "os":        "linux",
    "arch":      os.environ["ARCH"],
    "agent_ver": "join-linux",
    "lan_addrs": json.loads(os.environ["LAN"]),
    "wg_listen": int(os.environ["WG_LISTEN"]),
    "site_slug": os.environ["SITE_SLUG"],
}))
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
    extras = p.get("allowed_extra", []) or []
    aips = ([p["wg_ip"] + "/32"] if p.get("wg_ip") else []) + extras
    if not aips:
        continue
    lines += [
        "[Peer]",
        f"PublicKey  = {p['pubkey']}",
        f"Endpoint   = {p['endpoint']}",
        f"AllowedIPs = {', '.join(aips)}",
        f"PersistentKeepalive = {ka}",
        "",
    ]

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

# Best-effort heartbeat so the node shows online + last-seen in admin (the macOS
# agent already does this; Linux didn't). Minimal body; any failure is ignored —
# the peer refresh below is the part that matters.
DEF_IF=\$(ip route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if(\$i=="dev"){print \$(i+1); exit}}')
PUB_IP=\$(ip -o -4 addr show "\$DEF_IF" 2>/dev/null | awk '{print \$4}' | cut -d/ -f1 | head -1)
WG_LISTEN=\$(read_field listen)
curl -fsS --max-time 8 -X POST "\$SERVER/v1/heartbeat" \\
    -H "Authorization: Bearer \$TOKEN" -H "X-Device-Id: \$DEVICE_ID" \\
    -H 'Content-Type: application/json' \\
    -d "{\\"lan_addrs\\":[],\\"wg_endpoint\\":\\"\${PUB_IP}:\${WG_LISTEN}\\"}" \\
    >/dev/null 2>&1 || true

HTTP=\$(mktemp)
CODE=\$(curl -fsS -o "\$HTTP" -w '%{http_code}' \\
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
    # NAT for spoke internet egress via this hub (optional — set EGRESS_IF):
    #   EGRESS_IF=eth0
    #   MESH_CIDR=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("mesh_cidr","100.64.0.0/24"))')
    #   iptables -t nat -C POSTROUTING -s "$MESH_CIDR" -o "$EGRESS_IF" -j MASQUERADE 2>/dev/null || \
    #     iptables -t nat -A POSTROUTING -s "$MESH_CIDR" -o "$EGRESS_IF" -j MASQUERADE
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

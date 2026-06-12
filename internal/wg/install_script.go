package wg

// install.sh renderer for /v1/install — see JOIN_PROTOCOL.md §6.
//
// One template; SERVER, BUNDLE_VERSION, MESH_CIDR baked in at render
// time. Operators run:
//
//   curl -sSL https://zen.4950.store/v1/install | sudo bash -s -- --token=<TOKEN>
//
// The script fetches /v1/bundle/<BUNDLE_VERSION> for the C wg_core /
// wgctl binaries, generates a wg keypair, posts /v1/register, writes
// /etc/wireguard/wgc0.conf, and kicks launchd.

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

type wgInstallScriptInput struct {
	Server        string
	BundleVersion string
	MeshCIDR      string
	OS            string // pinned target os (empty = script auto-detects via uname)
	Arch          string // pinned target arch (empty = auto-detect)
}

const wgInstallScriptTemplate = `#!/bin/bash
# wg-mac join installer (Polar control plane).
# Usage: curl -sSL <server>/v1/install | sudo bash -s -- --token=<TOKEN> [--hostname=NAME] [--site=SLUG]
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "must run as root"; exit 1; }

SERVER='{{.Server}}'
BUNDLE_VERSION='{{.BundleVersion}}'
MESH_CIDR='{{.MeshCIDR}}'
# Pinned target platform (baked by /v1/install?os=&arch=); empty = auto-detect.
TARGET_OS='{{.OS}}'
TARGET_ARCH='{{.Arch}}'

TOKEN=""
HOSTNAME_OVERRIDE=""
SITE_SLUG=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token=*)    TOKEN="${1#*=}";;
    --hostname=*) HOSTNAME_OVERRIDE="${1#*=}";;
    --site=*)     SITE_SLUG="${1#*=}";;
    *) echo "unknown arg: $1" >&2; exit 1;;
  esac
  shift
done
[[ -n "$TOKEN" ]] || { echo "--token=<TOKEN> required"; exit 1; }

# --- 0. resolve target os/arch (baked target wins, else detect locally) ---
OS="${TARGET_OS:-$(uname -s | tr 'A-Z' 'a-z')}"
ARCH_RAW="${TARGET_ARCH:-$(uname -m)}"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) ARCH="$ARCH_RAW" ;;
esac

# --- 1. fetch + extract bundle (server serves the os/arch-specific bundle) ---
TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT
echo "==> downloading wg bundle ($OS/$ARCH $BUNDLE_VERSION)"
PLATQ="os=$OS&arch=$ARCH"
BUNDLE_URL="$SERVER/v1/bundle/$BUNDLE_VERSION?$PLATQ"
if [[ "$BUNDLE_VERSION" == "" || "$BUNDLE_VERSION" == "latest" ]]; then
  BUNDLE_URL="$SERVER/v1/bundle?$PLATQ"
fi
curl -fsSL "$BUNDLE_URL" -o "$TMP/bundle.tar.gz"
mkdir -p "$TMP/wg-mac"
tar xzf "$TMP/bundle.tar.gz" -C "$TMP/wg-mac" --strip-components=1

# --- 2. install binaries (delegates to bundle's install.sh; WG_SKIP_BUILD=1) ---
echo "==> installing binaries"
WG_SKIP_BUILD=1 bash "$TMP/wg-mac/scripts/install.sh" wgc0 \
    >/dev/null 2>&1 || true

# --- 3. generate keypair + state ---
mkdir -p /etc/wgctl /etc/wireguard
chmod 0700 /etc/wgctl
PRIV=$(/usr/local/bin/wgctl genkey)
PUB=$(echo "$PRIV" | /usr/local/bin/wgctl pubkey)

# --- 4. collect facts ---
HOSTNAME_REPORT="${HOSTNAME_OVERRIDE:-$(scutil --get LocalHostName 2>/dev/null || hostname)}"
OS_RAW="$OS"   # resolved in step 0 (baked target or uname); ARCH set there too
AGENT_VER=$(cat "$TMP/wg-mac/VERSION" 2>/dev/null || echo "unknown")
WG_LISTEN=51820

# Build lan_addrs JSON via python (ships with macOS CLT).
LAN_JSON=$(python3 - <<'PYLAN'
import json, subprocess, re
out = []
try:
    blob = subprocess.check_output(["/sbin/ifconfig"], text=True)
except Exception:
    blob = ""
iface = ""
for line in blob.splitlines():
    m = re.match(r"^([a-z0-9]+):", line)
    if m:
        iface = m.group(1)
        continue
    m = re.search(r"inet (\d+\.\d+\.\d+\.\d+)\s+netmask\s+0x([0-9a-fA-F]+)", line)
    if not m:
        continue
    ip, hexmask = m.group(1), m.group(2)
    if ip.startswith("127.") or ip.startswith("169.254."):
        continue
    # Convert hex netmask to CIDR prefix length.
    bits = bin(int(hexmask, 16)).count("1")
    out.append({"iface": iface, "cidr": f"{ip}/{bits}"})
print(json.dumps(out))
PYLAN
)

# --- 5. register ---
echo "==> registering with control plane"
# Export BEFORE the python heredoc so the child process inherits the
# env. (Originally placed below; python ran first and KeyError'd on
# 'TOKEN'.)
export TOKEN PUB HOSTNAME_REPORT OS_RAW ARCH AGENT_VER LAN_JSON WG_LISTEN SITE_SLUG SERVER MESH_CIDR PRIV
REGISTER_BODY=$(python3 - <<PYREG
import json, os
print(json.dumps({
  "token":     os.environ["TOKEN"],
  "pubkey":    os.environ["PUB"],
  "hostname":  os.environ["HOSTNAME_REPORT"],
  "os":        os.environ["OS_RAW"],
  "arch":      os.environ["ARCH"],
  "agent_ver": os.environ["AGENT_VER"],
  "lan_addrs": json.loads(os.environ["LAN_JSON"]) if os.environ.get("LAN_JSON") else [],
  "wg_listen": int(os.environ["WG_LISTEN"]),
  "site_slug": os.environ.get("SITE_SLUG", ""),
}))
PYREG
)
RESP=$(curl -fsSL -X POST "$SERVER/v1/register" \
    -H 'Content-Type: application/json' \
    -d "$REGISTER_BODY")
export RESP

# --- 6. render wgc0.conf + state ---
python3 - <<'PYRENDER'
import json, os
resp = json.loads(os.environ["RESP"])
out = open("/etc/wireguard/wgc0.conf", "w")
out.write(f"""[Interface]
PrivateKey = {os.environ["PRIV"]}
Address    = {resp['device_ip']}/32
ListenPort = {os.environ["WG_LISTEN"]}

""")
for p in resp.get("peers", []):
    extras = p.get("allowed_extra", [])
    aips = [p["wg_ip"] + "/32"] if p.get("wg_ip") else []
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
  "server":         os.environ["SERVER"],
  "device_id":      resp["device_id"],
  "token":          os.environ["TOKEN"],
  "token_expires":  resp.get("token_expires"),
  "wg_ip":          resp["device_ip"],
  "site_slug":      resp.get("site_slug", ""),
  "site_id":        resp.get("site_id", ""),
  "hub_slug":       resp.get("hub_slug", ""),
  "role":           resp.get("role", "device"),
  "mesh_cidr":      os.environ.get("MESH_CIDR", ""),
  "iface":          "wgc0",
  "wg_listen":      int(os.environ.get("WG_LISTEN", 51820)),
}
open("/etc/wgctl/config.json", "w").write(json.dumps(state, indent=2))
os.chmod("/etc/wgctl/config.json", 0o600)
PYRENDER

# --- 6.5 hub role: enable IP forwarding so this box relays ----
#     spoke-to-spoke and cross-hub traffic (multi-hub fabric).
ROLE=$(python3 -c 'import json; print(json.load(open("/etc/wgctl/config.json")).get("role", "device"))')
if [[ "$ROLE" == "hub" ]]; then
  echo "==> hub role: enabling IP forwarding"
  if [[ "$OS_RAW" == "darwin" ]]; then
    sysctl -w net.inet.ip.forwarding=1 >/dev/null || true
    grep -q '^net.inet.ip.forwarding=1' /etc/sysctl.conf 2>/dev/null \
      || echo 'net.inet.ip.forwarding=1' >> /etc/sysctl.conf
  else
    sysctl -w net.ipv4.ip_forward=1 >/dev/null || true
    grep -q '^net.ipv4.ip_forward=1' /etc/sysctl.conf 2>/dev/null \
      || echo 'net.ipv4.ip_forward=1' >> /etc/sysctl.conf
  fi
fi

# --- 7. kickstart wgc0, then bring up wgctl-agent for periodic ----
#     heartbeat + peer-list refresh. Agent reads /etc/wgctl/config.json
#     and posts /v1/heartbeat + polls /v1/peers (or /v1/hub/peers) on
#     a fixed interval. Without it the device "looks offline" in admin.
launchctl bootout  system/com.wireguard.wg-mac.wgc0 2>/dev/null || true
launchctl enable   system/com.wireguard.wg-mac.wgc0 2>/dev/null || true
launchctl bootstrap system /Library/LaunchDaemons/com.wireguard.wg-mac.wgc0.plist 2>/dev/null || true
launchctl kickstart -k system/com.wireguard.wg-mac.wgc0 || true

if [ -f /Library/LaunchDaemons/com.wireguard.wgctl-agent.plist ]; then
    launchctl bootout  system/com.wireguard.wgctl-agent 2>/dev/null || true
    launchctl enable   system/com.wireguard.wgctl-agent 2>/dev/null || true
    launchctl bootstrap system /Library/LaunchDaemons/com.wireguard.wgctl-agent.plist 2>/dev/null || true
fi

WGIP=$(python3 -c 'import json; print(json.load(open("/etc/wgctl/config.json"))["wg_ip"])')
echo "==> done. wg_ip=$WGIP"
echo "    sudo wgctl show wgc0"
`

func renderWGInstallScript(in wgInstallScriptInput) (string, error) {
	if strings.TrimSpace(in.Server) == "" {
		return "", fmt.Errorf("server URL required")
	}
	if strings.TrimSpace(in.BundleVersion) == "" {
		in.BundleVersion = "latest"
	}
	if strings.TrimSpace(in.MeshCIDR) == "" {
		in.MeshCIDR = "100.64.0.0/10"
	}
	tpl, err := template.New("wg-install").Parse(wgInstallScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse install template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, in); err != nil {
		return "", fmt.Errorf("execute install template: %w", err)
	}
	out := strings.TrimRight(buf.String(), "\n") + "\n"
	return out, nil
}

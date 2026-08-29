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
# Usage: curl -sSL <server>/v1/install | sudo bash -s -- --token=<TOKEN> [--hostname=NAME] [--site=SLUG] [--isolated]
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
HOST_ID=""
ISOLATED=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token=*)    TOKEN="${1#*=}";;
    --hostname=*) HOSTNAME_OVERRIDE="${1#*=}";;
    --site=*)     SITE_SLUG="${1#*=}";;
    --host-id=*)  HOST_ID="${1#*=}";;
    --isolated)   ISOLATED=1;;   # hub-only peering (VM behind a guest-isolating host NAT)
    *) echo "unknown arg: $1" >&2; exit 1;;
  esac
  shift
done
[[ -n "$TOKEN" ]] || { echo "--token=<TOKEN> required"; exit 1; }

# host_id ties this wg device to its polar-hosts host row (wg<->hosts cross-link
# stamped at register). Auto-read from the local polar-agent config if not given.
if [[ -z "$HOST_ID" ]]; then
  for cfg in "$HOME/.polar/agent.toml" /var/root/.polar/agent.toml /Users/*/.polar/agent.toml /root/.polar/agent.toml; do
    [[ -r "$cfg" ]] || continue
    HOST_ID=$(sed -n 's/^[[:space:]]*host_id[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg" | head -1)
    [[ -n "$HOST_ID" ]] && break
  done
fi

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

# --- 2.5 JSON helpers (awk; no python) -----------------------------------
# python3 is not a safe dependency here. On macOS /usr/bin/python3 is a
# Command Line Tools *stub*: on a Mac without Xcode CLT it prints "No
# developer tools were found" and exits non-zero, so this installer could not
# run there at all. Minimal Linux/FreeBSD images ship no python either.
# Everything below is base-system awk/sed/tr, POSIX features only, so gawk,
# mawk and one-true-awk all work.
#
#   jflat FILE      -> one "path<TAB>scalar" line per JSON leaf, e.g.
#                        /device_ip                10.88.0.20
#                        /peers/0/allowed_extra/0  192.168.11.0/24
#   jget  FLAT PATH -> value at PATH ("" when absent or JSON null)
#   json_esc STR    -> STR escaped for use inside a JSON string literal
#
# Strings are unescaped (including surrogate pairs); a control character
# inside a value becomes a space, so one scalar is always exactly one line.
# Object keys are assumed free of "/" — true for every field the CP sends.
JSON_AWK='
function ws(   ch) { while (i <= n) { ch = substr(s,i,1)
        if (ch==" "||ch=="\t"||ch=="\n"||ch=="\r") i++; else return } }
function hex4(h,   v, k, d, hx) { hx = "0123456789abcdef"; v = 0
    for (k = 1; k <= 4; k++) { d = index(hx, tolower(substr(h,k,1))) - 1
        if (d < 0) d = 0; v = v*16 + d }
    return v }
function utf8(v) { if (v < 32) return " "
    if (v < 128)   return sprintf("%c", v)
    if (v < 2048)  return sprintf("%c%c", 192+int(v/64), 128+v%64)
    if (v < 65536) return sprintf("%c%c%c", 224+int(v/4096), 128+int(v/64)%64, 128+v%64)
    return sprintf("%c%c%c%c", 240+int(v/262144), 128+int(v/4096)%64,
                   128+int(v/64)%64, 128+v%64) }
function pstr(   out, ch, e, u, lo) { i++; out = ""
    while (i <= n) { ch = substr(s,i,1)
        if (ch == "\\") { i++; e = substr(s,i,1)
            if (e == "u") { u = hex4(substr(s,i+1,4)); i += 4
                if (u >= 55296 && u <= 56319 && substr(s,i+1,2) == "\\u") {
                    lo = hex4(substr(s,i+3,4))
                    if (lo >= 56320 && lo <= 57343) {
                        u = 65536 + (u-55296)*1024 + (lo-56320); i += 6 } }
                out = out utf8(u) }
            else if (e=="n"||e=="t"||e=="r"||e=="b"||e=="f") out = out " "
            else out = out e
            i++ }
        else if (ch == "\"") { i++; return out }
        else { if (ch=="\n"||ch=="\t") ch = " "; out = out ch; i++ } }
    return out }
function pval(path,   ch, key, idx, out) { ws(); ch = substr(s,i,1)
    if (ch == "{") { i++; ws()
        if (substr(s,i,1) == "}") { i++; return }
        while (i <= n) { ws(); key = pstr(); ws(); i++
            pval(path "/" key)
            ws(); ch = substr(s,i,1); i++
            if (ch == "}") return }
        return }
    if (ch == "[") { i++; ws()
        if (substr(s,i,1) == "]") { i++; return }
        idx = 0
        while (i <= n) { pval(path "/" idx); idx++
            ws(); ch = substr(s,i,1); i++
            if (ch == "]") return }
        return }
    if (ch == "\"") { print path "\t" pstr(); return }
    out = ""
    while (i <= n) { ch = substr(s,i,1)
        if (index(" \t\r\n,}]", ch) > 0) break
        out = out ch; i++ }
    print path "\t" out }
{ s = s $0 "\n" }
END { n = length(s); i = 1; pval("") }
'
jflat() { LC_ALL=C awk "$JSON_AWK" "$1"; }
jget()  { LC_ALL=C awk -F'\t' -v p="$2" '$1 == p { if ($2 != "null") print $2; exit }' "$1"; }
json_esc() { printf '%s' "$1" | tr -d '[:cntrl:]' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }
# A JSON string literal, or bare null when empty (what json.dumps(None) wrote).
json_str_or_null() {
  if [[ -n "$1" ]]; then printf '"%s"' "$(json_esc "$1")"; else printf 'null'; fi
}

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

# lan_addrs, same source and filters as before: /sbin/ifconfig lines carrying
# a hex netmask (macOS/FreeBSD), minus loopback and link-local. Linux
# net-tools prints a dotted netmask and most Linux images have no ifconfig at
# all, so Linux reports [] here — unchanged from the python this replaced.
LAN_JSON=$( (/sbin/ifconfig 2>/dev/null || true) | LC_ALL=C awk '
    BEGIN { split("0 1 1 2 1 2 2 3 1 2 2 3 2 3 3 4", pc); hex = "0123456789abcdef" }
    /^[a-z0-9]+:/ { iface = substr($1, 1, length($1) - 1); next }
    $1 == "inet" && $3 == "netmask" {
        ip = $2; mask = $4
        if (ip !~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) next
        if (ip ~ /^127\./ || ip ~ /^169\.254\./) next
        if (mask !~ /^0[xX][0-9a-fA-F]+$/) next
        sub(/^0[xX]/, "", mask)
        bits = 0
        for (k = 1; k <= length(mask); k++)
            bits += pc[index(hex, tolower(substr(mask, k, 1)))]
        out = out (out == "" ? "" : ",") \
              "{\"iface\":\"" iface "\",\"cidr\":\"" ip "/" bits "\"}"
    }
    END { print "[" out "]" }')
[[ -n "$LAN_JSON" ]] || LAN_JSON='[]'

# --- 5. register ---
echo "==> registering with control plane"
HOST_ID=$(printf '%s' "$HOST_ID" | tr -d '[:cntrl:] ')
EXTRA_FIELDS=""
# host_id cross-links to polar-hosts; the server stamps wg_devices.host_id.
if [[ -n "$HOST_ID" ]]; then
  EXTRA_FIELDS="$EXTRA_FIELDS$(printf ',"host_id":"%s"' "$(json_esc "$HOST_ID")")"
fi
# isolated: hub-only peering, no LAN-direct peers offered or received.
if [[ -n "$ISOLATED" ]]; then
  EXTRA_FIELDS="$EXTRA_FIELDS,\"isolated\":true"
fi
REGISTER_BODY=$(printf '{"token":"%s","pubkey":"%s","hostname":"%s","os":"%s","arch":"%s","agent_ver":"%s","lan_addrs":%s,"wg_listen":%d,"site_slug":"%s"%s}' \
  "$(json_esc "$TOKEN")" "$(json_esc "$PUB")" "$(json_esc "$HOSTNAME_REPORT")" \
  "$(json_esc "$OS_RAW")" "$(json_esc "$ARCH")" "$(json_esc "$AGENT_VER")" \
  "$LAN_JSON" "$WG_LISTEN" "$(json_esc "$SITE_SLUG")" "$EXTRA_FIELDS")
RESP=$(curl -fsSL -X POST "$SERVER/v1/register" \
    -H 'Content-Type: application/json' \
    -d "$REGISTER_BODY")

# --- 6. render wgc0.conf + state ---
printf '%s\n' "$RESP" > "$TMP/register.json"
jflat "$TMP/register.json" > "$TMP/register.flat"

DEVICE_IP=$(jget "$TMP/register.flat" /device_ip)
DEVICE_ID=$(jget "$TMP/register.flat" /device_id)
if [[ -z "$DEVICE_IP" || -z "$DEVICE_ID" ]]; then
  echo "register response missing device_ip/device_id:" >&2
  head -c 500 "$TMP/register.json" >&2; echo >&2
  exit 1
fi
KEEPALIVE=$(jget "$TMP/register.flat" /keepalive_sec)
[[ -n "$KEEPALIVE" ]] || KEEPALIVE=25
ROLE=$(jget "$TMP/register.flat" /role)
[[ -n "$ROLE" ]] || ROLE=device

# Peer stanzas in one awk pass over the flattened response. Same layout rules
# as wgctl-agent's re-render (blank line before each [Peer], no Endpoint line
# when the server sent none, peer skipped when it has no AllowedIPs) so the
# agent's first tick does not rewrite this file and restart the tunnel.
umask 077
{
  echo "[Interface]"
  echo "PrivateKey = $PRIV"
  echo "Address    = $DEVICE_IP/32"
  echo "ListenPort = $WG_LISTEN"
  LC_ALL=C awk -F'\t' -v ka="$KEEPALIVE" '
    function idx_of(path,   a) { split(path, a, "/"); return a[3] + 0 }
    $1 ~ /^\/peers\/[0-9]+\/pubkey$/   { k = idx_of($1); pub[k] = $2
                                         if (k + 1 > np) np = k + 1; next }
    $1 ~ /^\/peers\/[0-9]+\/endpoint$/ { k = idx_of($1); ep[k] = $2
                                         if (k + 1 > np) np = k + 1; next }
    $1 ~ /^\/peers\/[0-9]+\/wg_ip$/    { k = idx_of($1); wgip[k] = $2
                                         if (k + 1 > np) np = k + 1; next }
    $1 ~ /^\/peers\/[0-9]+\/allowed_extra\/[0-9]+$/ { k = idx_of($1)
        ex[k] = (ex[k] == "" ? $2 : ex[k] ", " $2)
        if (k + 1 > np) np = k + 1; next }
    END {
      for (k = 0; k < np; k++) {
        if (pub[k] == "") continue
        aips = (wgip[k] == "" ? "" : wgip[k] "/32")
        if (ex[k] != "") aips = (aips == "" ? ex[k] : aips ", " ex[k])
        if (aips == "") continue
        print ""
        print "[Peer]"
        print "PublicKey  = " pub[k]
        if (ep[k] != "") print "Endpoint   = " ep[k]
        print "AllowedIPs = " aips
        if (ka != "0") print "PersistentKeepalive = " ka
      }
    }' "$TMP/register.flat"
} > /etc/wireguard/wgc0.conf
chmod 0600 /etc/wireguard/wgc0.conf

cat > /etc/wgctl/config.json <<WGSTATE
{
  "server": "$(json_esc "$SERVER")",
  "device_id": "$(json_esc "$DEVICE_ID")",
  "token": "$(json_esc "$TOKEN")",
  "token_expires": $(json_str_or_null "$(jget "$TMP/register.flat" /token_expires)"),
  "wg_ip": "$(json_esc "$DEVICE_IP")",
  "site_slug": "$(json_esc "$(jget "$TMP/register.flat" /site_slug)")",
  "site_id": "$(json_esc "$(jget "$TMP/register.flat" /site_id)")",
  "hub_slug": "$(json_esc "$(jget "$TMP/register.flat" /hub_slug)")",
  "role": "$(json_esc "$ROLE")",
  "mesh_cidr": "$(json_esc "$MESH_CIDR")",
  "iface": "wgc0",
  "wg_listen": $WG_LISTEN
}
WGSTATE
chmod 0600 /etc/wgctl/config.json

# --- 6.5 hub role: enable IP forwarding so this box relays ----
#     spoke-to-spoke and cross-hub traffic (multi-hub fabric).
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

echo "==> done. wg_ip=$DEVICE_IP"
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

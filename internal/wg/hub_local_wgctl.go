package wg

// hub_local_wgctl.go — wgctl(8) flavor of the hub-local self-poll path.
//
// The wg-mac Network Extension creates a UNIX control socket at
// /var/run/wireguard/<iface>.sock but it doesn't speak the standard
// wireguard-tools cross-version protocol — `wg show <iface> dump`
// returns "Unable to access interface: Protocol error" on those hosts.
//
// The wg-mac installer ships a sibling CLI `wgctl` at /usr/local/bin
// that talks the right protocol. We prefer it when present, fall back
// to `wg` for kernel / userspace-wireguard hosts.
//
// wgctl's output is human-readable (no TSV / JSON / dump mode):
//
//	interface: utun0
//	  logical: wgc0
//	  peers: 3
//
//	peer #0: <base64-pubkey>
//	  endpoint: 1.2.3.4:51820
//	  allowed ips: 10.88.0.2/32
//	  latest handshake: 1 minutes, 22 seconds ago
//	  transfer: 121.25 MiB received, 3.69 MiB sent
//	  packets: rx=115991 tx=58832  rx_dropped_aips=0
//	  persistent keepalive: every 25 seconds
//	  handshake state: idle
//
// It does NOT print the *interface* pubkey, so we need that from the
// operator via POLAR_WG_HUB_LOCAL_POLL_PUBKEY (cache rows are keyed
// by iface_public_key in dock so the UI can join against wg_hubs.pubkey).

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	hubLocalWGCtlBinEnvOverride = "POLAR_WG_HUB_LOCAL_WGCTL_BIN"
	hubLocalWGCtlDefaultPaths   = "/usr/local/bin/wgctl:/opt/homebrew/bin/wgctl"
	hubLocalIfacePubKeyEnv      = "POLAR_WG_HUB_LOCAL_POLL_PUBKEY"
)

// resolveWGCtlBin finds the wgctl binary, env override wins.
// Returns "" when wgctl isn't on the box — caller falls back to `wg`.
func resolveWGCtlBin() string {
	if v := strings.TrimSpace(os.Getenv(hubLocalWGCtlBinEnvOverride)); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if p, err := exec.LookPath("wgctl"); err == nil {
		return p
	}
	for _, p := range strings.Split(hubLocalWGCtlDefaultPaths, ":") {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// hubLocalIfacePubKey is the operator-supplied iface public key. wgctl
// doesn't surface it; without this the cache entries can't be joined
// to wg_hubs.pubkey in the admin UI. Returns "" when unset — caller
// can still push samples (with empty pubkey) but they'll be useless.
func hubLocalIfacePubKey() string {
	return strings.TrimSpace(os.Getenv(hubLocalIfacePubKeyEnv))
}

// transferUnitToBytes — wgctl uses humanised units (KiB, MiB, GiB, …).
// Standard binary multipliers, NOT decimal.
var transferUnitToBytes = map[string]int64{
	"B":   1,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
}

// parseWGCtlBytes turns "121.25 MiB" into 127143936 (bytes).
// Returns (0, false) for "0 B" / malformed / unknown unit.
func parseWGCtlBytes(s string) (int64, bool) {
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) != 2 {
		return 0, false
	}
	mult, ok := transferUnitToBytes[parts[1]]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, false
	}
	return int64(f * float64(mult)), true
}

// handshakeAgeRe matches "1 minutes, 22 seconds ago" / "5 hours, 3 minutes ago"
// / "3 days, 4 hours ago" / "22 seconds ago" / "now" / "never".
// Group meanings depend on which units are present — see parseWGCtlHandshakeAge.
var (
	handshakeUnitRe = regexp.MustCompile(`(\d+)\s+(seconds?|minutes?|hours?|days?)`)
)

func parseWGCtlHandshakeAge(s string) (ageSec int64, ok bool) {
	s = strings.TrimSpace(strings.TrimSuffix(s, " ago"))
	if s == "" || s == "never" {
		return 0, false
	}
	if s == "now" {
		return 0, true
	}
	matches := handshakeUnitRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return 0, false
	}
	var total int64
	for _, m := range matches {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(m[2], "second"):
			total += n
		case strings.HasPrefix(m[2], "minute"):
			total += n * 60
		case strings.HasPrefix(m[2], "hour"):
			total += n * 3600
		case strings.HasPrefix(m[2], "day"):
			total += n * 86400
		}
	}
	return total, true
}

// keepaliveRe matches "every 25 seconds" / "off".
var keepaliveSecRe = regexp.MustCompile(`every\s+(\d+)\s+seconds?`)

func parseWGCtlKeepalive(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "off" || s == "(none)" {
		return 0
	}
	m := keepaliveSecRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// transferRe — "121.25 MiB received, 3.69 MiB sent". The numbers can
// have decimals; the units come from transferUnitToBytes.
var transferRe = regexp.MustCompile(`([0-9.]+\s+\w+)\s+received,\s+([0-9.]+\s+\w+)\s+sent`)

func parseWGCtlTransfer(s string) (rx, tx int64, ok bool) {
	m := transferRe.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) != 3 {
		return 0, 0, false
	}
	rxB, rxOK := parseWGCtlBytes(m[1])
	txB, txOK := parseWGCtlBytes(m[2])
	if !rxOK && !txOK {
		return 0, 0, false
	}
	return rxB, txB, true
}

// parseWGCtlShow parses `wgctl show <iface>` (or `wgctl show` for any
// iface). The iface_public_key argument is plumbed through from env
// because wgctl doesn't print it.
func parseWGCtlShow(iface, ifacePubKey, raw string) (parsedDump, error) {
	if strings.TrimSpace(raw) == "" {
		return parsedDump{}, fmt.Errorf("empty wgctl output")
	}
	lines := strings.Split(raw, "\n")
	now := time.Now().UTC()
	peers := make([]map[string]any, 0, 8)

	var (
		curPeer    map[string]any
		listenPort int // wgctl doesn't surface this; left as 0 for now
	)
	flush := func() {
		if curPeer != nil {
			peers = append(peers, curPeer)
			curPeer = nil
		}
	}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// peer header — "peer #N: <base64>"
		if strings.HasPrefix(line, "peer #") {
			flush()
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			curPeer = map[string]any{
				"public_key":  strings.TrimSpace(parts[1]),
				"endpoint":    "",
				"allowed_ips": "",
				"bytes_rx":    int64(0),
				"bytes_tx":    int64(0),
				// has_preshared_key isn't surfaced by wgctl in show
				// output; leave unset rather than guess.
			}
			continue
		}
		if curPeer == nil {
			continue
		}

		switch {
		case strings.HasPrefix(line, "endpoint:"):
			ep := strings.TrimSpace(strings.TrimPrefix(line, "endpoint:"))
			if ep == "(none)" {
				ep = ""
			}
			curPeer["endpoint"] = ep
		case strings.HasPrefix(line, "allowed ips:"):
			curPeer["allowed_ips"] = strings.TrimSpace(strings.TrimPrefix(line, "allowed ips:"))
		case strings.HasPrefix(line, "latest handshake:"):
			body := strings.TrimSpace(strings.TrimPrefix(line, "latest handshake:"))
			if age, ok := parseWGCtlHandshakeAge(body); ok {
				curPeer["handshake_age_sec"] = age
				curPeer["latest_handshake_unix"] = now.Unix() - age
			}
		case strings.HasPrefix(line, "transfer:"):
			body := strings.TrimPrefix(line, "transfer:")
			if rx, tx, ok := parseWGCtlTransfer(body); ok {
				curPeer["bytes_rx"] = rx
				curPeer["bytes_tx"] = tx
			}
		case strings.HasPrefix(line, "persistent keepalive:"):
			body := strings.TrimSpace(strings.TrimPrefix(line, "persistent keepalive:"))
			curPeer["keepalive_sec"] = parseWGCtlKeepalive(body)
		}
	}
	flush()

	return parsedDump{
		IfacePublicKey: ifacePubKey,
		Data: map[string]any{
			"kind":             "wg_peer_status",
			"iface":            iface,
			"iface_public_key": ifacePubKey,
			"listen_port":      listenPort,
			"fwmark":           "",
			"peer_count":       len(peers),
			"sampled_at":       now.Format("2006-01-02T15:04:05.000Z"),
			"peers":            peers,
		},
	}, nil
}

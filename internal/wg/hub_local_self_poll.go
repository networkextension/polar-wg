package wg

// hub_local_self_poll.go — when polar-wg-svc runs on a wireguard hub
// box, it can observe the local wg iface directly via the userspace
// control socket (`wg show <iface> dump`) and push samples to dock,
// bypassing the polar-agent skill pipeline.
//
// Activation: set POLAR_WG_HUB_LOCAL_POLL_IFACE=<iface> (e.g. wgc0).
// Cadence: POLAR_WG_HUB_LOCAL_POLL_SEC (default 30, clamped [5,300]).
//
// Why this exists: polar-agent's wireguard skill is designed for
// CLIENTS joining a mesh (it does `wg-quick up` against a provided
// config). Hubs that came up via macOS Network Extension or any
// other mechanism don't fit that flow. polar-wg-svc is already
// running on those boxes — let it be the sample source.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	hubLocalPollIfaceEnv      = "POLAR_WG_HUB_LOCAL_POLL_IFACE"
	hubLocalPollSecEnv        = "POLAR_WG_HUB_LOCAL_POLL_SEC"
	hubLocalPollSecDefault    = 30
	hubLocalPollSecMin        = 5
	hubLocalPollSecMax        = 300
	hubLocalPushPath          = "/internal/v1/wg-peer-status"
	hubLocalWGBinEnvOverride  = "POLAR_WG_HUB_LOCAL_WG_BIN" // for tests; "" => look up in PATH
	hubLocalWGBinDefaultPaths = "/opt/homebrew/bin/wg:/usr/local/bin/wg:/usr/bin/wg"
)

// hubLocalBackend describes which CLI we shell out to + which parser
// to feed its output through. Both flavors emit the same parsedDump
// shape so the rest of the poll loop doesn't care.
type hubLocalBackend struct {
	name      string // "wgctl" or "wg" — purely for log lines
	bin       string // resolved absolute path
	usesSudo  bool   // wgctl on macOS needs sudo to read /var/run/wireguard/<iface>.sock
	parser    func(iface, ifacePubKey, raw string) (parsedDump, error)
	cmdArgs   func(iface string) []string
	pubKeyEnv string // operator-supplied pubkey (wgctl can't print it)
}

// pickHubLocalBackend prefers wgctl (wg-mac NE-backed hubs), falls
// back to wg(8) (kernel / wireguard-go hubs). Returns nil when neither
// is on the box.
func pickHubLocalBackend() *hubLocalBackend {
	if bin := resolveWGCtlBin(); bin != "" {
		return &hubLocalBackend{
			name:     "wgctl",
			bin:      bin,
			usesSudo: true,
			parser:   parseWGCtlShow,
			cmdArgs:  func(iface string) []string { return []string{"show", iface} },
		}
	}
	if bin := resolveWGBin(); bin != "" {
		return &hubLocalBackend{
			name:     "wg",
			bin:      bin,
			usesSudo: false,
			parser:   func(iface, _ string, raw string) (parsedDump, error) { return parseWGShowDump(iface, raw) },
			cmdArgs:  func(iface string) []string { return []string{"show", iface, "dump"} },
		}
	}
	return nil
}

// startHubLocalSelfPoll wires the self-poll goroutine when configured.
// No-op when POLAR_WG_HUB_LOCAL_POLL_IFACE is unset — silent for
// non-hub plugin deployments. Logs once at INFO when activating.
func (p *Plugin) startHubLocalSelfPoll(ctx context.Context) {
	iface := strings.TrimSpace(os.Getenv(hubLocalPollIfaceEnv))
	if iface == "" {
		return
	}
	backend := pickHubLocalBackend()
	if backend == nil {
		log.Printf("wg: hub-local self-poll: requested iface=%s but neither `wgctl` (wg-mac NE) nor `wg` (wireguard-tools) found on PATH",
			iface)
		return
	}
	ifacePubKey := hubLocalIfacePubKey()
	if backend.name == "wgctl" && ifacePubKey == "" {
		log.Printf("wg: hub-local self-poll: %s backend can't surface the interface public key — set %s=<base64> so cache rows can be joined against wg_hubs.pubkey",
			backend.name, hubLocalIfacePubKeyEnv)
		return
	}
	interval := hubLocalPollInterval()
	log.Printf("wg: hub-local self-poll: iface=%s backend=%s bin=%s interval=%v sudo=%v (pushing to %s)",
		iface, backend.name, backend.bin, interval, backend.usesSudo, hubLocalPushPath)
	go p.hubLocalSelfPollLoop(ctx, iface, ifacePubKey, backend, interval)
}

func (p *Plugin) hubLocalSelfPollLoop(ctx context.Context, iface, ifacePubKey string, backend *hubLocalBackend, interval time.Duration) {
	// One immediate run so the cache populates before the first ticker.
	p.hubLocalSelfPollOnce(ctx, iface, ifacePubKey, backend)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.hubLocalSelfPollOnce(ctx, iface, ifacePubKey, backend)
		}
	}
}

func (p *Plugin) hubLocalSelfPollOnce(ctx context.Context, iface, ifacePubKey string, backend *hubLocalBackend) {
	dumpCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if backend.usesSudo {
		args := append([]string{"-n", backend.bin}, backend.cmdArgs(iface)...)
		cmd = exec.CommandContext(dumpCtx, "sudo", args...)
	} else {
		cmd = exec.CommandContext(dumpCtx, backend.bin, backend.cmdArgs(iface)...)
	}
	out, err := cmd.Output()
	if err != nil {
		log.Printf("wg: hub-local self-poll: %s exec failed: %v", backend.name, err)
		return
	}
	sample, err := backend.parser(iface, ifacePubKey, string(out))
	if err != nil {
		log.Printf("wg: hub-local self-poll: %s parse failed: %v", backend.name, err)
		return
	}
	body := map[string]any{
		"iface":            iface,
		"iface_public_key": sample.IfacePublicKey,
		"data":             sample.Data,
	}
	resp, err := p.Dock.Do(http.MethodPost, hubLocalPushPath, body)
	if err != nil {
		log.Printf("wg: hub-local self-poll: push failed: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("wg: hub-local self-poll: dock HTTP %d on push", resp.StatusCode)
	}
}

func hubLocalPollInterval() time.Duration {
	v := os.Getenv(hubLocalPollSecEnv)
	if v == "" {
		return hubLocalPollSecDefault * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < hubLocalPollSecMin {
		return hubLocalPollSecDefault * time.Second
	}
	if n > hubLocalPollSecMax {
		n = hubLocalPollSecMax
	}
	return time.Duration(n) * time.Second
}

// resolveWGBin finds the `wg` userspace tool. POLAR_WG_HUB_LOCAL_WG_BIN
// wins for tests + non-standard layouts; otherwise PATH lookup, then
// the common homebrew / /usr/local / /usr/bin fallbacks.
func resolveWGBin() string {
	if v := strings.TrimSpace(os.Getenv(hubLocalWGBinEnvOverride)); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if p, err := exec.LookPath("wg"); err == nil {
		return p
	}
	for _, p := range strings.Split(hubLocalWGBinDefaultPaths, ":") {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// parsedDump is one (host_id, iface) sample built from `wg show dump`.
type parsedDump struct {
	IfacePublicKey string
	Data           map[string]any
}

// parseWGShowDump turns `wg show <iface> dump` output into the same
// JSON shape the polar-agent wireguard skill emits.
//
// Output format (TSV):
//
//	line 1 — interface row: <private-key> <public-key> <listen-port> <fwmark>
//	line 2+ — peer rows: <public-key> <preshared-key> <endpoint>
//	                     <allowed-ips> <latest-handshake-unix>
//	                     <rx-bytes> <tx-bytes> <persistent-keepalive>
//
// "(none)" / "0" are sentinel values per wg(8). We map them to the
// shape downstream (UI + dock cache) already understands.
func parseWGShowDump(iface, raw string) (parsedDump, error) {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return parsedDump{}, fmt.Errorf("empty wg dump output")
	}
	ifaceFields := strings.Split(lines[0], "\t")
	if len(ifaceFields) < 4 {
		return parsedDump{}, fmt.Errorf("interface row has %d fields, want >=4", len(ifaceFields))
	}
	ifacePubKey := ifaceFields[1]
	listenPort, _ := strconv.Atoi(ifaceFields[2])
	fwmark := ifaceFields[3]
	if fwmark == "off" {
		fwmark = ""
	}

	now := time.Now().UTC()
	peers := make([]map[string]any, 0, len(lines)-1)
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		// 8 fields per wg(8) dump format. Be permissive: skip rows
		// that look malformed rather than failing the whole sample.
		if len(f) < 8 {
			continue
		}
		latestHandshakeUnix, _ := strconv.ParseInt(f[4], 10, 64)
		rx, _ := strconv.ParseInt(f[5], 10, 64)
		tx, _ := strconv.ParseInt(f[6], 10, 64)
		keepalive := 0
		if f[7] != "off" {
			keepalive, _ = strconv.Atoi(f[7])
		}
		endpoint := f[2]
		if endpoint == "(none)" {
			endpoint = ""
		}
		hasPreshared := f[1] != "(none)" && f[1] != ""
		peer := map[string]any{
			"public_key":            f[0],
			"has_preshared_key":     hasPreshared,
			"endpoint":              endpoint,
			"allowed_ips":           f[3],
			"latest_handshake_unix": latestHandshakeUnix,
			"bytes_rx":              rx,
			"bytes_tx":              tx,
			"keepalive_sec":         keepalive,
		}
		if latestHandshakeUnix > 0 {
			ageSec := now.Unix() - latestHandshakeUnix
			if ageSec < 0 {
				ageSec = 0
			}
			peer["handshake_age_sec"] = ageSec
		}
		peers = append(peers, peer)
	}

	return parsedDump{
		IfacePublicKey: ifacePubKey,
		Data: map[string]any{
			"kind":             "wg_peer_status",
			"iface":            iface,
			"iface_public_key": ifacePubKey,
			"listen_port":      listenPort,
			"fwmark":           fwmark,
			"peer_count":       len(peers),
			"sampled_at":       now.Format("2006-01-02T15:04:05.000Z"),
			"peers":            peers,
		},
	}, nil
}

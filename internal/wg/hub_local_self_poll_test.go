package wg

import (
	"strings"
	"testing"
	"time"
)

// Real wg(8) dump format — interface row + 2 peer rows. Tab-separated.
const sampleWGDump = "PRIV-KEY\tIFACE-PUB-KEY\t51820\toff\n" +
	"PEER1-PUB\t(none)\t1.2.3.4:51820\t10.0.0.2/32\t1716705421\t4096\t8192\t25\n" +
	"PEER2-PUB\tPSK-OK\t(none)\t10.0.0.3/32\t0\t0\t0\toff\n"

func TestParseWGShowDump_HappyPath(t *testing.T) {
	got, err := parseWGShowDump("wgc0", sampleWGDump)
	if err != nil {
		t.Fatalf("parseWGShowDump returned error: %v", err)
	}
	if got.IfacePublicKey != "IFACE-PUB-KEY" {
		t.Fatalf("iface_public_key = %q, want IFACE-PUB-KEY", got.IfacePublicKey)
	}
	if k, _ := got.Data["kind"].(string); k != "wg_peer_status" {
		t.Fatalf("data.kind = %q, want wg_peer_status", k)
	}
	if pc, _ := got.Data["peer_count"].(int); pc != 2 {
		t.Fatalf("data.peer_count = %d, want 2", pc)
	}
	if lp, _ := got.Data["listen_port"].(int); lp != 51820 {
		t.Fatalf("data.listen_port = %d, want 51820", lp)
	}
	if fw, _ := got.Data["fwmark"].(string); fw != "" {
		t.Fatalf("data.fwmark = %q, want empty (off → '')", fw)
	}
	peers, _ := got.Data["peers"].([]map[string]any)
	if len(peers) != 2 {
		t.Fatalf("peers len = %d, want 2", len(peers))
	}
}

func TestParseWGShowDump_PeerFields(t *testing.T) {
	got, err := parseWGShowDump("wgc0", sampleWGDump)
	if err != nil {
		t.Fatal(err)
	}
	peers := got.Data["peers"].([]map[string]any)
	p0 := peers[0]
	if p0["public_key"] != "PEER1-PUB" {
		t.Errorf("peer0.public_key = %v, want PEER1-PUB", p0["public_key"])
	}
	if p0["endpoint"] != "1.2.3.4:51820" {
		t.Errorf("peer0.endpoint = %v", p0["endpoint"])
	}
	if p0["has_preshared_key"] != false {
		t.Errorf("peer0.has_preshared_key = %v, want false (psk=(none))", p0["has_preshared_key"])
	}
	if rx, _ := p0["bytes_rx"].(int64); rx != 4096 {
		t.Errorf("peer0.bytes_rx = %d, want 4096", rx)
	}
	if k, _ := p0["keepalive_sec"].(int); k != 25 {
		t.Errorf("peer0.keepalive_sec = %d, want 25", k)
	}
	if _, ok := p0["handshake_age_sec"]; !ok {
		t.Errorf("peer0.handshake_age_sec missing — set when latest_handshake_unix > 0")
	}

	p1 := peers[1]
	if p1["has_preshared_key"] != true {
		t.Errorf("peer1.has_preshared_key = %v, want true (psk=PSK-OK)", p1["has_preshared_key"])
	}
	if p1["endpoint"] != "" {
		t.Errorf("peer1.endpoint = %q, want empty ((none) → '')", p1["endpoint"])
	}
	if k, _ := p1["keepalive_sec"].(int); k != 0 {
		t.Errorf("peer1.keepalive_sec = %d, want 0 (off → 0)", k)
	}
	if _, ok := p1["handshake_age_sec"]; ok {
		t.Errorf("peer1.handshake_age_sec should be absent when latest_handshake_unix==0")
	}
}

func TestParseWGShowDump_FwmarkPreserved(t *testing.T) {
	dump := "PRIV\tPUB\t51820\t0x1234\n"
	got, err := parseWGShowDump("wgc0", dump)
	if err != nil {
		t.Fatal(err)
	}
	if fw, _ := got.Data["fwmark"].(string); fw != "0x1234" {
		t.Errorf("fwmark = %q, want 0x1234", fw)
	}
}

func TestParseWGShowDump_EmptyError(t *testing.T) {
	if _, err := parseWGShowDump("wgc0", ""); err == nil {
		t.Fatal("empty input should error")
	}
	if _, err := parseWGShowDump("wgc0", "\n\n"); err == nil {
		t.Fatal("whitespace input should error")
	}
}

func TestParseWGShowDump_SkipsMalformedPeerRow(t *testing.T) {
	dump := "PRIV\tIFACE-PUB\t51820\toff\n" +
		"PEER-OK\t(none)\t1.1.1.1:51820\t10.0.0.2/32\t1000\t0\t0\toff\n" +
		"too\tfew\tfields\n" + // malformed — should be skipped, not abort
		"PEER-OK2\t(none)\t2.2.2.2:51820\t10.0.0.3/32\t2000\t0\t0\toff\n"
	got, err := parseWGShowDump("wgc0", dump)
	if err != nil {
		t.Fatal(err)
	}
	peers := got.Data["peers"].([]map[string]any)
	if len(peers) != 2 {
		t.Fatalf("peers len = %d, want 2 (malformed row skipped, valid ones kept)", len(peers))
	}
}

func TestHubLocalPollInterval_EnvClamp(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset → 30s default", "", 30 * time.Second},
		{"valid 60s", "60", 60 * time.Second},
		{"below min → default", "1", 30 * time.Second},
		{"above max → clamped 300s", "9999", 300 * time.Second},
		{"garbage → default", "xxx", 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hubLocalPollSecEnv, tc.env)
			if got := hubLocalPollInterval(); got != tc.want {
				t.Errorf("hubLocalPollInterval(%q)=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestParseWGShowDump_HandshakeAgeMonotonic(t *testing.T) {
	// Sample a known recent handshake; age should be > 0 and < 24h.
	recentTS := time.Now().Add(-2 * time.Minute).Unix()
	dump := "PRIV\tIFACE\t51820\toff\nPEER\t(none)\t1.1.1.1:51820\t10.0.0.2/32\t" +
		strings.TrimSpace(itoa(recentTS)) + "\t0\t0\toff\n"
	got, err := parseWGShowDump("wgc0", dump)
	if err != nil {
		t.Fatal(err)
	}
	peers := got.Data["peers"].([]map[string]any)
	age, _ := peers[0]["handshake_age_sec"].(int64)
	if age < 100 || age > 200 {
		t.Errorf("handshake_age_sec=%d, want ~120 (sample was 2 min ago)", age)
	}
}

// itoa avoids strconv import cycle in test file (parser already imports it).
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

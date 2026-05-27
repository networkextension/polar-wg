package wg

import (
	"testing"
	"time"
)

const sampleWGCtlShow = `interface: utun0
  logical: wgc0
  peers: 3

peer #0: zXjio6QR5F1nmCy3SoiiGYC1j1I1ofHh9DGzWU6pTw0=
  endpoint: 180.171.124.101:3534
  allowed ips: 10.88.0.2/32
  latest handshake: 1 minutes, 22 seconds ago
  transfer: 121.25 MiB received, 3.69 MiB sent
  packets: rx=115991 tx=58832  rx_dropped_aips=0
  persistent keepalive: every 25 seconds
  handshake state: idle

peer #1: 4UhkQLXSbNhedxsStru0nQQu4k/EMXlKN8hwNe7bwR0=
  endpoint: (none)
  allowed ips: 10.88.0.3/32
  latest handshake: never
  transfer: 0 B received, 0 B sent
  packets: rx=0 tx=0  rx_dropped_aips=0
  persistent keepalive: every 25 seconds
  handshake state: idle

peer #2: JlcDSdk3aAx7BU1QEgvZf2mRnI8vB54klJtibJp8MHs=
  endpoint: 58.37.118.182:46663
  allowed ips: 10.88.0.4/32
  latest handshake: 1 minutes, 37 seconds ago
  transfer: 278.89 KiB received, 69.41 KiB sent
  packets: rx=1323 tx=1145  rx_dropped_aips=0
  persistent keepalive: every 25 seconds
  handshake state: idle
`

func TestParseWGCtlShow_HappyPath(t *testing.T) {
	got, err := parseWGCtlShow("wgc0", "IFACE-PUB", sampleWGCtlShow)
	if err != nil {
		t.Fatalf("parseWGCtlShow: %v", err)
	}
	if got.IfacePublicKey != "IFACE-PUB" {
		t.Fatalf("IfacePublicKey = %q, want IFACE-PUB", got.IfacePublicKey)
	}
	if pc, _ := got.Data["peer_count"].(int); pc != 3 {
		t.Fatalf("peer_count = %d, want 3", pc)
	}
	peers := got.Data["peers"].([]map[string]any)
	if len(peers) != 3 {
		t.Fatalf("peers len = %d, want 3", len(peers))
	}
}

func TestParseWGCtlShow_PeerFields(t *testing.T) {
	got, _ := parseWGCtlShow("wgc0", "IFACE-PUB", sampleWGCtlShow)
	peers := got.Data["peers"].([]map[string]any)

	p0 := peers[0]
	if p0["public_key"] != "zXjio6QR5F1nmCy3SoiiGYC1j1I1ofHh9DGzWU6pTw0=" {
		t.Errorf("p0.public_key wrong: %v", p0["public_key"])
	}
	if p0["endpoint"] != "180.171.124.101:3534" {
		t.Errorf("p0.endpoint = %v", p0["endpoint"])
	}
	if p0["allowed_ips"] != "10.88.0.2/32" {
		t.Errorf("p0.allowed_ips = %v", p0["allowed_ips"])
	}
	if age, _ := p0["handshake_age_sec"].(int64); age != 82 { // 1m22s
		t.Errorf("p0.handshake_age_sec = %d, want 82", age)
	}
	if rx, _ := p0["bytes_rx"].(int64); rx != int64(127139840) { // 121.25 MiB = 121.25 * 1<<20
		t.Errorf("p0.bytes_rx = %d, want 127139840 (121.25 MiB)", rx)
	}
	if tx, _ := p0["bytes_tx"].(int64); tx != int64(3869245) { // 3.69 MiB = 3.69 * 1<<20 (truncated)
		t.Errorf("p0.bytes_tx = %d, want 3869245 (3.69 MiB)", tx)
	}
	if k, _ := p0["keepalive_sec"].(int); k != 25 {
		t.Errorf("p0.keepalive_sec = %d, want 25", k)
	}

	p1 := peers[1]
	if p1["endpoint"] != "" {
		t.Errorf("p1.endpoint should be empty for (none), got %q", p1["endpoint"])
	}
	if _, ok := p1["handshake_age_sec"]; ok {
		t.Errorf("p1.handshake_age_sec should be absent for 'never'")
	}
	if rx, _ := p1["bytes_rx"].(int64); rx != 0 {
		t.Errorf("p1.bytes_rx = %d, want 0", rx)
	}

	p2 := peers[2]
	if age, _ := p2["handshake_age_sec"].(int64); age != 97 { // 1m37s
		t.Errorf("p2.handshake_age_sec = %d, want 97", age)
	}
}

func TestParseWGCtlHandshakeAge_Variants(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"22 seconds ago", 22, true},
		{"1 minutes, 22 seconds ago", 82, true},
		{"5 minutes ago", 300, true},
		{"5 hours, 3 minutes ago", 18180, true},
		{"3 days, 4 hours ago", 273600, true},
		{"1 hour ago", 3600, true},
		{"never", 0, false},
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseWGCtlHandshakeAge(tc.in)
			if ok != tc.ok {
				t.Errorf("ok=%v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseWGCtlBytes_Units(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0 B", 0, true},
		{"1024 B", 1024, true},
		{"1 KiB", 1024, true},
		{"121.25 MiB", 127139840, true},
		{"3.69 MiB", 3869245, true},
		{"2.5 GiB", 2684354560, true},
		{"1 TiB", 1 << 40, true},
		{"5 PB", 0, false}, // unknown unit
		{"abc", 0, false},  // malformed
		{"5", 0, false},    // missing unit
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseWGCtlBytes(tc.in)
			if ok != tc.ok {
				t.Errorf("ok=%v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseWGCtlKeepalive(t *testing.T) {
	cases := map[string]int{
		"every 25 seconds": 25,
		"every 60 seconds": 60,
		"off":              0,
		"":                 0,
		"(none)":           0,
		"garbage":          0,
	}
	for in, want := range cases {
		if got := parseWGCtlKeepalive(in); got != want {
			t.Errorf("parseWGCtlKeepalive(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseWGCtlTransfer_Both(t *testing.T) {
	rx, tx, ok := parseWGCtlTransfer("121.25 MiB received, 3.69 MiB sent")
	if !ok {
		t.Fatal("expected ok=true for valid transfer line")
	}
	if rx != 127139840 || tx != 3869245 {
		t.Errorf("rx/tx = %d/%d, want 127139840/3869245", rx, tx)
	}
}

func TestParseWGCtlShow_RecordedAtMonotonic(t *testing.T) {
	got, _ := parseWGCtlShow("wgc0", "PK", sampleWGCtlShow)
	sampledAt, _ := got.Data["sampled_at"].(string)
	if sampledAt == "" {
		t.Fatal("sampled_at missing")
	}
	ts, err := time.Parse("2006-01-02T15:04:05.000Z", sampledAt)
	if err != nil {
		t.Fatalf("sampled_at parse: %v", err)
	}
	if d := time.Since(ts); d < 0 || d > 5*time.Second {
		t.Errorf("sampled_at too far from now: %v", d)
	}
}

func TestParseWGCtlShow_EmptyError(t *testing.T) {
	if _, err := parseWGCtlShow("wgc0", "PK", ""); err == nil {
		t.Fatal("empty input should error")
	}
}

func TestParseWGCtlShow_InterfaceOnlyNoPeers(t *testing.T) {
	in := "interface: utun0\n  logical: wgc0\n  peers: 0\n"
	got, err := parseWGCtlShow("wgc0", "PK", in)
	if err != nil {
		t.Fatalf("interface-only block should parse, got %v", err)
	}
	if pc, _ := got.Data["peer_count"].(int); pc != 0 {
		t.Errorf("peer_count = %d, want 0", pc)
	}
	if peers, _ := got.Data["peers"].([]map[string]any); len(peers) != 0 {
		t.Errorf("peers should be empty slice, got %d", len(peers))
	}
}

package wg

import (
	"strings"
	"testing"
)

func TestRenderWGInstallScript(t *testing.T) {
	s, err := renderWGInstallScript(wgInstallScriptInput{
		Server:        "https://zen.4950.store",
		BundleVersion: "20260517-abc12345",
		MeshCIDR:      "10.88.0.0/16",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	wants := []string{
		"#!/bin/bash",
		"SERVER='https://zen.4950.store'",
		"BUNDLE_VERSION='20260517-abc12345'",
		"MESH_CIDR='10.88.0.0/16'",
		"/v1/register",
		"/v1/bundle/$BUNDLE_VERSION",
		"/usr/local/bin/wgctl genkey",
		"launchctl kickstart",
		// hub role enables forwarding so the box relays spoke-to-spoke
		// and cross-hub traffic (multi-hub fabric).
		"net.inet.ip.forwarding=1",
		"net.ipv4.ip_forward=1",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("rendered script missing %q", w)
		}
	}
	// Defaults applied when caller passes empty MeshCIDR / version.
	s2, err := renderWGInstallScript(wgInstallScriptInput{Server: "http://localhost:8089"})
	if err != nil {
		t.Fatalf("render defaults: %v", err)
	}
	if !strings.Contains(s2, "BUNDLE_VERSION='latest'") {
		t.Errorf("default BUNDLE_VERSION not applied")
	}
	if !strings.Contains(s2, "MESH_CIDR='100.64.0.0/10'") {
		t.Errorf("default MESH_CIDR not applied (expected RFC 6598 CGNAT range)")
	}
	// Empty server → error.
	if _, err := renderWGInstallScript(wgInstallScriptInput{Server: ""}); err == nil {
		t.Errorf("expected error on empty server URL")
	}
}

func TestExtractPublicIP(t *testing.T) {
	cases := []struct {
		name             string
		xff, xrip, raddr string
		want             string
	}{
		{"xff wins", "1.2.3.4, 5.6.7.8", "9.9.9.9", "10.0.0.1:5555", "1.2.3.4"},
		{"xff single", "1.2.3.4", "", "", "1.2.3.4"},
		{"xri fallback", "", "9.9.9.9", "10.0.0.1:5555", "9.9.9.9"},
		{"raddr fallback", "", "", "10.0.0.1:5555", "10.0.0.1"},
		{"all empty", "", "", "", ""},
		{"xff with spaces", "  1.2.3.4 , 5.6.7.8  ", "", "", "1.2.3.4"},
	}
	for _, c := range cases {
		if got := extractPublicIP(c.xff, c.xrip, c.raddr); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestFirstLANIP(t *testing.T) {
	if got := firstLANIP("192.168.1.5/24"); got != "192.168.1.5" {
		t.Errorf("got %q", got)
	}
	if got := firstLANIP("192.168.1.5"); got != "192.168.1.5" {
		t.Errorf("got %q", got)
	}
}

func TestFirstLANCIDR(t *testing.T) {
	got := firstLANCIDR([]WGLanAddr{{Iface: "en0", CIDR: ""}, {Iface: "en1", CIDR: "192.168.1.5/24"}})
	if got != "192.168.1.5/24" {
		t.Errorf("got %q", got)
	}
	if got := firstLANCIDR(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestNewWGTokenPlaintext(t *testing.T) {
	p1, pref1 := newWGTokenPlaintext()
	p2, _ := newWGTokenPlaintext()
	if !strings.HasPrefix(p1, "polar_wg_") {
		t.Errorf("missing prefix: %q", p1)
	}
	if pref1 != p1[:len("polar_wg_")+8] {
		t.Errorf("prefix mismatch: %q vs %q", pref1, p1[:len("polar_wg_")+8])
	}
	if p1 == p2 {
		t.Errorf("randoms collided (incredible)")
	}
}

func TestValidatePubkey(t *testing.T) {
	if err := validatePubkey(""); err == nil {
		t.Errorf("empty pubkey accepted")
	}
	if err := validatePubkey(strings.Repeat("A", 200)); err == nil {
		t.Errorf("oversize pubkey accepted")
	}
	if err := validatePubkey("TqbeoU9mc1234567890abcdef="); err != nil {
		t.Errorf("normal pubkey rejected: %v", err)
	}
}

// lanCIDRNetwork normalizes device-IP CIDRs to network CIDRs so
// multiple boxes on the same LAN deterministically share a site.
func TestLanCIDRNetwork(t *testing.T) {
	cases := []struct{ in, want string }{
		{"192.168.11.42/24", "192.168.11.0/24"},
		{"192.168.11.42/16", "192.168.0.0/16"},
		{"10.0.0.1/8", "10.0.0.0/8"},
		{"172.16.5.10/12", "172.16.0.0/12"},
		{"", ""},
		{"junk", ""},
		{"192.168.1.1", ""},
	}
	for _, c := range cases {
		if got := lanCIDRNetwork(c.in); got != c.want {
			t.Errorf("lanCIDRNetwork(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestParseMeshCIDRAndDeviceIP(t *testing.T) {
	mesh, err := parseMeshCIDR("10.88.0.0/24")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		d    int
		want string
	}{
		{1, "10.88.0.1"},
		{2, "10.88.0.2"},
		{254, "10.88.0.254"},
	}
	for _, c := range cases {
		if got := mesh.deviceIP(c.d); got != c.want {
			t.Errorf("deviceIP(%d): got %q want %q", c.d, got, c.want)
		}
	}
	// Different /24 base.
	mesh2, err := parseMeshCIDR("192.168.50.0/24")
	if err != nil {
		t.Fatalf("parse2: %v", err)
	}
	if got := mesh2.deviceIP(7); got != "192.168.50.7" {
		t.Errorf("custom /24 deviceIP(7): %q", got)
	}
	// Empty / invalid.
	if _, err := parseMeshCIDR(""); err == nil {
		t.Errorf("empty CIDR accepted")
	}
	if _, err := parseMeshCIDR("not-a-cidr"); err == nil {
		t.Errorf("garbage CIDR accepted")
	}
	if _, err := parseMeshCIDR("fd00::/64"); err == nil {
		t.Errorf("IPv6 accepted (should require v4)")
	}
}

func TestSanitizeHostname(t *testing.T) {
	cases := []struct{ in, want string }{
		{"yarshure-mac", "yarshure-mac"},
		{"Bob's Mac", "bob-s-mac"},
		{"  spaces  ", "spaces"},
		{"WITH.dots", "with.dots"},
		{"  ", ""},
		{"-leading", "leading"},
		{"trailing-", "trailing"},
		{strings.Repeat("a", 200), strings.Repeat("a", 63)},
		{"🍎🍊", ""},
	}
	for _, c := range cases {
		if got := sanitizeHostname(c.in); got != c.want {
			t.Errorf("sanitizeHostname(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestHubWGIPFromCIDR(t *testing.T) {
	got, err := hubWGIPFromCIDR("10.88.0.0/24")
	if err != nil || got != "10.88.0.1" {
		t.Errorf("default mesh: got %q err %v", got, err)
	}
	got, err = hubWGIPFromCIDR("192.168.50.0/24")
	if err != nil || got != "192.168.50.1" {
		t.Errorf("custom /24: got %q err %v", got, err)
	}
}

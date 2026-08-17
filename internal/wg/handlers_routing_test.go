package wg

import (
	"reflect"
	"testing"
)

// hub is a test-fixture constructor for a configured (bound) hub.
func hub(id int64, slug, cidr, endpoint, pubkey, wgip string) WGHub {
	return WGHub{
		ID:       id,
		Slug:     slug,
		MeshCIDR: cidr,
		Endpoint: endpoint,
		Pubkey:   pubkey,
		WGIP:     wgip,
	}
}

func TestCrossHubAllowedExtra(t *testing.T) {
	hubA := hub(1, "west", "100.64.0.0/24", "west.example.com:51820", "PKa", "100.64.0.1")
	hubB := hub(2, "east", "100.64.1.0/24", "east.example.com:51820", "PKb", "100.64.1.1")
	hubC := hub(3, "south", "100.64.2.0/24", "south.example.com:51820", "PKc", "100.64.2.1")

	// Unbound hub (no pubkey) and endpoint-less hub must be skipped.
	hubUnbound := hub(4, "pending", "100.64.3.0/24", "p.example.com:51820", "", "100.64.3.1")
	hubNoEndpoint := hub(5, "nat", "100.64.4.0/24", "", "PKe", "100.64.4.1")
	// Overlapping /24 with hubB — must not be emitted twice.
	hubDup := hub(6, "dupe", "100.64.1.5/24", "dupe.example.com:51820", "PKf", "100.64.1.9")

	tests := []struct {
		name      string
		allHubs   []WGHub
		ownHubID  int64
		ownCIDR   string
		linked    map[int64]bool // hubs the operator has published a link to
		skip      map[int64]bool
		want      []string
	}{
		{
			name:     "no published links → no cross-hub routes",
			allHubs:  []WGHub{hubA, hubB, hubC},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   nil, // nothing published → empty
			want:     []string{},
		},
		{
			name:     "single hub mesh → no cross-hub routes",
			allHubs:  []WGHub{hubA},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   map[int64]bool{2: true, 3: true},
			want:     []string{},
		},
		{
			name:     "both other hubs linked → both /24s",
			allHubs:  []WGHub{hubA, hubB, hubC},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   map[int64]bool{2: true, 3: true},
			want:     []string{"100.64.1.0/24", "100.64.2.0/24"},
		},
		{
			name:     "only one hub linked → only its /24",
			allHubs:  []WGHub{hubA, hubB, hubC},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   map[int64]bool{3: true}, // only hubC linked
			want:     []string{"100.64.2.0/24"},
		},
		{
			name:     "own cidr unnormalized still excludes self",
			allHubs:  []WGHub{hubA, hubB},
			ownHubID: 1,
			ownCIDR:  "100.64.0.7/24", // host bits set; normalizes to .0/24
			linked:   map[int64]bool{2: true},
			want:     []string{"100.64.1.0/24"},
		},
		{
			name:     "unbound + endpoint-less hubs skipped even if linked",
			allHubs:  []WGHub{hubA, hubB, hubUnbound, hubNoEndpoint},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   map[int64]bool{2: true, 4: true, 5: true},
			want:     []string{"100.64.1.0/24"},
		},
		{
			name:     "overlapping /24 emitted once",
			allHubs:  []WGHub{hubA, hubB, hubDup},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   map[int64]bool{2: true, 6: true},
			want:     []string{"100.64.1.0/24"},
		},
		{
			name:     "dual-homed host skips its other direct hub",
			allHubs:  []WGHub{hubA, hubB, hubC},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			linked:   map[int64]bool{2: true, 3: true},
			skip:     map[int64]bool{2: true}, // host is also a direct member of hubB
			want:     []string{"100.64.2.0/24"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crossHubAllowedExtra(tt.allHubs, tt.ownHubID, tt.ownCIDR, tt.linked, tt.skip)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("crossHubAllowedExtra = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOtherConfiguredHubPeers(t *testing.T) {
	hubA := hub(1, "west", "100.64.0.0/24", "west.example.com:51820", "PKa", "100.64.0.1")
	hubB := hub(2, "east", "100.64.1.0/24", "east.example.com:51820", "PKb", "100.64.1.1")
	hubUnbound := hub(3, "pending", "100.64.2.0/24", "p.example.com:51820", "", "100.64.2.1")
	hubNoEndpoint := hub(4, "nat", "100.64.3.0/24", "", "PKd", "100.64.3.1")

	got := otherConfiguredHubPeers([]WGHub{hubA, hubB, hubUnbound, hubNoEndpoint}, 1, "100.64.0.0/24", map[int64]bool{2: true, 3: true, 4: true})
	want := []wgHubPeerEntry{
		{
			Pubkey:       "PKb",
			WGIP:         "100.64.1.1/32",
			Hostname:     "hub:east",
			Endpoint:     "east.example.com:51820",
			AllowedExtra: []string{"100.64.1.0/24"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("otherConfiguredHubPeers = %+v, want %+v", got, want)
	}

	// The hub's own row is excluded; a lone hub yields no fabric peers.
	if peers := otherConfiguredHubPeers([]WGHub{hubA}, 1, "100.64.0.0/24", nil); len(peers) != 0 {
		t.Fatalf("single-hub mesh should produce no other-hub peers, got %+v", peers)
	}

	// wg_ip already carrying /32 is not double-suffixed.
	hubWithMask := hub(5, "north", "100.64.4.0/24", "north.example.com:51820", "PKn", "100.64.4.1/32")
	peers := otherConfiguredHubPeers([]WGHub{hubA, hubWithMask}, 1, "100.64.0.0/24", map[int64]bool{5: true})
	if len(peers) != 1 || peers[0].WGIP != "100.64.4.1/32" {
		t.Fatalf("wg_ip /32 handling wrong: %+v", peers)
	}

	// A hub whose /24 collides with the OWN hub's must be skipped too
	// (own CIDR seeds the dedup set in the shared filter).
	hubOwnDup := hub(6, "shadow", "100.64.0.0/24", "shadow.example.com:51820", "PKs", "100.64.0.9")
	if peers := otherConfiguredHubPeers([]WGHub{hubA, hubOwnDup}, 1, "100.64.0.0/24", map[int64]bool{6: true}); len(peers) != 0 {
		t.Fatalf("hub colliding with own /24 should be skipped, got %+v", peers)
	}
}

// ---- P2 egress ----

func hubWithRoutes(id int64, slug, cidr, endpoint, pubkey, wgip string, routes ...string) WGHub {
	h := hub(id, slug, cidr, endpoint, pubkey, wgip)
	h.AdvertisedRoutes = routes
	return h
}

func i64(v int64) *int64 { return &v }

func TestEgressAllowedExtra(t *testing.T) {
	own := hubWithRoutes(1, "west", "100.64.0.0/24", "west.example.com:51820", "PKa", "100.64.0.1",
		"192.168.10.0/24", "0.0.0.0/0")
	other := hubWithRoutes(2, "east", "100.64.1.0/24", "east.example.com:51820", "PKb", "100.64.1.1",
		"172.16.5.0/24", "0.0.0.0/0")
	otherUnbound := hubWithRoutes(3, "pending", "100.64.2.0/24", "p.example.com:51820", "", "100.64.2.1",
		"10.9.0.0/24")
	all := []WGHub{own, other, otherUnbound}

	tests := []struct {
		name   string
		egress *int64
		want   []string
	}{
		{"nil egress → nothing", nil, nil},
		{"own hub → all routes incl. full tunnel", i64(1), []string{"192.168.10.0/24", "0.0.0.0/0"}},
		{"cross-hub → subnets only, full tunnel stripped", i64(2), []string{"172.16.5.0/24"}},
		{"cross-hub unbound egress hub → nothing (no blackhole)", i64(3), nil},
		{"unknown hub id → nothing", i64(99), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := egressAllowedExtra(tt.egress, &own, all)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("egressAllowedExtra = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFabricPeersCarryAdvertisedSubnetsNotDefault(t *testing.T) {
	hubA := hub(1, "west", "100.64.0.0/24", "west.example.com:51820", "PKa", "100.64.0.1")
	hubB := hubWithRoutes(2, "east", "100.64.1.0/24", "east.example.com:51820", "PKb", "100.64.1.1",
		"172.16.5.0/24", "0.0.0.0/0")
	peers := otherConfiguredHubPeers([]WGHub{hubA, hubB}, 1, "100.64.0.0/24", map[int64]bool{2: true})
	if len(peers) != 1 {
		t.Fatalf("want 1 fabric peer, got %d", len(peers))
	}
	want := []string{"100.64.1.0/24", "172.16.5.0/24"}
	if !reflect.DeepEqual(peers[0].AllowedExtra, want) {
		t.Fatalf("fabric AllowedExtra = %v, want %v (subnets yes, 0.0.0.0/0 never)", peers[0].AllowedExtra, want)
	}
}

func TestValidateAdvertisedRoutes(t *testing.T) {
	hubs := []WGHub{
		hub(1, "west", "100.64.0.0/24", "w:51820", "PKa", "100.64.0.1"),
		hub(2, "east", "100.64.1.0/24", "e:51820", "PKb", "100.64.1.1"),
	}
	tests := []struct {
		name   string
		routes []string
		wantOK bool
	}{
		{"valid subnets", []string{"192.168.10.0/24", "10.9.0.0/16"}, true},
		{"full tunnel marker", []string{"0.0.0.0/0"}, true},
		{"mixed", []string{"192.168.10.0/24", "0.0.0.0/0"}, true},
		{"empty entry", []string{""}, false},
		{"garbage", []string{"not-a-cidr"}, false},
		{"ipv6 rejected", []string{"fd00::/64"}, false},
		{"overlaps a hub mesh /24", []string{"100.64.1.0/25"}, false},
		{"contains a hub mesh /24", []string{"100.64.0.0/16"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAdvertisedRoutes(tt.routes, hubs)
			if (err == nil) != tt.wantOK {
				t.Fatalf("validateAdvertisedRoutes(%v) err=%v, wantOK=%v", tt.routes, err, tt.wantOK)
			}
		})
	}
}

func TestValidateMeshCIDRDisjoint(t *testing.T) {
	hubs := []WGHub{
		hub(1, "west", "100.64.0.0/24", "w:51820", "PKa", "100.64.0.1"),
		hub(2, "east", "100.64.1.0/24", "e:51820", "PKb", "100.64.1.1"),
	}
	if err := validateMeshCIDRDisjoint("100.64.2.0/24", hubs, 0); err != nil {
		t.Fatalf("disjoint /24 rejected: %v", err)
	}
	// Updating hub 1 keeping its own CIDR must pass (self excluded).
	if err := validateMeshCIDRDisjoint("100.64.0.0/24", hubs, 1); err != nil {
		t.Fatalf("self-overlap on update rejected: %v", err)
	}
	if err := validateMeshCIDRDisjoint("100.64.1.0/24", hubs, 0); err == nil {
		t.Fatalf("exact collision accepted")
	}
	if err := validateMeshCIDRDisjoint("100.64.0.0/16", hubs, 0); err == nil {
		t.Fatalf("supernet containing existing hubs accepted")
	}
	if err := validateMeshCIDRDisjoint("100.64.1.128/25", hubs, 0); err == nil {
		t.Fatalf("subnet inside existing hub accepted")
	}
}

func TestSubnetRoutes(t *testing.T) {
	got := subnetRoutes([]string{"192.168.10.0/24", "0.0.0.0/0", " 0.0.0.0/0", "10.0.0.0/8"})
	want := []string{"192.168.10.0/24", "10.0.0.0/8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subnetRoutes = %v, want %v", got, want)
	}
}

func TestHubEndpointResolvesTo(t *testing.T) {
	if !hubEndpointResolvesTo("58.37.118.81", "58.37.118.81") {
		t.Fatal("literal match")
	}
	if hubEndpointResolvesTo("58.37.118.81", "1.2.3.4") {
		t.Fatal("literal mismatch")
	}
	if !hubEndpointResolvesTo("localhost", "127.0.0.1") && !hubEndpointResolvesTo("localhost", "::1") {
		t.Fatal("localhost should resolve to a loopback address")
	}
}

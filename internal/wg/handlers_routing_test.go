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
		want      []string
	}{
		{
			name:     "single hub mesh → no cross-hub routes",
			allHubs:  []WGHub{hubA},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			want:     []string{},
		},
		{
			name:     "three hubs → own excluded, other two /24s",
			allHubs:  []WGHub{hubA, hubB, hubC},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			want:     []string{"100.64.1.0/24", "100.64.2.0/24"},
		},
		{
			name:     "own cidr unnormalized still excludes self",
			allHubs:  []WGHub{hubA, hubB},
			ownHubID: 1,
			ownCIDR:  "100.64.0.7/24", // host bits set; normalizes to .0/24
			want:     []string{"100.64.1.0/24"},
		},
		{
			name:     "unbound + endpoint-less hubs skipped",
			allHubs:  []WGHub{hubA, hubB, hubUnbound, hubNoEndpoint},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			want:     []string{"100.64.1.0/24"},
		},
		{
			name:     "overlapping /24 emitted once",
			allHubs:  []WGHub{hubA, hubB, hubDup},
			ownHubID: 1,
			ownCIDR:  "100.64.0.0/24",
			want:     []string{"100.64.1.0/24"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crossHubAllowedExtra(tt.allHubs, tt.ownHubID, tt.ownCIDR)
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

	got := otherConfiguredHubPeers([]WGHub{hubA, hubB, hubUnbound, hubNoEndpoint}, 1)
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
	if peers := otherConfiguredHubPeers([]WGHub{hubA}, 1); len(peers) != 0 {
		t.Fatalf("single-hub mesh should produce no other-hub peers, got %+v", peers)
	}

	// wg_ip already carrying /32 is not double-suffixed.
	hubWithMask := hub(5, "north", "100.64.4.0/24", "north.example.com:51820", "PKn", "100.64.4.1/32")
	peers := otherConfiguredHubPeers([]WGHub{hubA, hubWithMask}, 1)
	if len(peers) != 1 || peers[0].WGIP != "100.64.4.1/32" {
		t.Fatalf("wg_ip /32 handling wrong: %+v", peers)
	}
}

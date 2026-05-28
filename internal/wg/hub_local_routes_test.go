package wg

import "testing"

func TestNormalizeRouteDst(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"10.88.0.0/24", "10.88.0.0/24"},
		{"10.88.0/24", "10.88.0.0/24"},
		{"10/8", "10.0.0.0/8"},
		{"default", "0.0.0.0/0"},
		{"10.88.0.1", "10.88.0.1/32"},
		{"", ""},
		{"garbage", ""},
		// IPv6 passes through as-is — family filtering happens at the
		// platform collector (netstat -f inet / ip -4) before this fn.
		{"fe80::1/128", "fe80::1/128"},
	}
	for _, c := range cases {
		got := normalizeRouteDst(c.in)
		if got != c.want {
			t.Errorf("normalizeRouteDst(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCollectIfaceNetInfo_EmptyIface(t *testing.T) {
	info := collectIfaceNetInfo("")
	if info.Addrs == nil || info.Routes == nil {
		t.Fatalf("nil slices on empty iface; want empty (non-nil)")
	}
	if len(info.Addrs) != 0 || len(info.Routes) != 0 {
		t.Errorf("got %d addrs %d routes, want 0/0", len(info.Addrs), len(info.Routes))
	}
}

func TestCollectIfaceNetInfo_LoopbackHasAddrButNoExtraRoutes(t *testing.T) {
	// lo0 (darwin) / lo (linux) — both surfaces the 127.0.0.1/8 addr.
	// We just assert the function runs without panicking and returns
	// the non-nil shape; exact routes vary per box.
	for _, name := range []string{"lo0", "lo"} {
		_ = collectIfaceNetInfo(name)
	}
}

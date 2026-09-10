package wg

import "testing"

func lanAddrs(cidrs ...string) []WGLanAddr {
	out := make([]WGLanAddr, 0, len(cidrs))
	for i, c := range cidrs {
		out = append(out, WGLanAddr{Iface: "en" + string(rune('0'+i)), CIDR: c})
	}
	return out
}

func TestSameLANSegment(t *testing.T) {
	cases := []struct {
		name string
		a, b []WGLanAddr
		want bool
	}{
		{"同一个 /24", lanAddrs("192.168.11.57/24"), lanAddrs("192.168.11.82/24"), true},
		{"不同网段", lanAddrs("192.168.11.57/24"), lanAddrs("192.168.10.9/24"), false},
		{"多网卡命中其一", lanAddrs("10.0.0.5/24", "192.168.11.57/24"), lanAddrs("192.168.11.82/24"), true},
		{"公网地址不算同网段", lanAddrs("124.221.22.9/24"), lanAddrs("124.221.22.10/24"), false},
		{"一侧为空", nil, lanAddrs("192.168.11.82/24"), false},
		{"无法解析的 CIDR", lanAddrs("nonsense"), lanAddrs("192.168.11.82/24"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameLANSegment(c.a, c.b); got != c.want {
				t.Fatalf("sameLANSegment = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLanEndpointForDevice(t *testing.T) {
	cases := []struct {
		name string
		dev  *WGDevice
		want string
	}{
		{"nil", nil, ""},
		{
			"wg_endpoint 已是私网 → 直接用",
			&WGDevice{WGEndpoint: "192.168.11.82:51820", WGListenPort: 51820},
			"192.168.11.82:51820",
		},
		{
			"wg_endpoint 是公网 → 退回 LAN 地址",
			&WGDevice{WGEndpoint: "58.37.118.182:1632", WGListenPort: 1632, LANAddrs: lanAddrs("192.168.11.57/24")},
			"192.168.11.57:1632",
		},
		{
			"没有 LAN 地址 → 不替换",
			&WGDevice{WGEndpoint: "58.37.118.182:1632", WGListenPort: 1632},
			"",
		},
		{
			"没有监听端口 → 不替换",
			&WGDevice{LANAddrs: lanAddrs("192.168.11.57/24")},
			"",
		},
		{
			"跳过公网 LAN 地址,取私网那条",
			&WGDevice{WGListenPort: 1639, LANAddrs: lanAddrs("124.221.22.9/24", "192.168.11.197/24")},
			"192.168.11.197:1639",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lanEndpointForDevice(c.dev); got != c.want {
				t.Fatalf("lanEndpointForDevice = %q, want %q", got, c.want)
			}
		})
	}
}

func TestHubsBehindSameNAT(t *testing.T) {
	zen := &WGDevice{LANAddrs: lanAddrs("192.168.11.57/24"), WGListenPort: 1632}
	everest := &WGDevice{LANAddrs: lanAddrs("192.168.11.82/24"), WGListenPort: 1632}
	idc := &WGDevice{LANAddrs: lanAddrs("10.0.13.2/24"), WGListenPort: 51820}

	if !hubsBehindSameNAT(zen, everest, "58.37.118.182:1632", "58.37.118.182:1633") {
		t.Fatal("同网段两个 hub 应判定为同 NAT")
	}
	// 不同网段但同一个出口 IP(跨 VLAN)—— hairpin 一样打不通。
	if !hubsBehindSameNAT(zen, idc, "58.37.118.182:1632", "58.37.118.182:1633") {
		t.Fatal("同出口 IP 应判定为同 NAT")
	}
	if hubsBehindSameNAT(zen, idc, "58.37.118.182:1632", "124.221.22.9:51820") {
		t.Fatal("异地 hub 不应被本地化")
	}
	// endpoint 缺失时不做判断(保留公网 endpoint)。
	if hubsBehindSameNAT(zen, idc, "", "") {
		t.Fatal("空 endpoint 不应判定为同 NAT")
	}
}

func TestFabricEndpointsFingerprint(t *testing.T) {
	a := []wgHubPeerEntry{{Endpoint: "192.168.11.82:1632"}}
	b := []wgHubPeerEntry{{Endpoint: "192.168.11.83:1632"}}
	if fabricEndpointsFingerprint(a) == fabricEndpointsFingerprint(b) {
		t.Fatal("endpoint 变化必须改变指纹,否则客户端不会重渲染 conf")
	}
	if fabricEndpointsFingerprint(a) != fabricEndpointsFingerprint(a) {
		t.Fatal("指纹必须稳定")
	}
	if fabricEndpointsFingerprint(nil) != fabricEndpointsFingerprint([]wgHubPeerEntry{}) {
		t.Fatal("空 fabric 指纹应一致")
	}
}

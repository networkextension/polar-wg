package wg

// 同一网段 / 同一 NAT 下的多 hub —— hub↔hub fabric 的端点本地化。
//
// 两个 hub 落在同一个 LAN(或同一个出口 NAT)后面时,它们互相拨对方的**公网**
// endpoint 是打不通的:家用路由器普遍不支持 hairpin/NAT-loopback,握手永远
// "handshake: never",于是跨 hub 的 /24 全部黑洞。
//
// `wg_hubs.endpoint` 必须保持公网地址(异地 spoke 和异地 hub 要用它),所以修法
// 和 spoke 侧一致:**下发时按对端位置替换**。spoke→自己 hub 这条已经由
// hubEndpointFor()(handlers.go)处理;这里补上 hub→hub 这条 fabric 边。
//
// 判据(任一成立即本地化):
//   1. 两个 hub 的绑定设备共享同一个 LAN 网段(lan_addrs_json 归一化后有交集)——
//      字面意义上的"同一网段";
//   2. 两个 hub 的公网 endpoint 解析到同一个 IP —— 同一个出口 NAT(可能跨 VLAN),
//      hairpin 同样打不通。
//
// 见 doc/wg-multi-hub-routing.md「同一网段 / 同一 NAT」节。

import (
	"net"
	"strconv"
	"strings"
)

// lanNetworkSet 把设备上报的 LAN 地址归一化成网段集合
// ("192.168.11.42/24" → "192.168.11.0/24")。只收私网 IPv4 ——
// 公网地址不构成"同一网段"的证据。
func lanNetworkSet(addrs []WGLanAddr) map[string]bool {
	out := map[string]bool{}
	for _, a := range addrs {
		ip := net.ParseIP(firstLANIP(a.CIDR))
		if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
			continue
		}
		if n := lanCIDRNetwork(a.CIDR); n != "" {
			out[n] = true
		}
	}
	return out
}

// sameLANSegment: 两台设备是否至少共享一个私网网段。
func sameLANSegment(a, b []WGLanAddr) bool {
	sa := lanNetworkSet(a)
	if len(sa) == 0 {
		return false
	}
	for n := range lanNetworkSet(b) {
		if sa[n] {
			return true
		}
	}
	return false
}

// lanEndpointForDevice 给出一台设备可在 LAN 内直拨的 endpoint:优先用已经是
// 私网的 wg_endpoint(心跳会刷新它),否则用第一个私网 LAN 地址 + 监听端口。
// 拿不到就返回 ""(调用方保留公网 endpoint,不做替换)。
func lanEndpointForDevice(d *WGDevice) string {
	if d == nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(strings.TrimSpace(d.WGEndpoint)); err == nil {
		if ip := net.ParseIP(h); ip != nil && ip.IsPrivate() {
			return strings.TrimSpace(d.WGEndpoint)
		}
	}
	if d.WGListenPort <= 0 {
		return ""
	}
	for _, a := range d.LANAddrs {
		ip := net.ParseIP(firstLANIP(a.CIDR))
		if ip != nil && ip.To4() != nil && ip.IsPrivate() {
			return net.JoinHostPort(ip.String(), strconv.Itoa(d.WGListenPort))
		}
	}
	return ""
}

// hubsBehindSameNAT: 两个 hub 是否处在同一网段 / 同一出口 NAT 之后,
// 也就是"互相拨公网 endpoint 会因为 hairpin 打不通"的那一类。
func hubsBehindSameNAT(devA, devB *WGDevice, epA, epB string) bool {
	if devA != nil && devB != nil && sameLANSegment(devA.LANAddrs, devB.LANAddrs) {
		return true
	}
	return sameEndpointHost(epA, epB)
}

// sameEndpointHost: 两个 "host:port" 的 host 是否指向同一个 IP。
// host 可以是域名(走 hubResolveCache,5 分钟缓存)或字面 IP。
func sameEndpointHost(epA, epB string) bool {
	ha, _, err := net.SplitHostPort(strings.TrimSpace(epA))
	if err != nil {
		return false
	}
	hb, _, err := net.SplitHostPort(strings.TrimSpace(epB))
	if err != nil {
		return false
	}
	if ha == hb {
		return true
	}
	ipsA := resolveHostIPs(ha)
	if len(ipsA) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, ip := range ipsA {
		seen[ip] = true
	}
	for _, ip := range resolveHostIPs(hb) {
		if seen[ip] {
			return true
		}
	}
	return false
}

// localizeFabricEndpoints 把 fabric peer 里"同网段/同 NAT"的对端 endpoint 换成
// 对方 hub 设备的 LAN endpoint。就地改写 peers。DB 读取量 = 每个 fabric peer 一
// 次设备查询(hub 数量是个位数,刷新周期 30-60s,可以忽略)。
func (p *Plugin) localizeFabricEndpoints(ownHub *WGHub, allHubs []WGHub, peers []wgHubPeerEntry) {
	if ownHub == nil || len(peers) == 0 || ownHub.BoundDeviceID == nil {
		return
	}
	ownDev, err := p.getWGDeviceByID(*ownHub.BoundDeviceID)
	if err != nil || ownDev == nil {
		return
	}
	byPubkey := make(map[string]*WGHub, len(allHubs))
	for i := range allHubs {
		if k := strings.TrimSpace(allHubs[i].Pubkey); k != "" {
			byPubkey[k] = &allHubs[i]
		}
	}
	for i := range peers {
		h := byPubkey[strings.TrimSpace(peers[i].Pubkey)]
		if h == nil || h.BoundDeviceID == nil {
			continue
		}
		otherDev, err := p.getWGDeviceByID(*h.BoundDeviceID)
		if err != nil || otherDev == nil {
			continue
		}
		if !hubsBehindSameNAT(ownDev, otherDev, ownHub.Endpoint, h.Endpoint) {
			continue
		}
		if lep := lanEndpointForDevice(otherDev); lep != "" {
			peers[i].Endpoint = lep
		}
	}
}

// fabricEndpointsFingerprint —— fabric peer 的 endpoint 集合指纹,折进
// /v1/hub/peers 的 rev。否则对端 hub 的 LAN IP 变了(DHCP)而 wg_hubs.updated_at
// 没动,rev 不变 → 客户端不会重渲染 conf,fabric 卡在旧地址上。
func fabricEndpointsFingerprint(peers []wgHubPeerEntry) uint32 {
	var h uint32 = 2166136261 // FNV-1a
	for _, e := range peers {
		for _, b := range []byte(e.Endpoint + "|") {
			h ^= uint32(b)
			h *= 16777619
		}
	}
	return h
}

# wg-mac NAT 穿透 — design (Phase 9 设计文档)

> 半年起步工程。本文先把分阶段方案、协议、风险写清楚，**实现 PR 一个个来**。
> 决策：要不要砸钱做？做几期？参考 `doc/wg-mac-vs-tailscale.md §3` #9。

---

## 0. 现状 + 目标

**现状**：wg-mac mesh 要求 hub 节点有公网 IP（或 DDNS）。`wg_hubs.endpoint`
是 hub 上线时填的 `host:51820`。两端都在 NAT 后的 device-to-device 流量**只能**
走 hub 转发；hub 挂了就废。

**目标**：让两端都在 NAT 后的设备能**直连**（hole punch），失败时**自动 fallback**
到 hub relay。完整覆盖 ICE-lite + STUN-style endpoint discovery，把 hub 转发流量
压到 < 10%（参考 Tailscale 数据：DERP 仅承载 ~5% 流量）。

**对标**：Tailscale 的 DERP + STUN/ICE 组合是事实标准。我们做**轻量版**——
单 mesh 通常 < 50 节点，不需要全球 30+ relay。

---

## 1. 三阶段拆解

| 阶段 | 主要工作 | LOC 估算 | 风险 | 交付 |
|---|---|---|---|---|
| **P1 — endpoint discovery + 协议扩展** | server 收集每个 device 的"自报 LAN 列表 + 服务端观察到的 srcIP:srcPort"；peer list 里给每个对端塞**候选 endpoint 列表**（不止一个）；客户端**并发尝试**所有候选 | ~600 LOC server + ~400 LOC C 客户端 | 低 | 同 LAN 自动 P2P，多 NAT 边界仍要 relay |
| **P2 — UDP hole punch + STUN binding** | 加一个 `/v1/stun` UDP server（标准 STUN binding request），客户端先 STUN 查自己出口 ip:port，POST 给 server；server 把双方出口告诉对方；同时 PUNCH 包；wireguard 看到的 src 就是 NAT 映射 | ~800 LOC server + ~1200 LOC C 客户端（含 ICE 状态机） | 中高 | 80% 的家庭/办公 NAT 能穿；CGNAT / 对称 NAT 失败 |
| **P3 — DERP-lite relay 网格** | hub 之间互相做 fallback relay（任何 hub 都能 relay 任何 mesh 的流量）；客户端在直连失败 N 秒后切到最近的 hub；TLS over TCP 包 wg UDP 防火墙穿透 | ~1500 LOC server + ~800 LOC C 客户端 | 高（协议复杂、HA） | 100% 可达性，hub 不再是 SPOF |

**总计**: ~5300 LOC 跨多个 PR，**保守 4-6 个月**。

---

## 2. P1 协议扩展 — endpoint candidates

修改 `wgPeerResponse`：

```go
type wgPeerResponse struct {
    Pubkey       string   `json:"pubkey"`
    WGIP         string   `json:"wg_ip,omitempty"`
    // OLD (Phase 2): single endpoint string
    // Endpoint     string   `json:"endpoint"`
    // NEW (Phase 9.1): ordered candidate list. Client tries in order.
    Endpoints    []wgEndpointCandidate `json:"endpoints"`
    SiteSlug     string   `json:"site_id,omitempty"`
    Hostname     string   `json:"hostname,omitempty"`
    AllowedExtra []string `json:"allowed_extra,omitempty"`
}

type wgEndpointCandidate struct {
    Kind     string `json:"kind"`     // "lan" | "stun" | "hub-relay"
    Addr     string `json:"addr"`     // "host:port"
    Priority int    `json:"priority"` // higher = try first
}
```

Server collects candidates from:
1. **lan**: device's `lan_addrs[].cidr` → `<ip>:wg_listen` (Phase 2 already has this)
2. **stun**: server's observed `srcIP:srcPort` from the most recent UDP packet (Phase 2 has `wg_endpoint` from heartbeat — repurpose)
3. **hub-relay**: hub's public endpoint as the last-resort candidate

Backwards compat: keep `endpoint` string set to highest-priority candidate so old clients still work.

**Schema impact**: add `wg_devices.observed_endpoints_json` (JSON array of `{kind, addr, observed_at}`) — server writes on each heartbeat.

**Client impact**: C `wg_session` needs a multi-endpoint state machine. wg_core today binds one endpoint per peer; needs to round-robin try candidates within REKEY_TIMEOUT and pin the first one that handshakes successfully. ~400 LOC C.

---

## 3. P2 STUN + hole punch

### 3.1 STUN binding server

New endpoint: `UDP 0.0.0.0:<configurable_stun_port>` (separate from wg
ports). Speaks RFC 5389 binding request → response with the observed
SOURCE-ADDRESS attribute. Stateless. ~150 LOC Go.

Why a separate port: lets clients hit it directly without going through
the wg control plane HTTP, and STUN responses bypass TLS encryption
overhead.

### 3.2 Coordinator endpoint

`POST /v1/stun/punch` (HTTPS):
- Body: `{target_device_id, observed_self_endpoint}`
- Server: validates caller has same `hub_id` as target, then forwards
  to target via a long-poll channel — target sees `{from_device_id,
  their_endpoint, your_endpoint}` and BOTH sides emit UDP punch packets
  to each other's observed endpoint within ~500ms of each other.
- WireGuard's handshake then succeeds on one of the candidate slots.

Long-poll channel: server-side a `notification_chan` per device; HTTP
SSE or websocket if we want bidirectional. **Pick: SSE** (simpler,
no upgrade dance).

### 3.3 NAT types support

| NAT type | Hole punch works? | Notes |
|---|---|---|
| Full Cone | ✅ trivial | even unilateral works |
| Restricted Cone | ✅ | both sides must send first packet within window |
| Port-Restricted Cone | ✅ | same as above |
| Symmetric (most strict) | ⚠️ partial | requires port prediction; ~60% success |
| CGNAT | ❌ | must use hub relay (operator's ISP problem) |

Strategy: try punch, give up after 3 attempts × 5s, mark this pair as
"relay-only" and remember for 1h. Cache invalidated on either side's
endpoint change.

### 3.4 Schema additions

```sql
CREATE TABLE IF NOT EXISTS wg_punch_attempts (
    id BIGSERIAL PRIMARY KEY,
    initiator_device_id BIGINT REFERENCES wg_devices(id) ON DELETE CASCADE,
    target_device_id    BIGINT REFERENCES wg_devices(id) ON DELETE CASCADE,
    initiator_endpoint  TEXT NOT NULL,
    target_endpoint     TEXT NOT NULL,
    result              TEXT,  -- "success" | "timeout" | "rejected"
    attempted_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_wg_punch_pair_recent
    ON wg_punch_attempts(initiator_device_id, target_device_id, attempted_at DESC);
```

Cache lookup before next attempt: if last attempt was < 1h ago and
result = "timeout", skip directly to relay.

---

## 4. P3 DERP-lite relay

The "no SPOF" tier. Tailscale's DERP is HTTP/2 with WS upgrade per
peer; we can do simpler.

### 4.1 Relay protocol

Each hub also runs a **relay listener** on its existing endpoint port
(or a dedicated `relay_port`). Wire shape:

```
client sends to relay over wg UDP:
  outer: src=client public ip, dst=relay public ip
  payload: [relay_header(target_pubkey)] [wg_inner_frame]

relay rewrites:
  outer: src=relay public ip, dst=resolved target ip
  payload: [wg_inner_frame]

target's wg handshake sees relay as the apparent peer; but the
pubkey + AllowedIPs is set to the original client, so handshake
succeeds.
```

Hub's relay code just demuxes by `target_pubkey` (registered in
wg_devices) and forwards. ~400 LOC C in wg_core.

### 4.2 Multi-hub relay graph

Each hub knows ALL other hubs' relay endpoints (via Polar control
plane). If client wants to reach a device in another hub's mesh AND
direct fails, client picks the nearest hub by RTT (measured via STUN
ping) and tunnels through it.

Cross-hub key exchange: when device A in hub W wants device B in hub
E, both pubkeys are exchanged through Polar (extension of
`/v1/peers`). No fundamental wireguard change.

### 4.3 Polar additions

- `wg_hubs.relay_endpoint` column (NULL = relay disabled for this hub)
- `wg_hubs.relay_capacity_devices` (admin-set, default 200)
- New endpoint `GET /v1/hubs` (read-only list, all hubs across all
  meshes if mesh-graph mode is enabled)
- Admin UI: Hub form gets a "Act as relay for other meshes" checkbox

---

## 5. 替代方案考虑

| 方案 | 评价 |
|---|---|
| **直接 fork tailscale 客户端** | 最快但 GPL/MIT 边界要研究；tailscale 协议 CapVer 100+ 复杂；放弃我们对 wg_core 的控制 |
| **headscale 兼容层** | 见 doc/wg-mac-vs-tailscale.md #11 — 不做 |
| **DERP-only no-punch** | 永远走 relay；简单但带宽和延迟代价大；不是 Tailscale 级体验 |
| **完整 ICE (full RFC 8445)** | 最贵；除 NAT 复杂场景外不需要 |

**推荐**：分阶段做 P1 → P2，P3 看实际用户反馈。

---

## 6. 决策时间表

- **现在**（Polar Phase 3 这个 PR）：本设计文档落盘；任何代码都不写。
- **+1 月**：用户反馈"hub 挂了"或"我家没公网 IP"次数 ≥ 3 → 启动 P1。
- **+3 月**：P1 ship 后，统计 "hub-relayed" 流量比例。若 > 30% → P2。
- **+6 月**：P2 ship 后，若 hub SPOF 还有事件 → P3。

否则继续保持当前的"要求 hub 有公网 IP"约束，把精力放在 §3 的其他高 ROI 项。

---

## 7. 反对意见 — 为什么也许不该做

诚实记录：

- **Tailscale 公司核心 IP**：人家投了 5+ 年工程师做这个；我们小团队抄不到这水平
- **维护成本**：STUN/ICE/DERP 的边缘 case 永远填不完
- **用户场景未必需要**：如果客户都是企业内网（hub 有公网 IP），这一块工作零回报
- **机会成本**：5K LOC 的精力能做完 #3+#4+#7.5+#5+#12+#14 整套
- **风险**：协议级 bug 会让 mesh 沉默死亡，比"hub 挂了"难排错

**反对结论**：如果客户场景里 hub 公网 IP 是合理假设，**别做**。如果客户场景里
"两端都在 NAT 后"是 50%+，**做 P1 已经覆盖大部分**。

---

## 8. 相关文档

- `doc/JOIN_PROTOCOL.md` — 当前 Phase 2 protocol
- `doc/wg-mac-api.md` — 当前 /v1/* API
- `doc/wg-mac-vs-tailscale.md §3 #9` — 决策入口

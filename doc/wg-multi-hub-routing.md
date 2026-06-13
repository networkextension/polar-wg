# wg-mac 多 Hub 组网与路由分发 — design

Status: design · 2026-06-12
Scope: `polar-wg` 控制面 (wg-svc) + wgctl 客户端 conf 渲染。**不涉及** NAT 穿透
(见 `wg-mac-nat-traversal-design.md`,那是另一条线)。

## 决策(已拍板)

1. **不做 hub 选举,纯手工指派。** Hub 由运营商显式指定 —— 现有 `role=hub` token
   机制已经是手工的(operator 铸 token 时填 `public_endpoint`)。多 hub 只是允许
   `wg_hubs` 有多行,这个 schema 已经支持。
2. **指派依据 = 是否有公网 IP + 资源位置。**
   - **有公网 IP** 是当 hub 的硬性资格 —— hub 必须能被其他 hub 和本 site 的 spoke
     直接拨到(`wg_hubs.endpoint` 非空)。没公网 IP 的节点只能当 spoke。
   - **资源位置** 决定 spoke 挂哪个 hub:device 注册时按所在机房/LAN 归到"最近"的
     hub(operator 用 hub-scoped token 控制,token 自带 `hub_id`)。
3. **系统只负责"打通路由"。** 不做 failover、不做选举、不做动态负载均衡。控制面唯一的
   职责是:维护 `hub → 它负责的 CIDR 集合` 映射,据此算出每个节点的 peer list 和
   AllowedIPs。路由表正确 = 任务完成。

## 拓扑模型:Hub 全互联 + Spoke 星挂

```
        机房A (公网IP)              机房B (公网IP)
         Hub-A  ◄───── hub 全互联 ─────►  Hub-B
        /  |  \                          /  |  \
   spoke spoke spoke              spoke spoke spoke
   (同site LAN直连)                 (同site LAN直连)
```

- **Spoke**:无公网 IP,只认两类 peer —— ① 同 site 的 LAN-direct spoke(已实现)
  ② 自己的 hub(AllowedIPs = 自己 hub 的 /24 + 其他所有 hub 的 /24,把所有非本地
  流量甩给 hub)。
- **Hub**:有公网 IP,peer = ① 自己名下所有 spoke(每个 /32) ② **其他所有 hub**
  (AllowedIPs = 对方 hub 负责的 CIDR 集合)。Hub 之间因为都有公网 IP,两两直连无 NAT
  问题,天然全互联(full mesh among hubs)。

跨 hub 流量路径:`spokeA → HubA → HubB → spokeB`。每跳都是 wireguard 加密直连。

## 核心:路由分发算法

控制面要算的就一张表 —— **每个 hub 负责哪些 CIDR**。路由单位是 **hub 的 `mesh_cidr`
(一个 /24)**,不是 per-site —— `s_index` 在 schema 里存在,但 `deviceIP()`
(`alloc.go`) 并不用它分片,同一 hub 下所有 site 共享 hub 的那个 /24。各 hub 的 /24
由 `suggestFreeMeshCIDR` 从 CGNAT `100.64.0.0/10` 里挑互不重叠的块(故意非连续,不是
一个 /16 的切片)。

```
hub_owned_cidrs(hub) = hub.mesh_cidr             (它自己的 /24,如 100.64.1.0/24)
                     + hub.advertised_routes     (出口/外部子网,见下;P2)
```

关键:spoke 和 hub 走的是**两个不同的端点**,两侧都要改:

- **Spoke** poll `/v1/peers`(`buildPeerListResponse`,`handlers.go`)。它只看到自己
  hub 这一条 peer。改造:把这条 hub-peer 的 `allowed_extra` 从"仅自己 hub 的 /24"
  扩成 **"自己 hub 的 /24 + 其他所有 hub 的 /24"**,于是跨 hub 流量也甩给本 hub。
  **客户端透明** —— `allowed_extra` 本来就是客户端已经在用的字段。
- **Hub** poll `/v1/hub/peers`(`handleWGHubPeers`,`handlers.go`)。它原本只拿到自己
  名下的 spoke。改造:追加**其他每个 configured hub** 作为 peer,带上对方的公网
  `endpoint` + `allowed_extra=[对方 /24]`,形成 hub 间 full mesh。需要给
  `wgHubPeerEntry` 加 `endpoint` / `allowed_extra` 两个字段(additive,`omitempty`)。

两侧的 CIDR 组装抽成**纯函数**(无 DB,便于不依赖 Postgres 单测):

```go
// spoke 侧:其他每个 configured hub 的 /24
func crossHubAllowedExtra(allHubs []WGHub, ownHubID int64, ownCIDR string) []string
// hub 侧:其他每个 configured hub 作为一条 hub-peer
func otherConfiguredHubPeers(allHubs []WGHub, ownHubID int64) []wgHubPeerEntry
```

两者都跳过:自己 hub、未 bound(无 pubkey)或无公网 endpoint 的 hub、以及 /24 与已
输出项重复的 hub(防御 mesh_cidr 重叠)。

**无需新 store 方法** —— 直接复用 `listWGHubs()`。`hub_owned_cidrs` 退化成读
`hub.mesh_cidr` 一个字段。

## 出口 / 外部子网路由(advertised routes)— P2,已实现

"涉及到出口" —— 某些 hub 不止转发 mesh 内流量,还是通往**物理子网或公网的网关**:
- 机房内网 `192.168.10.0/24`(hub 同时在 mesh 和机房 LAN 上)
- 全隧道出口 `0.0.0.0/0`(让某些 spoke 的全部上网流量从这个 hub 出去)

**实现语义(已拍板 + 落地):**

1. **全部 opt-in,零自动分发**:hub 在 `advertised_routes_json` 声明出口 CIDR;设备
   通过 `wg_devices.egress_hub_id`(每设备一个,NULL=关)显式选用。没选的设备一条
   advertised route 都看不到 —— 与 mesh /24(自动分发)刻意不同。
2. **全隧道仅限本 hub**:`0.0.0.0/0` 只在 `egress_hub_id == 本 hub` 时下发。跨 hub
   选用时服务端剥掉默认路由只给子网(跨 hub 默认路由会劫持中转 hub 自己的流量)。
   admin 设置时若跨 hub 且对方只声明了 `0.0.0.0/0`,直接 400 拒绝(避免静默 no-op)。
3. **fabric 无条件携带子网**:hub 互联 peer 的 AllowedIPs = 对方 /24 + 对方 advertised
   **子网**(永不含 `0.0.0.0/0`)—— 这是跨 hub 出口流量的中转路径,与设备 opt-in 无关。
4. **校验**:advertised route 必须是合法 IPv4 CIDR,且不得与任何 hub 的 mesh_cidr
   重叠(防 mesh 路由被遮蔽);顺带补了 mint/update 时 hub mesh_cidr 互不重叠校验
   (P1 的 follow-up)。

```sql
-- 实际 schema(ensureEgressColumns 启动幂等 + wg-schema.sql)
ALTER TABLE wg_hubs    ADD COLUMN IF NOT EXISTS advertised_routes_json JSONB;
ALTER TABLE wg_devices ADD COLUMN IF NOT EXISTS egress_hub_id BIGINT REFERENCES wg_hubs(id) ON DELETE SET NULL;
```

Admin 面:`PUT /api/admin/wg-hubs/:id` 收 `advertised_routes`(nil=不动,[]=清空);
新增 `PUT /api/admin/wg-devices/:id` 收 `{egress_hub_id}`。UI:Hubs 标签「出口」按钮
编辑 CIDR 列表;Devices 标签每行出口下拉(标注 本hub/跨hub仅子网)。

**运维注意(NAT)**:出口 hub 把 mesh 流量转出物理网卡时需要 masquerade,wg 层不管。
macOS pf 示例:`nat on en0 from 100.64.0.0/10 to any -> (en0)`;Linux:
`iptables -t nat -A POSTROUTING -s 100.64.0.0/10 -o eth0 -j MASQUERADE`。

## Schema 改动汇总(全部 additive)

见上节 SQL —— P2 共两列。`wg_hubs` 多行、`hub_id` 外键、site→hub 归属全都已存在。
P1 cross-hub 路由零 schema 改动。

## 客户端(wgctl)影响

- **Spoke 节点(零客户端改动)**:hub-peer 的 `allowed_extra` 多了几条 /24。沿用现有
  `wgPeerResponse.AllowedExtra` 字段,wgctl "用 server 返回覆盖本地 conf" 流程不动 ——
  P1 server 一上线即生效。
- **Hub 节点 — 初始 conf(零客户端改动)**:`/v1/register` 对 hub 本机的响应现在直接
  包含其他 hub 作为 peer(`buildPeerListResponse` 的 hub-self 分支),而 install.sh
  内嵌的渲染器本来就会写 `endpoint` + `allowed_extra` → **装机即获得 hub fabric**。
  install.sh 同时在 `role=hub` 时开启 IP 转发(darwin `net.inet.ip.forwarding=1` /
  linux `net.ipv4.ip_forward=1`,并写入 sysctl.conf 持久化)。
- **Hub 节点 — 后续刷新(已验证客户端兼容,零改动)**:wgctl-agent 周期 poll 的
  `/v1/hub/peers` 现在会多返回其他 hub 的条目(新增 `endpoint` + `allowed_extra`
  字段)。客户端在 `networkextension/polar-wg-app` —— 经核对其两条渲染路径
  (`scripts/join.sh` 初始 conf、`scripts/wgctl-agent.sh` 周期重写)都是**通用 peer
  渲染**:对每个条目读 `pubkey` / `wg_ip` / `endpoint` / `allowed_extra`,hub 与
  device 角色共用同一段代码(role 只切 URL)。新字段 key 与渲染器已读的 key 完全
  一致 → hub roster 变化(新增 hub / 换 endpoint / 改出口)在下一次 agent poll 即
  生效,**无需重装,无需客户端改动**。

## 一致性 / 防环

- Hub 全互联是**完全图**,但 wireguard 路由按 AllowedIPs 最长前缀匹配,每个目标 /24
  只属于唯一一个 hub → 不会成环。前提:**各 hub 的 `mesh_cidr` 互不重叠**。
- 保证来源:`suggestFreeMeshCIDR` 从 `100.64.0.0/10` 挑空闲 /24;P2 起
  `validateMeshCIDRDisjoint` 在 hub-token 铸造与 hub 更新两条路径强制校验运营商
  自填的 mesh_cidr 不与任何现有 hub 重叠。纯函数侧仍保留防御性跳过(双保险)。

## Phasing

- **P1(本 PR,server 侧)** 共享过滤器 `otherConfiguredHubs` + 两个纯函数
  `crossHubAllowedExtra` / `otherConfiguredHubPeers`;`buildPeerListResponse` 改造
  (spoke 侧 widening + hub-self fabric 分支);`handleWGHubPeers` / `wgHubPeerEntry`
  (hub 刷新合约);install.sh hub 角色开 IP 转发。复用 `listWGHubs()`,零 schema
  改动。spoke 侧 + hub 初始 conf 即时生效。
- **P1-client(已验证为 no-op)** polar-wg-app 的 join.sh / wgctl-agent.sh 渲染器
  本就通用读 `endpoint` + `allowed_extra`,与新字段 key 一致 —— 客户端零改动,
  整个系列端到端 client-complete。见上文「客户端影响」节。
- **P2(已实现)** `advertised_routes_json` 出口路由 + per-device `egress_hub_id`
  选择 + mesh_cidr 重叠校验 + admin API/UI。见上文「出口」节。
- **P3(已实现)** 拓扑图标注:hub 节点下方显示 owned mesh_cidr;声明了出口路由的
  hub 带 🌍 角标(悬停列出路由,全隧道特别标注);hub tooltip 含 mesh + 出口。
  hub↔hub 互联线 PR#16 已有(P1 fabric peer 出现在 wg show 后自动显现)。

## 不做(out of scope)

- Hub 选举 / 自动 failover —— 明确不做,hub 挂了由运营商手动处理。
- NAT 穿透 / hole punch —— 见 `wg-mac-nat-traversal-design.md`,独立线。
- 动态负载均衡 / spoke 自动选最近 hub —— 资源位置由 operator 用 token 的 hub_id 钉死。
```

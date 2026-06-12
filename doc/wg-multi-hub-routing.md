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

## 出口 / 外部子网路由(advertised routes)

"涉及到出口" —— 某些 hub 不止转发 mesh 内流量,还是通往**物理子网或公网的网关**:
- 机房内网 `192.168.10.0/24`(hub 同时在 mesh 和机房 LAN 上)
- 全隧道出口 `0.0.0.0/0`(让某些 spoke 的全部上网流量从这个 hub 出去)

做法:给 hub 加一个运营商配置的 **advertised_routes** 字段,合并进 `hub_owned_cidrs`。
spoke 要用某个出口,就把对应 hub 的这些 CIDR 放进它 hub-peer 的 AllowedIPs。
**默认不全给** —— 全隧道 `0.0.0.0/0` 必须 spoke 显式选择(否则会劫持所有流量)。

```sql
ALTER TABLE wg_hubs ADD COLUMN IF NOT EXISTS advertised_routes_json TEXT NOT NULL DEFAULT '[]';
-- 例: ["192.168.10.0/24", "0.0.0.0/0"]
```

是否把某 hub 的 `0.0.0.0/0` 出口暴露给某 spoke,是一个**显式开关**(per-device 或
per-site 的策略),不是自动 —— 避免误把所有人流量导到一个机房。

## Schema 改动汇总(全部 additive)

```sql
-- hub 的对外/出口路由声明(出口网关、全隧道)
ALTER TABLE wg_hubs ADD COLUMN IF NOT EXISTS advertised_routes_json TEXT NOT NULL DEFAULT '[]';
```

就这一列(P2 才加)。`wg_hubs` 多行、`hub_id` 外键、site→hub 归属全都已存在。P1
cross-hub 路由**零 schema 改动** —— 路由单位就是 `hub.mesh_cidr`,出口从
`advertised_routes_json` 读(P2)。

## 客户端(wgctl)影响

- **Spoke 节点(零客户端改动)**:hub-peer 的 `allowed_extra` 多了几条 /24。沿用现有
  `wgPeerResponse.AllowedExtra` 字段,wgctl "用 server 返回覆盖本地 conf" 流程不动 ——
  P1 server 一上线即生效。
- **Hub 节点(需要客户端跟进)**:`/v1/hub/peers` 现在会多返回其他 hub 的条目,带新增的
  `endpoint` + `allowed_extra` 字段。消费 `/v1/hub/peers` 的 **wgctl / wg-mac 客户端
  (独立 C 代码库,不在本 repo)** 需要学会把这些条目渲染成带公网 endpoint + AllowedIPs
  的 [Peer],并开 `net.inet.ip.forwarding=1`。**在客户端落地前,server 合约已就绪但
  hub 间不会真正转发** —— spoke 侧已先生效,无回归。

## 一致性 / 防环

- Hub 全互联是**完全图**,但 wireguard 路由按 AllowedIPs 最长前缀匹配,每个目标 /24
  只属于唯一一个 hub → 不会成环。前提:**各 hub 的 `mesh_cidr` 互不重叠**。
- 保证来源:`suggestFreeMeshCIDR` 从 `100.64.0.0/10` 挑空闲 /24。但
  `createWGHub`/`updateWGHub` 默认 `100.64.0.0/24` 且 `mesh_cidr_pref` 运营商可自填,
  两个手建 hub 理论上可能撞同一 /24。两个纯函数已**防御性跳过** /24 与已输出项重复的
  hub;建议(本 PR 可选,否则跟进)在 hub-token 铸造处加一条 mesh_cidr 重叠校验。

## Phasing

- **P1(本 PR,server 侧)** 两个纯函数 `crossHubAllowedExtra` /
  `otherConfiguredHubPeers` + `buildPeerListResponse`(spoke 侧)+ `handleWGHubPeers`
  / `wgHubPeerEntry`(hub 侧)。复用 `listWGHubs()`,零 schema 改动。spoke 侧即时生效;
  hub 侧合约就绪,等客户端跟进。
- **P1-client(跟进)** wgctl / wg-mac 渲染 `/v1/hub/peers` 新增的 hub-peer 条目。
- **P2** `advertised_routes_json` 出口路由 + per-spoke 出口选择策略。
- **P3** 管理 UI:hub 拓扑图标注每个 hub 的 owned-CIDRs 和出口(复用
  `feat/wg-hub-topology-ui` PR#16 的星形图,升级成多 hub 互联图)。

## 不做(out of scope)

- Hub 选举 / 自动 failover —— 明确不做,hub 挂了由运营商手动处理。
- NAT 穿透 / hole punch —— 见 `wg-mac-nat-traversal-design.md`,独立线。
- 动态负载均衡 / spoke 自动选最近 hub —— 资源位置由 operator 用 token 的 hub_id 钉死。
```

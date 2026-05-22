# Tailscale 客户端兼容 — design (Phase 11)

> 用户目标：**操作员把 `--login-server=https://our.example` 一改，原版
> Tailscale 客户端就能加入我们的 mesh**。Polar dock 同时维持 wg-mac 客户端的
> `/v1/*` 协议。这是 `doc/wg-mac-vs-tailscale.md §3 #11`，决策已翻转：要做。

---

## 0. 现状 vs 目标

**现状**：
- Polar dock 实现自定义 `/v1/register`、`/v1/peers` 等（我们的协议）
- wg-mac 客户端（C + Swift NE）讲这个协议
- Tailscale 客户端讲 **完全不同** 的协议（`/machine/<pubkey>/key`、`/machine/<pubkey>/map`、`/machine/<pubkey>/poll`、DERP map、Noise 协议握手等）

**目标**：
- 两条协议都在 Polar dock 里跑（**dual control plane**）
- wg_devices / wg_hubs 是公共底座
- 用户可以混部：一台 mac 跑 wg-mac，另一台 mac 跑 Tailscale 官方客户端，**同一个 mesh**
- 看作 "**自托管 Headscale + 自己的 macOS C 客户端**"

---

## 1. 三条路径对比

| 路径 | 工作量 | 优点 | 缺点 |
|---|---|---|---|
| **A. 从零写 Tailscale 协议层** | **6+ 月** | 100% 控制 | Tailscale CapVer 还在涨；追协议追到死 |
| **B. Vendor Headscale 当 library** | **8-12 周** | 30K LOC 现成可工作的代码（MIT 开源），社区跟着 TS bump | Schema 双写或映射；进程内集成有边界（DB 池、日志、配置） |
| **C. Sidecar Headscale 独立进程** | **4-6 周** | Headscale 完全独立跑，Polar 只做 web UI 代理 + 用户/token 联动 | 两个进程要 supervisor；DB 双套；运维复杂 |

**推荐 B**：vendor Headscale。最大化复用，又能改 internals。

---

## 2. 推荐架构 — Vendored Headscale

```
┌──────────────────────────────────────────────────────────┐
│ polar-dock (single Go binary)                            │
│                                                          │
│  ┌─────────────────────┐    ┌────────────────────────┐  │
│  │ /v1/* handlers      │    │ /machine/*, /derp/*    │  │
│  │ (wg-mac native)     │    │ (Tailscale-compat)     │  │
│  │ — Phase 1/2/3 code  │    │ — vendored Headscale   │  │
│  └─────────────────────┘    └────────────────────────┘  │
│           ↓                            ↓                 │
│  ┌──────────────────────────────────────────────────┐    │
│  │ Shared adapter layer (新增 ~600 LOC)             │    │
│  │  - wgDevicesAdapter implements headscale.Machine │    │
│  │  - wgHubsAdapter implements headscale.Namespace  │    │
│  │  - tokenAdapter: wg_tokens ↔ headscale PreAuthKey│    │
│  └──────────────────────────────────────────────────┘    │
│           ↓                                              │
│  ┌──────────────────────────────────────────────────┐    │
│  │ Postgres (ideamesh)                              │    │
│  │  - wg_devices, wg_hubs, wg_tokens (我们的)       │    │
│  │  - headscale 用 separate schema headscale.*      │    │
│  │    OR 完全映射到我们表（adapter layer 干）       │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

**关键决定**：是 schema 双写（headscale 自己一套表，adapter 同步），还是 adapter 把 headscale 内部 model 投影到我们表上？

| 方案 | 评价 |
|---|---|
| **B1. 两套 schema 双写** | 简单，headscale 当黑盒；缺点：写操作要同步两套，conflict 时谁赢？ |
| **B2. Adapter 投影到我们表** | 单一 source of truth；缺点：headscale 内部 query 模式可能要重写 |

**推荐 B1 起步，B2 长期**。先让两者并存，确认协议跑通后再考虑统一 schema。

---

## 3. Tailscale 协议核心点（要实现的）

Headscale 已经把这些都实现了。我们的工作是 **wire it up + map identity to our token system**。

| Endpoint | 用途 | 频率 |
|---|---|---|
| `POST /machine/<machinekey>/key` | 节点上线，提交 nodekey、capabilities | 一次 |
| `POST /machine/<machinekey>/map` | 拉网络地图（peer list, ACL, DNS, derpmap） | 上线后 + 变化时 |
| `POST /machine/<machinekey>/poll` | long-poll 等待网络地图变化 | 持续 |
| `POST /machine/<machinekey>/register` | OAuth 风格首次注册（认证流） | 一次 |
| `GET /derp/...` | DERP relay 协议（websocket） | 持续 (用户 DERP 流量) |
| `GET /a/login` | 浏览器登录回调（OAuth-style） | 一次/会话 |

**最复杂的是 OAuth 登录流**：Tailscale 客户端跑 `tailscale up`，会浏览器打开
一个 URL（来自我们 server），用户登录后 server 颁发 token。Headscale 通过
"PreAuthKey" 简化为：用户在 web UI 提前生成 key，客户端 `tailscale up
--authkey=tskey-xxx` 直接 bind，跳过浏览器。

**这跟我们 wg-mac 的 token 模型几乎一样**——可以直接映射：
- Polar `wg_tokens.token_hash` ↔ Headscale `PreAuthKey.hash`
- Polar `wg_tokens.hub_id` ↔ Headscale `PreAuthKey.namespace_id`
- Polar `wg_tokens.role` ↔ Headscale 不需要（自动设备角色）

---

## 4. 分阶段

| 阶段 | 工作 | LOC | 工期 |
|---|---|---|---|
| **C1. Vendor + 路由分流** | go.mod 引入 headscale；在 polar-dock 主路由上挂 `/machine/*` `/derp/*` 转给 headscale handler | ~200 LOC + go.mod | 1 周 |
| **C2. Schema 适配**（B1 方案） | headscale.* 表跟我们 schema 并存；headscale config 改成连同一个 Postgres | ~100 LOC | 1 周 |
| **C3. Token 桥接** | Polar 创建 wg_tokens 时，同时在 headscale 创建对应 PreAuthKey；reusable/ephemeral 翻译 | ~400 LOC | 2 周 |
| **C4. 设备视图统一** | `/wg-tokens.html` 的 💻 Devices 标签把 headscale Machines 也列出来（标识"via Tailscale client"） | ~300 LOC backend + ~200 LOC UI | 1-2 周 |
| **C5. Hub ↔ namespace** | 我们的 hub 概念 = headscale namespace（每个 namespace 一个 mesh）；UI 让 admin 在创建 hub token 时选 namespace | ~300 LOC | 1 周 |
| **C6. DERP 配置** | headscale 内置 DERP server；操作员可在 Hub 配置启用本机当 DERP | ~150 LOC + nginx config | 1 周 |
| **C7. 文档 + 烟雾测试** | `tailscale up --login-server=https://zen.4950.store --authkey=…` 跑通；写 doc/wg-mac-tailscale-howto.md | — | 1 周 |

**合计**：8-10 周，~1700 LOC 集成代码 + 大量 vendor 体积。

---

## 5. 不可绕过的设计 trade-off

### 5.1 ACL 模型冲突

Headscale 用 Tailscale 的 HuJSON ACL（按 tag 互通规则）；我们当前**没 ACL**。
两种选择：
- **A**: 把 Headscale 的 ACL JSON 暴露在 admin UI，wg-mac 客户端也读这个 ACL 来过滤 peer list
- **B**: 维持现状（全 mesh 互通），ACL 留 v2

推荐 **B 起步**——做到"能连"就发版，ACL 是后续值。

### 5.2 Mesh CIDR 冲突

Tailscale 默认 `100.64.0.0/10`，我们 Phase 3 也改成这个 → **天然兼容**。✅

### 5.3 设备身份

Tailscale machinekey != wg pubkey。Tailscale 用一个 separate machinekey
做 Noise 握手，nodekey 是 wg pubkey。要在 wg_devices 上加一列
`tailscale_machinekey TEXT` 区分两种 client 来源。

### 5.4 DERP 寻址

Tailscale 客户端 expect 一个 DERP map (`derpmap` in network map)。即使我们
用 hub 直连，也必须给 derpmap 返回 placeholder（headscale 默认行为 OK）。

---

## 6. 风险

| 风险 | 概率 | 缓解 |
|---|---|---|
| Tailscale protocol bump 破坏兼容 | 中（每 1-2 月一次 CapVer） | 跟 Headscale main 同步，他们追 |
| Headscale 自己 schema 改动 | 中 | 锁版本（go.mod pin），分批升级 |
| 我们的 wg-mac 客户端跟 TS 客户端见到对方时 peer list 不一致 | 高 | C4 阶段重点测；让 wg-mac 通过 adapter 也看 headscale 的 Machines |
| DERP 流量被 nginx 反代干扰 | 中 | 单独的 DERP 端口（不走 :443 nginx），或 nginx stream module 透传 |
| 用户混用产生混乱 | 低 | UI 上明确标识 source（"wg-mac client" / "Tailscale client"） |

---

## 7. 反对意见

- **Tailscale 协议是 moving target**：你跟得起吗？Headscale 30K LOC 还在 chase
- **Tailscale 公司可能加防护**：未来某 CapVer 加证书 pinning / Tailscale.com 强校验 → 我们停服
- **DERP 流量带宽成本**：Tailscale 客户端有时把流量塞 DERP，作为运维你出口带宽要管理
- **License**：Headscale MIT，OK；Tailscale 客户端是 BSD，OK；都能商用
- **我们 wg-mac 客户端反而少了**：用户都用 Tailscale 客户端 → 我们 C/Swift 客户端价值缩水

**值不值得做的核心问题**：你想做"自托管 mesh VPN 产品"还是"自己写客户端的 mesh VPN"？

| 取舍 | 推荐 |
|---|---|
| 想最大化用户（让人用熟悉的 TS 客户端） | **做** Phase 11 |
| 想守住技术差异化（自己的 C 客户端） | **不做**，专注 wg-mac 体验 |
| 都要 | **做** Phase 11，wg-mac 客户端继续打苹果生态独家集成（Swift NE 深度集成是 TS 客户端做不到的） |

---

## 8. 决策时间表

- **现在**（这次 PR）：本设计落盘，零代码
- **+ 1 周**：确认要做后，单独开个 spike PR：vendor headscale，跑通最小 `tailscale up --login-server=https://localhost`
- **+ 4 周**：C1-C3 完成，能从 wg-tokens.html mint token 给 TS 客户端用
- **+ 8 周**：C4-C5 完成，设备视图统一
- **+ 10 周**：DERP + 文档，发版

---

## 9. 备选：从零自写

如果决定不 vendor Headscale（"不依赖第三方"理由），最小 MVP 工作量估算：

| 模块 | LOC | 工期 |
|---|---|---|
| Noise 协议握手（控制面） | ~1500 LOC | 4 周 |
| `/machine/<pubkey>/{key,map,register,poll}` 四套 endpoint | ~3000 LOC | 6 周 |
| Network map JSON 生成 | ~1000 LOC | 2 周 |
| DERP server | ~2000 LOC | 4 周 |
| 浏览器登录 OAuth flow | ~600 LOC | 1 周 |
| CapVer 升级追 Tailscale | 持续 | n/a |
| **合计** | **~8K+ LOC** | **17+ 周** |

**结论**：从零写 = 4 个月起步，**不推荐**。

---

## 10. 相关文档

- `doc/wg-mac-vs-tailscale.md §3 #11` — 决策入口（已翻转）
- `doc/JOIN_PROTOCOL.md` — 我们当前 wg-mac 协议
- `doc/wg-mac-api.md` — 我们当前 /v1/* API
- [Headscale](https://github.com/juanfont/headscale) — 拟 vendor 的开源 Tailscale 控制面
- [Tailscale CapVer 文档](https://tailscale.com/blog/coordination-server-api) — 官方协议参考

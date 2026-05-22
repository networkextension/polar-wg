# wg-mac vs Tailscale — 设计对比 + 路线决策参考

> 用来回答两个问题：
> 1. 我们差 Tailscale 多远？（**结论：很远，但 fit-for-purpose 时够用**）
> 2. 下一步该补哪个 gap？（按下面的 §3 优先级表挑）
>
> **本文档基于 2026-05-17 对 `/Volumes/Tahoe/Users/apple/Codex/wg`
> 的实地扫描。** 仓库里实际存在：macOS NE App Extension、macOS
> System Extension、iOS xcframework（slice 已校验）、Android
> Kotlin/JNI 客户端（raw VPN 能跑，未接 JOIN）、`scripts/join.sh`
> 成熟一键安装。把这些当成事实基线，不要凭记忆判断"有没有"。

---

## 1. 一句话结论

Tailscale 在**产品深度**上完胜——NAT 穿透、ACL、MagicDNS、多端、HA
都不是周末能补齐的。我们的 wg-mac + Polar 控制面是**"受控 mac 内
网 mesh"** 的精简方案，**别拿去打 Tailscale 的市场**，但作为：

- ≤ 50 台 mac
- 单一信任域（一个 admin、一组运维）
- 至少一台机有公网 IP / DDNS
- 客户端要可控（自己的 C 内核 + Swift NE）

——这种场景，**我们的更简单 / 更可改 / 完全自托管**，够用。

---

## 2. 维度对比

### 2.1 Tailscale 完胜的轴

| 维度 | Tailscale | 我们 | 差距 |
|---|---|---|---|
| **NAT 穿透** | DERP + STUN/ICE 自动打洞 | 无：要么同 LAN，要么 hub 公网 | **碾压** |
| **跨区域转发** | 任一节点可能转 relay；全球 30+ DERP | 单 hub 转发；hub 挂全网瞎 | **碾压** |
| **identity** | OIDC/SSO + tags + 细粒度 ACL | admin 手发 opaque bearer token | 大 |
| **多端** | macOS/Win/Linux/iOS/Android/FreeBSD/Synology/容器 | macOS (NE AppExt + SysExt) + iOS xcframework + **Android (Kotlin/JNI 已实证 raw VPN)**；Win/Linux 未做但 `macos_stubs/sys/callout.h` 已预留接口 | 中（不像我以前说的那么糟）|
| **MagicDNS** | `ssh yarshure-mac` 直接用 | 死记 `10.88.0.2` | 中 |
| **roaming** | 端点持续探测，切网无感 | 等下一个 `/v1/peers` 周期（默认 300s） | 中 |
| **subnet router** | 节点可 advertise `192.168.5.0/24` 当透传网关 | 没有；hub 不能做内网网关 | 中 |
| **exit node** | 一键把所有流量走某节点出去 | 没有 | 中 |
| **ACL / 网络策略** | JSON 写规则，按 tag 隔离 | 全 mesh 互通 | 中 |
| **HA 控制面** | 控制面分布式 | 单 Polar dock，挂了大家都没法 register/refresh | 中 |
| **scale** | 实测百万节点 | 单 hub 一个 /24，254 设备封顶 | 中 |
| **mesh CIDR** | `100.64.0.0/10` CGNAT（几乎不撞内网） | 默认 `10.88.0.0/24`，会撞用户家里的 10.x | 小 |
| **手机端** | iOS/Android 原生 app | Android raw VPN ✅（但还**没接 JOIN 协议**，目前要手工写 conf）；iOS xcframework 准备好待 TestFlight | 中 → 小（写一个 Android 端的 register/heartbeat 客户端就能补齐）|
| **审计 / 流量视图** | 完整 audit log + 流量统计 | 只有 heartbeat | 小 |

### 2.2 我们扳回来的几个点

| 维度 | 我们 |
|---|---|
| **自托管** | Polar 一套全搞定，不依赖 SaaS（TS 的 OSS 替代是 Headscale，要自己运维一摊）|
| **客户端可控** | wg_core/wgctl 是自己的 C 代码，能改 internals（max_peers、加密细节、Apple 特定优化）|
| **简单** | Polar ~1700 行 Go + 6 张表；wg-mac 客户端清晰；会 Go/C 的人一下午能读完 |
| **垂直集成** | install.sh + bundle 由 dock 自己服务，没第三方 |
| **苹果生态适配** | 两条苹果发布路径都打通：**NE App Extension**（App Store / TestFlight）+ **NE System Extension**（Developer ID，企业内分发，权限更高、survives app exit）|
| **Android 客户端在仓库里** | Kotlin + JNI 桥 (`WireGuardAndroid/app/.../tunnel/WgSessionBridge.kt`) + `VpnService` 子类，C 核共享 |

---

## 3. 优先级 gap 表（按 ROI 排序）

每行：**做这个的代价 vs 解锁的价值**。决定下一步看这表。

| # | Gap | Severity | 工作量估算 | ROI | 建议 |
|---|---|---|---|---|---|
| 1 | **mesh_cidr 默认改 `100.64.0.0/10` 切片** | 低 | 1 PR，~50 LOC | 高 | **做**。改默认 CIDR 抽 RFC 6598 段，避免家里 10.x 撞车；零客户改动 |
| 2 | **device 心跳频率提高 + 同步 `/v1/peers` 触发** | 中 | 1 PR，server 给 webhook / SSE，~200 LOC | 高 | **做**。roaming 体验从分钟级降到秒级 |
| 3 | **MagicDNS — `/etc/resolver/<hub>.lan` + `.wg` 后缀** | 中 | wgctl-agent 写 `/etc/resolver/<slug>` + dock 出 hostname → wg_ip 映射；~150 LOC server + ~100 LOC agent | 高 | **做**。操作员日常用感巨大提升 |
| 4 | **subnet router**（节点 advertise CIDR） | 中 | token 加 `advertised_routes` 字段；register 时校验 + 写入 wg_devices；peer list 时把它当 AllowedIPs；agent 端打开 forwarding；~300 LOC | 高 | **做**。让内网服务（NAS / 打印机 / 内部站）零改动接进 mesh |
| 5 | **token rotation 自动化** | 低 | 客户端在过期前 80% 自动 POST `/v1/token/refresh`；wgctl-agent 加个状态机；~80 LOC | 中 | **做**。防止 90 天后操作员失联 |
| 6 | **简单 ACL：tag → tag 可见性** | 中 | token 加 `tags[]`；register 时 device 继承；peer list 按 tag 过滤；~400 LOC + UI ~200 LOC | 中 | **可做**。M+ 团队多套环境时必要（dev/prod 隔离）|
| 7 | **DERP-lite：第二个 hub 当冷备 relay** | 高 | 协议级改动；多 hub 互通；客户端在主 hub 不可达时切到备 hub；~800 LOC + 客户端逻辑 | 中 | **能拖就拖**。单 hub 平均可用性其实够用；做 HA 复杂度暴涨 |
| 7.5 | **Android 接 Polar JOIN 协议** | 中 | wg-mac Android 客户端已有 wg_session JNI 桥，需补一个 OkHttp/Ktor 客户端打 `/v1/register` + 解析响应渲染 wg conf + 周期 `/v1/peers`；类比 wgctl-agent.sh 但用 Kotlin。~400 LOC + 1 周。 | **极高** | **做**（如果场景里有 Android）。已有 raw VPN 实证，缺的只是 JOIN 客户端 |
| 8 | **iOS / iPadOS 公测** | 中 | xcframework 已就绪、Makefile `build-ios` 已验证 iOS slice，主要工作是 TestFlight 流程 + Keychain + handler app messages；~1-2 周 | 中 | **看场景**。手机端要不要在你的客户场景里有 |
| 9 | **完整 NAT 穿透（STUN + 双向连接尝试）** | 极高 | 协议级；客户端做 endpoint 候选探测；服务端 collate；~3000+ LOC，工程量大半年 | 低 | **不做**。这是 Tailscale 公司核心 IP，自己做要付出极大代价但回报有限——直接告诉用户"hub 公网 IP 必备" |
| 10 | **Linux 客户端** | 中 | C 核 + Android JNI 已经把 stub 路径走通了，Linux 只需要一个 daemon 壳（select+TUN）+ JOIN 客户端；不像我之前以为的"整套新移植"。~800 LOC | 中 | **可做**（看是否需要把 Linux 服务器 / NAS 接入 mesh）|
| 10b | **Win 客户端** | 极高 | 跟 Linux 不同，没有 stub 预留，要重做 wintun + Windows service + GUI | 低 | **不做** |
| 11 | **Tailscale 客户端兼容（vendor Headscale）** | 高（8-10 周 / ~1.7K LOC + 大量 vendor 体积） | vendor `juanfont/headscale` 入 Polar dock；映射 wg_tokens ↔ PreAuthKey、wg_hubs ↔ namespace；操作员把 `--login-server=https://our.example` 一改，原版 Tailscale 客户端直接加入我们的 mesh。设计：[`doc/wg-mac-tailscale-compat-design.md`](./wg-mac-tailscale-compat-design.md) | **极高** | **做**（决策已翻转 2026-05-18）。产品定位升级为"自托管 Headscale + 我们自己的 macOS 客户端"双轨——TS 客户端拉新最低摩擦，wg-mac 走苹果生态深度集成（Swift NE）差异化 |
| 12 | **wg_devices.metrics → Prometheus** | 低 | gauge `polar_wg_devices_alive{hub}`、histogram bandwidth；接 grafana ~100 LOC | 中 | **做**。运维体感巨大提升，便宜 |
| 13 | **bundle 签名（minisign / cosign）** | 中 | 上传时存 signature；install.sh 验证；~150 LOC + key 管理 | 低 | **看安全要求**。当前 HTTPS 已经挡 MITM，签名只挡 dock 被入侵后的供应链攻击 |
| 14 | **stale device GC** | 低 | 后台 goroutine，`last_seen_at > 7d` 自动 mark removed；~50 LOC | 低 | **做**。一次性写完省心 |

---

## 4. 推荐三个月路线（按 §3 ROI 排）

**Sprint 1（2 周）—— 体验提升**
- #1 默认 CIDR → 100.64.x.0/24
- #5 token 自动轮换
- #12 Prometheus metrics
- #14 stale device GC

打底，让现有用户体感稳定。

**Sprint 2（4 周）—— 实用功能**
- #3 MagicDNS（最高频日常用）
- #4 subnet router（"我家 NAS 在 192.168.5.10" 直接进 mesh）
- #7.5 Android JOIN 客户端（wg-mac 已有 JNI 桥，写个 Kotlin 端的 register+poll；让手机能一键入网）

这三个做完，Polar+wg-mac 在自己场景下基本不输 Tailscale。

**Sprint 3（4 周）—— 弹性**
- #2 近实时 peer list 推送（SSE 或 long-poll）
- #6 ACL（按 tag 隔离 dev/prod/visitor）

到这里基本可以打 "Tailscale 替代品（受控场景）" 的旗号。

**之后**：根据用户回声决定是否做 #8（iOS）或彻底放弃 #9-#11。

---

## 5. 决策点（你要拍板的）

| 问题 | 选项 A | 选项 B |
|---|---|---|
| 客户端覆盖 | **macOS + iOS + Android**（仓库里都有，缺的是 Android 的 JOIN 客户端 + iOS TestFlight 流程，不是从零）| 也做 Linux（callout stub 已预留，写个 daemon 壳 + JOIN 客户端 ~800 LOC，看是否需要 Linux 服务器入网）|
| NAT 穿透 | **永远要求 hub 有公网 IP**（写进文档 + UI 警告）| 啃 STUN/ICE（半年起步）|
| 商业模型 | **永远自托管 / OSS / 不上 SaaS**（差异化）| 跟 Tailscale 卷 SaaS（不建议）|
| 标杆 | **Headscale**（自托管 TS 替代）| Tailscale（云）|

我的建议：A / A / A / Headscale。坚持"我们是 macOS-first 的 Headscale + 你自己的内核"，路线清晰，跟 Tailscale 不直接竞争。

---

## 6. 相关文档

- `doc/JOIN_PROTOCOL.md` — 入网协议设计（wg-mac 客户端侧）
- `doc/wg-mac-api.md` — Polar 服务端 /v1/* API 完整参考
- `doc/playbook.md §12` — 多 hub + role-aware token 运维步骤
- `doc/wg_cloud.md` — wg-mac 客户端仓库总览（local-only, gitignored）

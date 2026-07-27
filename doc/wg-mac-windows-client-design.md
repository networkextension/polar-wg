# wg-mac Windows 客户端 — design

> **修订 2026-07-27。** 原稿（Phase 10b，从 Polar 单体仓提取时带过来）的核心判断
> ——不移植 wg 内核、复用官方 wireguard-windows、我们只写一个配置代理——**依然成立**。
> 但其中五处技术前提已经过时或经核查不成立，**全部朝更便宜的方向变**，工期估算
> 因此下修。选型也有一处改变（agent 语言 Go → Swift）。
>
> 逐条修订见 §10，原稿被推翻的论断保留在那里，便于回溯当时的判断依据。
> 决策入口仍是 `doc/wg-mac-vs-tailscale.md §3` #10b。

---

## 0. 背景 + 评估

**为什么需要 Windows 客户端**：客户场景里有 Win 设备要入网；目前用户得手工配
WireGuard for Windows 官方客户端，自己写 conf，缺少 mesh 的 auto-join / refresh 体验。

**为什么完全自研很贵**（原稿判断正确，保留）：

- 没有 `macos_stubs/sys/callout.h` 那种现成 stub —— 完全没移植 wg 内核到 Windows userspace
- 苹果系统服务（launchd / NetworkExtension / Keychain）在 Win 全无对应
- 数据面接口与 macOS utun 差很多
- 分发：MSI / WinGet / SCCM / GPO 多种 channel

**完全重写预估**：12-18 周一个工程师，~6-8K LOC。所以不走这条路。

**复用路线（本文推荐）修订后预估**：见 §3，约 **2-3 周**。原稿估 4-6 周，下修的原因
是 §10 那几条——名字管道 UAPI 不用写、DPAPI 不用碰、托盘 UI 不用做、服务端已就绪。

---

## 1. 栈选型

| 维度 | 选项 A | 选项 B | 选项 C | 决定 |
|---|---|---|---|---|
| **wg 数据面** | 复用 `libwg.a` + 写 Windows stubs | 直接用 `wireguard-windows`（官方） | 自己写 Rust 实现 | **B**（省掉 80% 工作量；内核驱动签名也不是我们能扛的） |
| **agent / register 客户端** | C + libcurl | Go + net/http | **Swift**（与 POSIX 侧同一套） | **Swift** ← 已改，见 §10.1 |
| **TUN / 数据面驱动** | — | — | — | **WireGuardNT**（官方 MSI 自带，我们不接触）← 已改，见 §10.2 |
| **UI / 系统托盘** | 自己写 | — | — | **不做**，用官方 UI ← 已改，见 §10.4 |
| **服务管理** | Windows Service (SCM) | 任务计划程序 | — | **Windows Service**，GPO 禁止时降级到任务计划 |
| **分发** | MSI | MSIX / WinGet | PowerShell 脚本 | **MSI**，后续可上 WinGet |

**路线 B 的本质**：wireguard-windows 已经解决了内核驱动、Windows Service、托盘 GUI、
配置加密这四件最难的事，而且是 BSD 许可、官方维护。我们只在它之上加一个**配置代理**：
从 Polar JOIN 协议拿 peer 列表 → 渲染 conf → 交给它。

---

## 2. 架构

```
┌──────────────────────────────────────────────────────────┐
│ polar-wg-agent.exe   (我们写的唯一一个东西)               │
│                                                          │
│   WGAgentCore   ← 与 macOS/Linux/FreeBSD 完全同一份代码   │
│     长轮询状态机 / conf 渲染 + 语义比较 / 端点漂移判定     │
│     心跳组装 / 状态解析      ← 零平台 API，136 个测试共享   │
│                                                          │
│   WGPlatform (Windows 实现)                              │
│     ProcessRunner  → CreateProcessW + WaitForSingleObject│
│     HostFacts      → GetAdaptersAddresses / RtlGetVersion│
│     Resolver       → getaddrinfo (ws2_32) + WSAStartup   │
│     TunnelControl  → wg.exe set / sc.exe restart         │
│     Runtime        → 命名互斥体；看门狗需重想（无 SIGALRM）│
│     HTTPTransport  → 未定，见 §5 开放问题                 │
└──────────────────────────────────────────────────────────┘
                    ↓  wireguard.exe / wg.exe / sc.exe
┌──────────────────────────────────────────────────────────┐
│ WireGuard for Windows（官方 MSI，BSD，我们不改一行）       │
│   wireguard.exe /installtunnelservice <conf>             │
│     → Windows 服务 WireGuardTunnel$<name>                 │
│   manager service → 系统托盘 UI + conf 的 DPAPI 加密      │
│   WireGuardNT → 内核数据面                                │
└──────────────────────────────────────────────────────────┘
```

**关键点：Windows 与 Linux 走同一条控制路径。** 标准 `wg(8)` 工具在 Windows 上可用，
所以读状态是 `wg show <iface> dump`、换端点是 `wg set <iface> peer … endpoint …`
——和 Linux/FreeBSD 分支逐字相同。只有"路由变了要重启"这一步从 `wg-quick down/up`
换成 `sc stop/start WireGuardTunnel$<name>`。

### 2.1 需要写的 Windows 平台实现

`WGAgentCore` 经核查**零平台 API**（无任何 Glibc/Darwin/Musl/posix_/sockaddr 引用），
Windows 上原样编译。要动的只有 `WGPlatform` 六个文件里的四个：

| 文件 | 当前 POSIX 调用 | Windows 对应 | 评估 |
|---|---|---|---|
| `Resolver.swift` | `getaddrinfo` | **同名存在**（ws2_32），补 `WSAStartup` + 换 import | 几乎白送 |
| `ProcessRunner.swift` | `posix_spawn` / `waitpid` / `dup2` | `CreateProcessW` + `WaitForSingleObject`，输出重定向走 `STARTUPINFO` | 直接 |
| `HostFacts.swift` | `getifaddrs` / `uname` / `sysctl` | `GetAdaptersAddresses` / `RtlGetVersion` / `GetTickCount64` | 直接 |
| `Runtime.swift` | `flock` / `alarm` / `signal` / `sockaddr_un` | 命名互斥体替代 flock；**无 SIGALRM，看门狗需另设计**；`sockaddr_un` 那段 Windows 不需要（它服务于 wg_core 状态套接字，Windows 读 `wg.exe show dump`） | 看门狗要重想 |
| `PublicIP.swift` | 已是纯 Swift | 只依赖 `PublicIPFetcher` 协议 | 已就绪 |
| `TunnelControl.swift` | 已是纯 Swift | 新增一个 conforming 的 redialer（`sc.exe` / `wg.exe set`），协议不变 | 已就绪 |

`TunnelControl` 现已有 `WGSetRedialer`（Linux/FreeBSD）和 `LaunchdRedialer`（macOS），
Windows 只是第三个实现——这层接口在设计时就是为此准备的。

---

## 3. 分阶段

| 阶段 | 工作 | 工期 |
|---|---|---|
| **W0 — 构建主机 + 冒烟** | Windows 机器装 Swift 工具链；确认 `WGAgentCore` 原样编译 + 136 个测试通过；**确定 HTTP 栈**（§5 唯一未知数）；验证 `wg.exe` 实际路径与权限 | 1-2 天 |
| **W1 — register + 一次性 conf** | PowerShell 安装脚本（`/v1/install/win`）；`polar-wg-agent register --token=…`；POST /v1/register；渲染 conf；`wireguard /installtunnelservice` | 1 周 |
| **W2 — 周期性 reconcile** | 注册成 Windows Service；心跳 + 长轮询 peers；conf 语义 diff 后 `wg set` 或重启服务；端点漂移检测 | 1 周 |
| **W3 — ~~系统托盘 UI~~** | **取消**，见 §10.4 | — |
| **W4 — MSI + 签名（可选）** | WiX 打 MSI；EV 证书 ~$300/年。**仅当面向终端用户下载时才需要**；GPO/SCCM 推送的托管场景不触发 SmartScreen | 1 周 |

**合计 W0+W1+W2：约 2-3 周。** W4 视分发方式决定。

---

## 4. Polar 服务端改动

**大部分已经就绪**（原稿写此文时尚未完成，现已存在）：

- ✅ `normWGOS()` 已识别 `windows` / `win`（`internal/wg/bundle_assets.go:79`）
- ✅ `wg_bundles` 有 os/arch 列 + per-(os,arch) 的 `is_latest` 选取，`getLatestWGBundleFor()` 带 arch 回退
- ✅ `wg_devices.os` 字段已存在

**仍需要**：

1. `/v1/install/win` —— PowerShell 版安装脚本渲染器（现有 `install_script.go` 的 bash 模板旁边加一个）
2. admin UI 加 platform 列（可选）

**~150 LOC。**

---

## 5. 开放问题（唯一真正的未知数）

**Swift 在 Windows 上的 HTTP 栈未确定。** 已查证：Windows 是 Swift 官方支持的
dev+deploy 平台，2026 年 1 月成立了 Windows Workgroup；但 **FoundationNetworking /
URLSession 在 Windows 上的当前可用状态查不到权威结论**（历史上 Swift 5.5 时代
Windows 缺 `URLRequest`/`URLSession`，之后的状态无明确说法）。

不猜测。这是**有了 Windows 机器一小时内就能证伪**的事，列为 W0 验收项。

三条备选，按优先级：

1. `FoundationNetworking` 的 URLSession —— 若 Windows 上可用且不像 Linux 那样
   busy-spin，最省事
2. **libcurl via vcpkg** —— 与 musl/Linux 分支共用同一份 `CCurl` systemLibrary target，
   跨平台一致性最好
3. **WinHTTP via C interop** —— 系统自带无依赖，但要单独写一份实现

无论选哪条，`HTTPTransport` 协议不变，`WGAgentCore` 不受影响。

---

## 6. 风险 + caveat

- **wireguard-windows 维护风险**：官方仓库仍活跃。万一停更，要么维护 fork，要么
  重新评估。这是路线 B 的根本性依赖，值得每年复核一次。
- **EV 代码签名证书**：~$300/年。**仅在面向终端用户下载分发时才是问题**——
  Defender SmartScreen 对未签名 installer 很不友好；GPO/SCCM 推送的托管场景不触发。
- **企业 GPO 可能禁止注册 Windows Service** → 降级到任务计划程序（与 POSIX 侧
  "launchd / systemd timer / cron 三选一"的模型一致）。
- **看门狗需要重新设计**：POSIX 侧用 `alarm(2)` + SIGALRM 强制退出，理由是当初的事故
  是 busy-spin，而 CPU 打满时睡眠线程会错过自己的 deadline。Windows 无 SIGALRM，
  需要等价物（可等待计时器 / Job Object 时间限制），不能简单退回睡眠线程。
- **`wg.exe` 的实际路径与权限模型未在真机验证**：目前只从官方 enterprise 文档推断
  （文档确认"标准 wg(8) 工具可用"但未给路径）。W0 验收项。

---

## 7. 替代方案

| 方案 | 评价 |
|---|---|
| **WSL2 跑 Linux 版** | 假装"我们做了 Win"——只支持 WSL，普通 Win 用户无用；NAT 还要桥 |
| **C# / .NET 客户端** | 与现有栈分裂；且会重新引入一份 protocol 逻辑，正是 `WGAgentCore` 要消除的东西 |
| **Go 客户端** | 原稿的推荐。见 §10.1——不是不可行，是会让 Windows 变成第二份实现 |
| **完全自己重写 wg 内核 → Win** | 12-18 周；除非需要对 wg 内核做苹果/中国特定改动，不该做 |
| **接 OpenVPN 替代** | 协议不同；mesh 设计要重做；放弃 |

---

## 8. 反对意见 — 为什么也许仍不该做

原稿这一节的逻辑依然成立，但**成本项已大幅下降**，门槛应相应下调：

- **客户场景未必需要**：如果客户都是苹果生态，零回报。这条不变。
- ~~**MS 生态摩擦**~~：DPAPI 不用碰了，wintun 重分发不存在了，SmartScreen 只在
  面向终端用户下载时才是问题。剩下的摩擦比原稿设想的小。
- **维护成本永久**：每个 Win Update 都可能影响数据面——但**那是官方客户端的责任
  不是我们的**，这正是路线 B 的价值。
- **机会成本**：原稿说 6 周；现在是 2-3 周，机会成本相应减半。

**修订后结论**：原稿的门槛是"Win 设备 < 5% 就别做"。按 2-3 周的新成本，这个门槛
可以放宽。但**决策依据仍是客户场景里 Win 设备的实际占比，不是技术可行性**——
技术上已经不难了。

---

## 9. 相关文档

- `doc/JOIN_PROTOCOL.md` — Win 客户端要遵守的 protocol
- `doc/wg-mac-api.md` — Win agent 调用的 /v1/* API
- `doc/wg-mac-vs-tailscale.md §3` #10b — 决策入口
- `polar-wg-app/doc/wg-agent-swift-design.md` — Swift agent 的分层、平台矩阵、
  工具链坑（**注意：该文档目前把 Windows 列为"不做"，需按本次决定同步更新**）
- [wireguard-windows](https://git.zx2c4.com/wireguard-windows/) — 复用的官方实现
- [enterprise.md](https://github.com/WireGuard/wireguard-windows/blob/master/docs/enterprise.md) — 静默安装 / 隧道服务 / wg(8) 可用性
- [adminregistry.md](https://git.zx2c4.com/wireguard-windows/about/docs/adminregistry.md) — `LimitedOperatorUI` 等策略

---

## 10. 本次修订逐条说明（2026-07-27）

### 10.1 agent 语言：Go → Swift

**原稿**：推荐 Go，"一致跨平台 Go 工具"。

**改为 Swift**。理由不是 Go 不好，而是 POSIX 侧的 agent 已在 Swift 重写中，且
`WGAgentCore`（长轮询状态机、conf 渲染与语义比较、端点漂移判定、心跳组装、状态解析）
**已实现且经核查零平台 API**，配 136 个测试。选 Go 意味着把这套 protocol 逻辑在
Windows 上再写一遍，并永久维护两份行为必须一致的实现——那正是最容易长期出错的地方。

曾有一条反对 Swift 的论据是"Windows 目标无法从 macOS 交叉编译"。**该论据已撤回**：
构建主机是可获取的资源，不是架构约束。W0 把它列为第一件事。

### 10.2 数据面：wintun.dll → WireGuardNT

**原稿**：TUN 接口是 wintun.dll，"没别的选"；风险项列了"wintun.dll 重分发要带
BSD attribution""每个 Win Update 都可能挂掉 wintun.dll"。

**现状**：wireguard-windows 的默认数据面早已是 **WireGuardNT**（内核驱动），wintun
是旧路径。且走路线 B 我们**根本不重分发任何驱动**——官方 MSI 自带。这两条风险
对我们基本消失。

### 10.3 conf 交付：名字管道 UAPI → `/installtunnelservice`，且 DPAPI 不用碰

**原稿**：设计为连 `\\.\pipe\ProtectedPrefix\Administrators\WireGuard\…` 自己实现
UAPI 客户端；并指出 conf 被 DPAPI 加密，"我们写的时候要走它的 API，不能直接写明文 conf"。

**核查结果**：官方 enterprise 文档给出受支持的简单路径：

```
wireguard /installtunnelservice C:\path\to\myconfname.conf
wireguard /uninstalltunnelservice myconfname
```

创建标准 Windows 服务 `WireGuardTunnel$myconfname`，`sc` / `services.msc` 可管。
**该命令接收的就是明文 .conf 路径**，manager 服务监视
`%ProgramFiles%\WireGuard\Data\Configurations\` 并自行加密成 `.conf.dpapi`
——DPAPI 不是我们的问题。

更重要的是：**标准 `wg(8)` 工具在 Windows 上可用**（`wg show` / `wg set`），
所以 Windows 复用与 Linux 相同的读写路径，不需要专用管道客户端。这是本次工期
下修最主要的一项。

### 10.4 托盘 UI（W3）：取消

**原稿**：W3 用 Go + walk 写系统托盘，~1200 LOC / 2 周。

**取消理由**：wireguard-windows 自带托盘 UI，已能显示隧道状态、启停。官方还提供
`HKLM\Software\WireGuard\LimitedOperatorUI` (REG_DWORD=1)，让 Network Configuration
Operators 组的用户看到一个**受限 UI**：可启停隧道，但看不到密钥、改不了配置、
不能触发更新——正好匹配"配置由我们托管、用户只负责开关"的形态。自己再写一个
是重复劳动。

（另注：`HKLM\Software\WireGuard\DangerousScriptExecution=1` 才会执行 conf 里的
`PreUp`/`PostUp`/`PreDown`/`PostDown`，默认关闭且以 Local System 权限运行。
我们的 conf 不依赖这些钩子，保持默认关闭。）

### 10.5 服务端：大部分已就绪

**原稿**：需要加 `?platform=win`、os 字段、admin UI 列，估 ~200 LOC。

**现状**：os 归一化、bundle 的 os/arch 列与 per-(os,arch) latest 选取、`wg_devices.os`
都已存在。只剩 `/v1/install/win` 的 PowerShell 渲染器。见 §4。

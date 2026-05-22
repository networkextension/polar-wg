# wg-mac Windows 客户端 — design (Phase 10b 设计文档)

> 极高工作量。本文先把栈选型、分发、UI、复用 vs 重写写清楚，**实现 PR 一个个来**。
> 决策：要不要砸钱做？参考 `doc/wg-mac-vs-tailscale.md §3` #10b。

---

## 0. 背景 + 评估

**为什么需要 Windows 客户端**：客户场景里有 Win 设备要入网；目前用户得
手工配 WireGuard for Windows 官方客户端，自己写 conf，缺少 mesh 的
auto-join / refresh 体验。

**为什么贵**：跟 macOS 不同：
- 没有 `macos_stubs/sys/callout.h` 那种现成 stub —— 完全没移植 wg 内核到 Windows userspace
- 苹果系统服务（launchd / NetworkExtension / Keychain）在 Win 全无对应
- Win 的 TUN/TAP 接口是 **wintun.dll**（Donenfeld 出品，BSD），跟 macOS utun 差很多
- 分发：MSI / WinGet / SCCM / GPO 多种 channel；UI 期望系统托盘 + Win 通知

**预估**：12-18 周一个工程师，~6-8K LOC。**确实**是 §3 表里说的"极高"。

---

## 1. 栈选型

| 维度 | 选项 A | 选项 B | 选项 C | 推荐 |
|---|---|---|---|---|
| **wg 内核** | 复用现有 `libwg.a` + 写 Windows stubs | 直接用 `wireguard-windows` (官方 Go 实现) | 自己写 Rust 实现 | **B**（80% 工作减免） |
| **agent / register 客户端** | C + libcurl | Go + net/http | Rust + reqwest | **B**（一致跨平台 Go 工具） |
| **TUN 接口** | wintun.dll | 不选 | 不选 | **wintun.dll** (没别的选) |
| **UI / 系统托盘** | C++/MFC | C# .NET 6 + WPF | Go + walk / fyne | **Go + walk**（编译单二进制，跟 agent 同栈） |
| **服务管理** | Windows Service (sc.exe) | scheduled task | UWP background | **Windows Service**（标准） |
| **分发** | MSI 安装包 | MSIX / WinGet | curl-bash 风格 (PowerShell) | **MSI** + 后续上 WinGet |

**强推 B 路线**：复用官方 `wireguard-windows`（已经有 wintun 集成 + Windows
Service + 系统托盘 GUI），我们只在它之上加一个**配置代理**：从 Polar JOIN
协议拿 conf → 写入 wireguard-windows 的 `Data\Configurations\` →
触发 wireguard-windows 服务 reload。

这把工作量从 12-18 周压到 **4-6 周**。

---

## 2. 推荐架构（基于复用 wireguard-windows）

```
┌────────────────────────────────────────────────────┐
│ Polar mesh client for Windows (polar-wg-agent.exe) │
│  ┌──────────────────────────────────────────────┐  │
│  │ Go service                                   │  │
│  │   - HTTP client (net/http) → POST /v1/register│  │
│  │   - timer → GET /v1/peers / POST /v1/heartbeat│  │
│  │   - render → write %ProgramData%\WireGuard\  │  │
│  │       Configurations\polar-<hub>.conf.dpapi  │  │
│  │   - notify wireguard-windows via UAPI named  │  │
│  │     pipe \\.\pipe\ProtectedPrefix\Administr… │  │
│  │     to reload                                │  │
│  └──────────────────────────────────────────────┘  │
│                  ↓ named pipe                       │
│  ┌──────────────────────────────────────────────┐  │
│  │ wireguard-windows (官方，BSD)                │  │
│  │   - tunnel.exe (Windows Service)             │  │
│  │   - manager.exe (system tray)                │  │
│  │   - wintun.dll → kernel TUN                  │  │
│  └──────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────┘
```

我们写的只有 `polar-wg-agent.exe`（Go 单二进制 + 一个 Windows Service
包装器）。wireguard-windows 当依赖。

---

## 3. polar-wg-agent.exe 分阶段

| 阶段 | 工作 | LOC | 工期 |
|---|---|---|---|
| **W1 — register + 一次性 conf 渲染** | curl-PS 安装脚本拉 polar-wg-agent.msi；安装后跑一次 `polar-wg-agent register --token=…`；POST /v1/register；渲染 conf；调 wireguard-windows 名字管道 add tunnel | ~800 LOC Go + ~200 LOC PowerShell installer | 2 周 |
| **W2 — periodic refresh service** | 注册成 Windows Service；timer 跑 /v1/peers + /v1/heartbeat；conf diff 后 reload tunnel | ~600 LOC Go + Service wrapper | 1 周 |
| **W3 — 系统托盘 UI** | walk 写托盘：状态（已入网 / 断开 / 错误）；快速 connect/disconnect；查看 wg_ip + peers；右键打开日志 | ~1200 LOC Go + 图标 | 2 周 |
| **W4 — MSI installer + 自动更新** | WiX 写 MSI；签名（EV cert ~$300/年）；自动更新通过 Polar 的 `/v1/bundle` 拉 .msi 验签替换 | ~400 LOC + WiX + cert | 1 周 |

**合计**：6 周，~3.4K LOC + WiX + EV cert。

---

## 4. Polar 服务端改动

最小：
1. `/v1/bundle` 接受 `?platform=win` query 返回 .msi（已有 multi-version
   能力扩展）。
2. install.sh 渲染器加一个 PowerShell 版本（`/v1/install/win`）。
3. `wg_devices.os` 字段已经存在（"darwin" / "windows" / "linux"），
   admin UI 加 platform 列即可。

**~200 LOC Polar 改动**。

---

## 5. 风险 + caveat

- **wireguard-windows 维护风险**：官方仓库还活跃；万一停更要么我们维护
  fork 要么转 Rust。
- **EV 代码签名证书贵**：~$300/年。MS Defender SmartScreen 对未签名
  installer 极不友好（用户拿到要点"more info → run anyway"）。
- **wintun.dll 重分发**：BSD，要在 EULA 里带 attribution。
- **DPAPI 加密 conf**：wireguard-windows 用 Windows DPAPI 加密 conf 里
  的私钥，我们写的时候要走它的 API（不能直接写明文 conf）。
- **企业 GPO**：管理员可能禁止 Windows Service 注册；提供"无服务模式"
  fallback（任务计划程序）。

---

## 6. 替代方案

| 方案 | 评价 |
|---|---|
| **WSL2 跑 Linux 版** | 假装"我们做了 Win"——其实只支持 WSL，普通 Win 用户没用；NAT 还要桥 |
| **写 C# .NET 客户端** | 跟 Go 栈分裂；Go 跨平台编译已经是 Polar 现状；不推荐 |
| **完全自己重写 wg 内核 → Win** | 12-18 周；本设计文档目录 §0 评估的"贵"方案；除非需要对 wg 内核做苹果/中国特定改动，不该做 |
| **接 OpenVPN 替代** | 协议不同；mesh 设计要重做；放弃 |

---

## 7. 决策时间表

- **现在**（Polar Phase 3 这个 PR）：本设计文档落盘；代码零。
- **+ 1 月**：客户场景里点名要 Win 入网的次数 ≥ 5 → 启动 W1。
- **+ 3 月**：W1+W2 ship 后，看 Win 设备增长 → 决定 W3 (托盘 UI)。
- **+ 6 月**：W4 自动更新 + EV 签名。

---

## 8. 反对意见 — 为什么也许不该做

- **客户场景未必需要**：如果客户都是 mac 团队 / 苹果生态，零回报
- **MS 生态摩擦**：Defender SmartScreen / EV 证书 / DPAPI 都是 wg-mac 这个团队没经验的坑
- **维护成本永久**：每个 Win Update 都可能挂掉 wintun.dll
- **机会成本**：6 周的人力可以做 §3 的 #3+#4+#7.5+#5+#12+#14 + 写完 #9 P1

**反对结论**：如果客户场景里 Win 设备 < 5%，**别做**；让用户用官方
wireguard-windows + 手工配 conf。如果 Win 是核心场景，**走 4-6 周
B 路线**，别陷入完全重写。

---

## 9. 相关文档

- `doc/JOIN_PROTOCOL.md` — 当前 protocol，Win 客户端要遵守
- `doc/wg-mac-api.md` — Win agent 调用的 /v1/* API
- `doc/wg-mac-vs-tailscale.md §3 #10b` — 决策入口
- [wireguard-windows](https://git.zx2c4.com/wireguard-windows/) — 拟复用的官方实现
- [wintun.dll](https://www.wintun.net/) — TUN 接口

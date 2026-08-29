# wg web — one-click SSH / screen-share remote access (design)

Status: proposed. Owner: polar-wg. Related: webmux (`webmux.4950.store`), wg-agent.

## Goal
From the wg admin web (the device list at `wg.4950.store`), let an operator **one-click into
any mesh device** — a browser SSH terminal or a screen-share (VNC/RDP) — with no manual
host/credential entry.

## Key enabler
`wg-svc` (and webmux) run **on the wg mesh** (co-located with hub `zen`, `10.88.0.1`), so the
backend has **L3 reachability to every device's `wg_ip`** over the encrypted tunnel. No inbound
NAT / port-forward to devices is needed. The browser only talks to the already-TLS'd, admin-gated
front door; the backend bridges into the mesh.

## Don't build a bridge — integrate webmux
`webmux` (zen: `/Users/local/github/webmux`, Node backend on `127.0.0.1:8181`, Vite SPA via
nginx `webmux.4950.store`) is already a **clientless remote-access platform**:
- `POST /api/sessions` — SSH / **mosh** / exec (node-pty), persistent + multi-viewer terminals
- `POST /api/vnc/sessions` — VNC screen-share (noVNC)
- `POST /api/rdp/sessions` — RDP (Windows)
- `/api/hosts` — host inventory registry (id, hostname, port, username, transport, key_id)
- `/api/keys` — SSH key registry (referenced by `key_id`)
- JWT auth (`jsonwebtoken`, 8h, owner-scoped sessions); append-only JSONL **audit log**

So terminal/VNC/RDP/mosh/persistence/audit are **done**. This feature is the integration glue +
a little device-side agent work.

## Architecture

```
wg web device list ──[SSH/Screen/RDP buttons]──▶ create webmux session ──▶ webmux UI (xterm/noVNC/RDP)
      │                                                  │ (on zen; reaches wg_ip over the mesh)
      └──[inventory sync]──▶ webmux POST /api/hosts      └── ssh/mosh/vnc/rdp → device wg_ip
wg-svc ──[poll action]──▶ wg-agent: install control-plane pubkey / enable screen-share on demand
```

### 1. Inventory sync: wg devices → webmux hosts
`wg-svc` pushes each device into webmux's host registry (on register/heartbeat, or a periodic
reconcile):
```
POST webmux/api/hosts
{ "hostname": "<wg_ip>", "port": 22, "username": "<ssh_user>",
  "transport": "ssh" | "mosh", "key_id": "<control-plane key>" }
```
→ webmux's host list mirrors the mesh device list. `ssh_user` + `transport` are reported by the
agent (heartbeat status). Cross-border devices default `transport: "mosh"` (survives the lossy
link — same rationale as the KCP work).

### 2. One-click launch (wg web → webmux API)
- **SSH**: `POST webmux/api/sessions { username, hostname:<wg_ip>, transport:"ssh", key_id }`
  → open/redirect `webmux.4950.store/#/session/<id>` (new tab or iframe in the wg UI).
- **Screen (VNC)**: (a) wg-svc issues an agent action to enable VNC + a one-time password →
  (b) `POST webmux/api/vnc/sessions { hostname:<wg_ip>, port:5900, password:<one-time> }`.
- **Windows (RDP)**: `POST webmux/api/rdp/sessions { hostname:<wg_ip>, ... }`.

### 3. SSO (wg = dock-admin auth; webmux = JWT)
Recommended: `wg-svc` mints a **webmux-compatible JWT** (shared `JWT_SECRET`) for the already
dock-verified admin → deep-link `webmux.4950.store/?token=<jwt>` (or set the cookie). Alternative:
a small webmux endpoint that trusts `wg-svc` server-to-server (HMAC) and pre-creates the session,
handing the browser a short-lived viewer link. Either way webmux scopes sessions by owner
(JWT `sub`) and audits them.

## Device-side (the only new wg-agent work)
1. **Install control-plane public key** into the device's `authorized_keys` (a dedicated
   `wgops` user recommended, optional `command=`/sudo restrictions). The **private key lives only
   in webmux's `/api/keys`** on zen. Done at join or via a poll action; rotation = re-install on
   next poll. → webmux SSHes by `key_id`, no per-device passwords.
2. **On-demand screen-share enable**: `wg-svc` → agent action `{action:"screenshare", ttl:600,
   nonce}` over the poll channel. Agent enables the OS screen server + sets a **one-time password**,
   reports `{vnc_port, vnc_secret, ready}`, and **auto-disables at TTL**.
   - macOS: built-in Screen Sharing (`launchctl`/`kickstart` ARD) + VNC password.
   - Linux: x11vnc / wayvnc / gnome-remote-desktop bound to `wg_ip:5900`.
   - Windows: built-in RDP (or a bundled VNC).

This reuses the existing poll channel (heartbeat / `/v1/peers`); it's a strong motivation to add
long-poll push (poll-only propagation is a known limitation).

## Security
- **admin-only + per-device ACL** (which admins → which devices); a per-device **opt-in flag
  `remote_access_enabled`** (default off; same pattern as egress opt-in). Optional **step-up/2FA**
  to open a session.
- **Ephemeral**: VNC/RDP enabled on demand, TTL-boxed, auto-disabled; one-time passwords;
  control-plane private key only on zen.
- **Audit**: webmux's JSONL log + a wg-side record (who/device/when/duration); optional session
  recording.
- **Kill switch**: revoke the control-plane key / disable screen-share fleet-wide.

## Connectivity (multi-hub reality)
webmux on zen reaches the home mesh `10.88.0.0/24` directly. The cloud mesh `100.64.0.0/24`
requires zen's cross-hub route. **v1: only devices on a mesh routable from the webmux host.**
v2/v3: jump through the owning hub (a lightweight relay on the hub, or webmux SSHes via the hub
as a `ProxyJump`).

## Phasing
- **v1 (SSH MVP)**: wg-svc → webmux host sync + SSO deep-link + wg web "SSH" button + agent
  installs the control-plane pubkey. One click → browser terminal. Home mesh only.
- **v2 (screen-share)**: agent on-demand VNC enable + one-time password → webmux VNC session.
  macOS first, then Linux.
- **v3**: Windows RDP, cross-hub reachability (ProxyJump via owning hub), per-device ACL UI,
  default `mosh` for cross-border devices, session recording.

## Open questions
- SSO: shared `JWT_SECRET` vs a dedicated `wg-svc`→webmux trust endpoint.
- `wgops` user + key restrictions vs root access for admin ops.
- Whether wg-svc pushes to webmux (`/api/hosts`) or webmux pulls the device list from wg-svc.
- Long-poll push vs 60s poll latency for the screen-share "enable" action.

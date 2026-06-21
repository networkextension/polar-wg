// /wg-tokens.html — Phase 2 admin UI for wg-mac multi-hub control plane.
//
// Tabs:
//   - 🌐 Hubs     — list of all hubs (slug/label/endpoint/cidr/status/bound device)
//   - 🔑 Tokens   — list + role-aware create modal + plaintext-once + revoke
//   - 💻 Devices  — list with hub column + force-remove
//   - 📦 Bundles  — list + upload + mark-latest

import {
  createWGHubLink,
  createWGTokenForDevice,
  createWGTokenForHub,
  deleteWGBundle,
  deleteWGHub,
  deleteWGHubLink,
  listWGBundles,
  listWGDevices,
  listWGHubLinks,
  listWGHubs,
  listWGHubStatus,
  listWGTokens,
  removeWGDevice,
  resetWGHubBind,
  revokeWGToken,
  setWGBundleLatest,
  updateWGDeviceEgress,
  updateWGHub,
  uploadWGBundle,
} from "./api/wg.js";
import { logout } from "@networkextension/polar-ui-common/api/session";
import { byId } from "@networkextension/polar-ui-common/lib/dom";
import { hydrateSiteBrand, hydrateSidebarFoot } from "@networkextension/polar-ui-common/lib/site";
import { mountPlatformNav } from "@networkextension/polar-ui-common/lib/sidebar";
import { bindThemeSync, initStoredTheme } from "@networkextension/polar-ui-common/lib/theme";
import { NEON, freshnessColor, nocGlowDefs, nocPanel } from "@networkextension/polar-ui-common/lib/neon-topo";
import type {
  WGBundle,
  WGDevice,
  WGHub,
  WGHubLink,
  WGHubIfaceNet,
  WGHubPeerSample,
  WGHubStatusRow,
  WGToken,
} from "./types/wg.js";

initStoredTheme();
bindThemeSync();

byId<HTMLButtonElement>("logoutBtn")?.addEventListener("click", async () => {
  await logout();
  window.location.href = "/login.html";
});
void hydrateSiteBrand();
void mountPlatformNav();
void hydrateSidebarFoot();

// ---- tabs ----

const tabs = ["status", "hubs", "tokens", "devices", "bundles"] as const;
type TabKey = (typeof tabs)[number];

function switchTab(target: TabKey): void {
  tabs.forEach((t) => {
    const cap = t.charAt(0).toUpperCase() + t.slice(1);
    byId<HTMLButtonElement>(`tab${cap}`)?.classList.toggle("lp-tab-active", t === target);
    const pane = byId<HTMLElement>(`pane${cap}`);
    if (pane) pane.hidden = t !== target;
  });
  // Status tab: refresh on every entry so operators see fresh peer data
  // without waiting for the 15s timer. The poller still runs while
  // hidden — this just avoids a stale paint on tab switch.
  if (target === "status") {
    void refreshStatus();
  }
}
tabs.forEach((t) => {
  const cap = t.charAt(0).toUpperCase() + t.slice(1);
  byId<HTMLButtonElement>(`tab${cap}`)?.addEventListener("click", () => switchTab(t));
});

// ---- formatters ----

function fmtRelative(iso?: string): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return iso;
  const ageSec = Math.floor((Date.now() - t) / 1000);
  if (ageSec < 60) return `${ageSec}s 前`;
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m 前`;
  if (ageSec < 86400) return `${Math.floor(ageSec / 3600)}h 前`;
  return `${Math.floor(ageSec / 86400)}d 前`;
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

// ---- Status tab (live hub peer state from dock /internal/v1/wg-peer-status) ----

const hubStatusGrid = byId<HTMLElement>("hubStatusGrid");
const hubStatusEmpty = byId<HTMLElement>("hubStatusEmpty");
const hubStatusSummary = byId<HTMLElement>("hubStatusSummary");
const hubStatusRefreshBtn = byId<HTMLButtonElement>("hubStatusRefreshBtn");
const hubTopology = byId<HTMLElement>("hubTopology");
const hubTopologyWrap = byId<HTMLElement>("hubTopologyWrap");
const hubViewToggle = byId<HTMLButtonElement>("hubViewToggle");
// Star-topology is the default view; the toggle flips to the legacy card list.
let topoView = true;

function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function fmtAgeSec(ageSec?: number): string {
  if (ageSec === undefined || ageSec === null || !Number.isFinite(ageSec)) return "—";
  if (ageSec < 60) return `${ageSec}s`;
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m`;
  return `${Math.floor(ageSec / 3600)}h`;
}

function handshakeClass(ageSec?: number): string {
  if (ageSec === undefined || ageSec === null) return "wg-status-handshake-stale";
  if (ageSec < 60) return "wg-status-handshake-fresh";
  if (ageSec < 300) return "wg-status-handshake-warn";
  return "wg-status-handshake-stale";
}

function truncPubkey(k?: string): string {
  if (!k) return "—";
  if (k.length <= 16) return k;
  return `${k.slice(0, 8)}…${k.slice(-6)}`;
}

// pubkey → device.name lookup. Populated by primePubkeyMap() on
// page load; falls back to truncPubkey when a peer isn't in the
// devices list (e.g. a hub-side leftover or a freshly joined client
// that hasn't been listed yet).
const pubkeyToName = new Map<string, string>();
const pubkeyToHostID = new Map<string, string>();
// device's assigned mesh IP (present even before first handshake, unlike the
// live wgctl allowed_ips which is empty until the peer connects).
const pubkeyToWGIP = new Map<string, string>();

// Cross-link to the polar-hosts module's detail page. Derives the hosts host
// from the current origin (wg.<env> → hosts.<env>) so it works in prod + dev,
// matching the platform-nav absolute-URL convention.
function hostsLinkURL(hostId: string): string {
  const host = location.host.replace(/^wg\./, "hosts.");
  return `${location.protocol}//${host}/hosts.html?id=${encodeURIComponent(hostId)}`;
}

async function primePubkeyMap(): Promise<void> {
  try {
    const { data } = await listWGDevices(false);
    for (const d of data.devices ?? []) {
      if (d.pubkey && d.hostname) pubkeyToName.set(d.pubkey, d.hostname);
      if (d.pubkey && d.host_id) pubkeyToHostID.set(d.pubkey, d.host_id);
      if (d.pubkey && d.device_ip) pubkeyToWGIP.set(d.pubkey, d.device_ip);
    }
    // Re-render status if it's already mounted, now that names exist.
    if (!byId<HTMLElement>("paneStatus")?.hidden) void refreshStatus();
  } catch {
    /* swallow — peer rows just show pubkey */
  }
}

function peerLabel(pubkey?: string): string {
  if (!pubkey) return "—";
  const name = pubkeyToName.get(pubkey);
  const trunc = esc(truncPubkey(pubkey));
  if (!name) return `<code title="${esc(pubkey)}">${trunc}</code>`;
  return `<strong>${esc(name)}</strong> <code title="${esc(pubkey)}" style="opacity:0.6; font-size:11px;">${trunc}</code>`;
}

function renderPeerRow(p: WGHubPeerSample): string {
  const handshakeCls = handshakeClass(p.handshake_age_sec);
  const handshakeAge = p.handshake_age_sec === undefined ? "never" : `${fmtAgeSec(p.handshake_age_sec)} ago`;
  return `
    <tr>
      <td>${peerLabel(p.public_key)}</td>
      <td><code>${esc(p.endpoint ?? "—")}</code></td>
      <td><code>${esc(p.allowed_ips ?? "—")}</code></td>
      <td class="${handshakeCls}">${esc(handshakeAge)}</td>
      <td>${esc(fmtBytes(p.bytes_rx ?? 0))}</td>
      <td>${esc(fmtBytes(p.bytes_tx ?? 0))}</td>
      <td>${p.keepalive_sec ? `${p.keepalive_sec}s` : "—"}</td>
    </tr>
  `;
}

// addrMask returns the /N suffix of an addr like "10.88.0.1/24" → 24.
function addrMask(cidr: string): number {
  const m = /\/(\d+)$/.exec(cidr);
  return m ? parseInt(m[1], 10) : -1;
}

// expectedSubnetForAddr derives the subnet route a properly-configured
// hub would have. "10.88.0.1/24" → "10.88.0.0/24". Returns "" if the
// addr can't be parsed cleanly.
function expectedSubnetForAddr(cidr: string): string {
  const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)\/(\d+)$/.exec(cidr);
  if (!m) return "";
  const oct = [parseInt(m[1], 10), parseInt(m[2], 10), parseInt(m[3], 10), parseInt(m[4], 10)];
  const mask = parseInt(m[5], 10);
  if (!Number.isFinite(mask) || mask < 0 || mask > 32) return "";
  // Apply mask: zero out (32-mask) low bits across the 4 octets.
  const bits = oct[0] * 0x1000000 + oct[1] * 0x10000 + oct[2] * 0x100 + oct[3];
  const m32 = mask === 0 ? 0 : (0xFFFFFFFF << (32 - mask)) >>> 0;
  const net = (bits & m32) >>> 0;
  const a = (net >>> 24) & 0xFF;
  const b = (net >>> 16) & 0xFF;
  const c = (net >>> 8) & 0xFF;
  const d = net & 0xFF;
  return `${a}.${b}.${c}.${d}/${mask}`;
}

// renderIfaceNetRow surfaces hub interface state + flags the
// "addr without matching subnet route" misconfig that causes peers
// to handshake but TX/RX stays 0 on the hub side. Skips IPv4 /32
// addrs since they don't imply a subnet route at all.
function renderIfaceNetRow(info?: WGHubIfaceNet): string {
  if (!info) return "";
  const addrs = info.addrs ?? [];
  const routes = info.routes ?? [];
  if (addrs.length === 0 && routes.length === 0) return "";
  const routeSet = new Set(routes);
  const missing: string[] = [];
  for (const a of addrs) {
    const mask = addrMask(a);
    if (mask <= 0 || mask >= 32) continue;
    const expected = expectedSubnetForAddr(a);
    if (expected && !routeSet.has(expected)) missing.push(expected);
  }
  const addrsHtml = addrs.length > 0
    ? addrs.map((a) => `<code>${esc(a)}</code>`).join(" ")
    : `<span class="dim">none</span>`;
  const routesHtml = routes.length > 0
    ? routes.map((r) => `<code>${esc(r)}</code>`).join(" ")
    : `<span class="dim">none</span>`;
  const warn = missing.length > 0
    ? `<div class="wg-status-stale-banner" style="margin-top:4px;">⚠ missing subnet route${missing.length > 1 ? "s" : ""}: ${missing.map((m) => `<code>${esc(m)}</code>`).join(" ")} — hub can't reply to peers, run <code>sudo route add -inet ${esc(missing[0])} -interface &lt;iface&gt;</code></div>`
    : "";
  return `
    <div class="wg-status-card-meta" style="margin-top:4px; font-size:12px;">
      addr ${addrsHtml} · routes ${routesHtml}
    </div>
    ${warn}
  `;
}

function renderHubCard(row: WGHubStatusRow): HTMLDivElement {
  const card = document.createElement("div");
  card.className = "wg-status-card";
  const headTitle = `<span class="wg-status-card-title">${esc(row.label || row.slug)}</span>`;
  const headSlug = `<code>${esc(row.slug)}</code>`;
  const headEndpoint = row.endpoint ? `<code>${esc(row.endpoint)}</code>` : "";
  const headIP = row.wg_ip ? `<code>${esc(row.wg_ip)}</code>` : "";

  let statusHtml = "";
  let peerTableHtml = "";

  if (!row.status) {
    statusHtml = `<div class="wg-status-card-status dim wg-status-empty-banner">no sample yet — hub agent not reporting</div>`;
  } else {
    const s = row.status;
    const peers = s.peers ?? [];
    const ageMs = Date.now() - new Date(s.recorded_at).getTime();
    const ageStr = Number.isFinite(ageMs) ? fmtAgeSec(Math.floor(ageMs / 1000)) : "?";
    const staleBadge = s.stale ? ` · <span class="wg-status-stale-banner">stale</span>` : "";
    statusHtml = `
      <div class="wg-status-card-status">
        ${s.peer_count} peers · iface <code>${esc(s.iface)}</code> · sample ${ageStr} ago${staleBadge}
      </div>
    `;
    if (peers.length > 0) {
      peerTableHtml = `
        <table class="wg-status-peer-table">
          <thead>
            <tr>
              <th>peer pubkey</th>
              <th>endpoint</th>
              <th>allowed_ips</th>
              <th>handshake</th>
              <th>rx</th>
              <th>tx</th>
              <th>keepalive</th>
            </tr>
          </thead>
          <tbody>
            ${peers.map(renderPeerRow).join("")}
          </tbody>
        </table>
      `;
    } else {
      peerTableHtml = `<div class="meta-subtitle" style="margin-top:6px;">no peers connected</div>`;
    }
  }

  const ifaceNetHtml = row.status?.extra?.iface_net
    ? renderIfaceNetRow(row.status.extra.iface_net)
    : "";

  card.innerHTML = `
    <div class="wg-status-card-head">${headTitle} ${headSlug}</div>
    <div class="wg-status-card-meta">
      ${headEndpoint ? `endpoint ${headEndpoint}` : ""}
      ${headIP ? `· wg ${headIP}` : ""}
    </div>
    ${statusHtml}
    ${ifaceNetHtml}
    ${peerTableHtml}
  `;
  return card;
}

// ---- Star topology (SVG) ----

// Neon handshake-age ramp, shared with the hosts panel via polar-ui-common.
function handshakeColor(ageSec?: number): string {
  return freshnessColor(ageSec);
}

type Pt = { x: number; y: number };

// renderTopology draws a star-of-stars: each hub is a star center, its live
// peers fan out as device spokes (colored by handshake freshness), and any
// peer whose pubkey matches ANOTHER hub is drawn as a hub↔hub interconnect.
function renderTopology(rows: WGHubStatusRow[]): string {
  const hubByPubkey = new Map<string, WGHubStatusRow>();
  rows.forEach((r) => {
    if (r.pubkey) hubByPubkey.set(r.pubkey, r);
  });

  const N = rows.length;
  const W = 1000;
  const H = N <= 1 ? 560 : Math.max(640, 380 + N * 40);
  const cx = W / 2;
  const cy = H / 2;
  const hubRing = N > 1 ? Math.min(W, H) * 0.3 : 0;

  const hubPos = new Map<number, Pt>();
  rows.forEach((r, i) => {
    const a = ((-90 + (360 * i) / N) * Math.PI) / 180;
    hubPos.set(r.id, { x: cx + Math.cos(a) * hubRing, y: cy + Math.sin(a) * hubRing });
  });

  const hubLinks: string[] = [];
  const spokes: string[] = [];
  const devNodes: string[] = [];
  const hubNodes: string[] = [];
  const seenLink = new Set<string>();

  rows.forEach((r) => {
    const hp = hubPos.get(r.id);
    if (!hp) return;
    const peers = r.status?.peers ?? [];
    const interHub = peers.filter(
      (p) => p.public_key && hubByPubkey.has(p.public_key) && p.public_key !== r.pubkey,
    );
    const devicePeers = peers.filter(
      (p) => !p.public_key || !hubByPubkey.has(p.public_key) || p.public_key === r.pubkey,
    );

    // hub ↔ hub interconnect links (dedup A-B / B-A)
    interHub.forEach((p) => {
      const other = hubByPubkey.get(p.public_key!);
      if (!other) return;
      const key = [r.id, other.id].sort((a, b) => a - b).join("-");
      if (seenLink.has(key)) return;
      seenLink.add(key);
      const op = hubPos.get(other.id);
      if (!op) return;
      hubLinks.push(
        `<line x1="${hp.x.toFixed(1)}" y1="${hp.y.toFixed(1)}" x2="${op.x.toFixed(1)}" y2="${op.y.toFixed(1)}" class="topo-hublink" stroke="${handshakeColor(p.handshake_age_sec)}" filter="url(#topo-glow)"><title>${esc(r.slug)} ↔ ${esc(other.slug)}</title></line>`,
      );
    });

    // device spokes fan outward from the hub
    const M = devicePeers.length;
    const outward = N > 1 ? Math.atan2(hp.y - cy, hp.x - cx) : -Math.PI / 2;
    const spokeR = Math.min(240, 95 + M * 5);
    const arc = N > 1 ? Math.PI * 1.25 : Math.PI * 2;
    devicePeers.forEach((p, j) => {
      const a =
        N > 1
          ? outward + (M === 1 ? 0 : (j / (M - 1) - 0.5) * arc)
          : (2 * Math.PI * j) / Math.max(1, M) - Math.PI / 2;
      const dp = { x: hp.x + Math.cos(a) * spokeR, y: hp.y + Math.sin(a) * spokeR };
      const col = handshakeColor(p.handshake_age_sec);
      const name = (p.public_key && pubkeyToName.get(p.public_key)) || truncPubkey(p.public_key);
      const hostID = p.public_key ? pubkeyToHostID.get(p.public_key) : undefined;
      const hs = p.handshake_age_sec === undefined ? "never" : `${fmtAgeSec(p.handshake_age_sec)} ago`;
      // WG IP shown on hover: prefer the device's assigned mesh IP (set at
      // register, survives "never handshaked"); fall back to the live
      // allowed-ips /32 if the peer isn't in the devices list.
      const wgip = ((p.public_key && pubkeyToWGIP.get(p.public_key)) || p.allowed_ips || "")
        .split(",")[0].split("/")[0].trim();
      const tip = `${name}\nwg ${wgip || "—"}\n${p.endpoint || "no endpoint"}\nhandshake ${hs}\nrx ${fmtBytes(p.bytes_rx ?? 0)} · tx ${fmtBytes(p.bytes_tx ?? 0)}${hostID ? "\n点击 → Hosts 详情" : ""}`;
      spokes.push(
        `<line x1="${hp.x.toFixed(1)}" y1="${hp.y.toFixed(1)}" x2="${dp.x.toFixed(1)}" y2="${dp.y.toFixed(1)}" class="topo-spoke" stroke="${col}" filter="url(#topo-glow)"/>`,
      );
      const lx = dp.x + Math.cos(a) * 10;
      const ly = dp.y + Math.sin(a) * 10 + 3;
      const anchor = Math.cos(a) < -0.25 ? "end" : Math.cos(a) > 0.25 ? "start" : "middle";
      const node = `<g class="topo-dev"><circle cx="${dp.x.toFixed(1)}" cy="${dp.y.toFixed(1)}" r="6" fill="${col}" filter="url(#topo-glow)"><title>${esc(tip)}</title></circle><text x="${lx.toFixed(1)}" y="${ly.toFixed(1)}" text-anchor="${anchor}" class="topo-dev-label">${esc(name)}</text></g>`;
      devNodes.push(hostID ? `<a href="${hostsLinkURL(hostID)}" target="_top">${node}</a>` : node);
    });

    // hub node on top
    const peerCount = r.status?.peer_count ?? peers.length;
    const fill = !r.status ? NEON.slate : r.status.stale ? NEON.red : NEON.blue;
    const egress = r.advertised_routes ?? [];
    const egressTip = egress.length
      ? `\n出口: ${egress.map((rt) => (rt === "0.0.0.0/0" ? "0.0.0.0/0 (全隧道)" : rt)).join(", ")}`
      : "";
    const htip = `${r.label || r.slug}\n${r.endpoint || "no endpoint"}\nwg ${r.wg_ip || "—"}\nmesh ${r.mesh_cidr || "—"}\n${peerCount} peers${r.status?.stale ? " (stale)" : ""}${egressTip}`;
    // P3 annotations: owned /24 under the label; 🌍 badge when the hub
    // declares egress routes (hover for the route list).
    const cidrLine = r.mesh_cidr
      ? `<text x="${hp.x.toFixed(1)}" y="${(hp.y + 54).toFixed(1)}" text-anchor="middle" class="topo-hub-cidr">${esc(r.mesh_cidr)}</text>`
      : "";
    const egressBadge = egress.length
      ? `<text x="${(hp.x + 18).toFixed(1)}" y="${(hp.y - 16).toFixed(1)}" class="topo-egress">🌍<title>${esc("出口路由:\n" + egress.join("\n"))}</title></text>`
      : "";
    hubNodes.push(
      `<g class="topo-hub"><circle cx="${hp.x.toFixed(1)}" cy="${hp.y.toFixed(1)}" r="22" fill="${fill}" filter="url(#topo-glow)"><title>${esc(htip)}</title></circle><text x="${hp.x.toFixed(1)}" y="${(hp.y + 5).toFixed(1)}" text-anchor="middle" class="topo-hub-count">${peerCount}</text><text x="${hp.x.toFixed(1)}" y="${(hp.y + 40).toFixed(1)}" text-anchor="middle" class="topo-hub-label">${esc(r.label || r.slug)}</text>${cidrLine}${egressBadge}</g>`,
    );
  });

  // Dark-NOC chrome (shared with the hosts panel): deep blue→black panel +
  // neon glow filter. Layer order: hub links, device spokes, device nodes,
  // hub nodes on top.
  const svg = `<svg viewBox="0 0 ${W} ${H}" class="topo-svg" preserveAspectRatio="xMidYMid meet" role="img" aria-label="WG mesh topology">${nocGlowDefs("topo-glow")}${hubLinks.join("")}${spokes.join("")}${devNodes.join("")}${hubNodes.join("")}</svg>`;
  return nocPanel({ svg });
}

function applyStatusView(): void {
  if (hubStatusGrid) hubStatusGrid.hidden = topoView;
  if (hubTopologyWrap) hubTopologyWrap.hidden = !topoView;
  if (hubViewToggle) hubViewToggle.textContent = topoView ? "▤ 列表" : "🌐 拓扑";
}

async function refreshStatus(): Promise<void> {
  if (!hubStatusGrid) return;
  try {
    const { data } = await listWGHubStatus();
    const rows: WGHubStatusRow[] = data.hubs ?? [];
    hubStatusGrid.innerHTML = "";
    if (hubTopology) hubTopology.innerHTML = "";
    if (rows.length === 0) {
      if (hubStatusEmpty) hubStatusEmpty.hidden = false;
      if (hubTopologyWrap) hubTopologyWrap.hidden = true;
      if (hubStatusSummary) hubStatusSummary.textContent = "no hubs";
      return;
    }
    if (hubStatusEmpty) hubStatusEmpty.hidden = true;
    rows.forEach((r: WGHubStatusRow) => hubStatusGrid.appendChild(renderHubCard(r)));
    if (hubTopology) hubTopology.innerHTML = renderTopology(rows);
    applyStatusView();
    const reported = rows.filter((r: WGHubStatusRow) => r.status !== null).length;
    const stale = rows.filter((r: WGHubStatusRow) => r.status?.stale === true).length;
    const summary =
      stale > 0
        ? `${reported}/${rows.length} reporting · ${stale} stale`
        : `${reported}/${rows.length} reporting`;
    if (hubStatusSummary) hubStatusSummary.textContent = summary;
  } catch (err) {
    if (hubStatusSummary) hubStatusSummary.textContent = `error: ${(err as Error).message}`;
  }
}

hubStatusRefreshBtn?.addEventListener("click", () => void refreshStatus());
hubViewToggle?.addEventListener("click", () => {
  topoView = !topoView;
  applyStatusView();
});

// ---- shared state ----

let hubsCache: WGHub[] = [];

// ---- Hubs tab ----

const hubsTable = byId<HTMLTableElement>("hubsTable");
const hubsTbody = byId<HTMLTableSectionElement>("hubsTbody");
const hubsEmpty = byId<HTMLElement>("hubsEmpty");
const hubsSummary = byId<HTMLElement>("hubsSummary");
const hubsRefreshBtn = byId<HTMLButtonElement>("hubsRefreshBtn");

function hubStatus(h: WGHub): string {
  if (h.bound_device_id) return `🟢 已绑定 dev#${h.bound_device_id}`;
  if (h.endpoint) return "⚠ 等待 hub install";
  return "⚪ 未配置";
}

function renderHubRow(h: WGHub): HTMLTableRowElement {
  const tr = document.createElement("tr");
  const routes = h.advertised_routes ?? [];
  const routesCell = routes.length
    ? routes.map((r) => `<code>${r}</code>`).join(", ")
    : "—";
  const cells = [
    `<code>${h.slug}</code>`,
    h.label || "—",
    `<code>${h.endpoint || "—"}</code>`,
    `<code>${h.mesh_cidr}</code>`,
    routesCell,
    hubStatus(h),
    h.bound_device_id ? `<a href="#" data-jump-device="${h.bound_device_id}">dev#${h.bound_device_id}</a>` : "—",
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  const cellActions = document.createElement("td");
  cellActions.style.whiteSpace = "nowrap";
  // P2 egress: edit the hub's advertised routes (comma-separated CIDRs).
  const egressBtn = document.createElement("button");
  egressBtn.type = "button";
  egressBtn.className = "btn-inline btn-secondary";
  egressBtn.textContent = "出口";
  egressBtn.title = "声明这个 hub 能网关到的外部 CIDR（如机房内网）；0.0.0.0/0 = 全隧道。设备侧在 Devices 标签 per-device 选用。";
  egressBtn.style.marginRight = "4px";
  egressBtn.addEventListener("click", async () => {
    const input = window.prompt(
      "出口路由（逗号分隔 CIDR；0.0.0.0/0 = 全隧道出口；留空 = 清除）：",
      routes.join(", "),
    );
    if (input === null) return;
    const next = input
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    try {
      // Backend overwrites label/endpoint unconditionally — echo them.
      const { data } = await updateWGHub(h.id, {
        label: h.label,
        endpoint: h.endpoint,
        advertised_routes: next,
      });
      if (data.error) {
        window.alert(data.error);
        return;
      }
      await refreshHubs();
    } catch (err) {
      window.alert((err as Error).message);
    }
  });
  cellActions.appendChild(egressBtn);
  if (h.bound_device_id) {
    const resetBtn = document.createElement("button");
    resetBtn.type = "button";
    resetBtn.className = "btn-inline btn-secondary";
    resetBtn.textContent = "解绑";
    resetBtn.title = "清掉 pubkey + bound_device_id，下个 hub token 可以重新接管";
    resetBtn.style.marginRight = "4px";
    resetBtn.addEventListener("click", async () => {
      if (!window.confirm(`解绑 hub "${h.slug}"？现有设备的 peer list 仍指向已绑定设备的旧 pubkey。`)) return;
      try {
        await resetWGHubBind(h.id);
        await refreshHubs();
      } catch (err) {
        window.alert((err as Error).message);
      }
    });
    cellActions.appendChild(resetBtn);
  }
  const delBtn = document.createElement("button");
  delBtn.type = "button";
  delBtn.className = "btn-inline btn-secondary";
  delBtn.textContent = "🗑";
  delBtn.addEventListener("click", async () => {
    if (!window.confirm(`删除 hub "${h.slug}"？必须先清掉它下面所有活跃设备。`)) return;
    try {
      const { data } = await deleteWGHub(h.id);
      if (data.error) {
        window.alert(data.error);
        return;
      }
      await refreshHubs();
    } catch (err) {
      window.alert((err as Error).message);
    }
  });
  cellActions.appendChild(delBtn);
  tr.appendChild(cellActions);
  return tr;
}

async function refreshHubs(): Promise<void> {
  try {
    const { data } = await listWGHubs();
    const items = data.hubs ?? [];
    hubsCache = items;
    hubsTbody.innerHTML = "";
    items.forEach((h) => hubsTbody.appendChild(renderHubRow(h)));
    hubsSummary.textContent = `${items.length} 个`;
    hubsTable.hidden = items.length === 0;
    hubsEmpty.hidden = items.length !== 0;
    // Keep token-create hub dropdown in sync.
    syncTokenCreateHubDropdown();
    // Cross-hub link publishing: populate the picker + refresh the link list.
    populateLinkHubSelects(items);
    void refreshHubLinks();
  } catch (err) {
    hubsEmpty.hidden = false;
    hubsEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

// ---- cross-hub link publishing ----

const linkHubA = byId<HTMLSelectElement>("linkHubA");
const linkHubB = byId<HTMLSelectElement>("linkHubB");
const publishLinkBtn = byId<HTMLButtonElement>("publishLinkBtn");
const linkPublishMsg = byId<HTMLElement>("linkPublishMsg");
const hubLinksTable = byId<HTMLTableElement>("hubLinksTable");
const hubLinksTbody = byId<HTMLTableSectionElement>("hubLinksTbody");
const hubLinksEmpty = byId<HTMLElement>("hubLinksEmpty");
const hubLinksSummary = byId<HTMLElement>("hubLinksSummary");

function populateLinkHubSelects(hubs: WGHub[]): void {
  if (!linkHubA || !linkHubB) return;
  const opts = hubs
    .map((h) => `<option value="${h.id}">${h.slug} (${h.mesh_cidr})</option>`)
    .join("");
  const aVal = linkHubA.value, bVal = linkHubB.value;
  linkHubA.innerHTML = opts;
  linkHubB.innerHTML = opts;
  if (aVal) linkHubA.value = aVal;
  if (bVal) linkHubB.value = bVal;
}

async function refreshHubLinks(): Promise<void> {
  if (!hubLinksTbody || !hubLinksTable || !hubLinksEmpty) return;
  try {
    const { data } = await listWGHubLinks();
    const links = data.links ?? [];
    hubLinksTbody.innerHTML = "";
    links.forEach((l) => hubLinksTbody.appendChild(renderHubLinkRow(l)));
    if (hubLinksSummary) hubLinksSummary.textContent = `${links.length} 条`;
    hubLinksTable.hidden = links.length === 0;
    hubLinksEmpty.hidden = links.length !== 0;
  } catch (err) {
    hubLinksEmpty.hidden = false;
    hubLinksEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

function renderHubLinkRow(l: WGHubLink): HTMLTableRowElement {
  const tr = document.createElement("tr");
  const a = l.hub_a_slug ?? String(l.hub_a_id);
  const b = l.hub_b_slug ?? String(l.hub_b_id);
  const when = l.created_at ? new Date(l.created_at).toLocaleString() : "—";
  for (const html of [a, b, when]) {
    const td = document.createElement("td");
    td.textContent = html;
    tr.appendChild(td);
  }
  const cellAction = document.createElement("td");
  const del = document.createElement("button");
  del.className = "btn-inline btn-secondary";
  del.textContent = "取消发布";
  del.addEventListener("click", async () => {
    if (!confirm(`取消发布 ${a} ↔ ${b} 的跨 hub 路由?`)) return;
    const { data } = await deleteWGHubLink(l.id);
    if (data && data.ok === false) {
      alert(`失败：${data.error ?? "unknown"}`);
      return;
    }
    void refreshHubLinks();
  });
  cellAction.appendChild(del);
  tr.appendChild(cellAction);
  return tr;
}

publishLinkBtn?.addEventListener("click", async () => {
  if (!linkHubA || !linkHubB) return;
  const a = Number(linkHubA.value), b = Number(linkHubB.value);
  if (linkPublishMsg) linkPublishMsg.textContent = "";
  if (!a || !b || a === b) {
    if (linkPublishMsg) linkPublishMsg.textContent = "请选择两个不同的 hub";
    return;
  }
  const { data } = await createWGHubLink(a, b);
  if (!data.link) {
    if (linkPublishMsg) linkPublishMsg.textContent = `失败：${data.error ?? "unknown"}`;
    return;
  }
  if (linkPublishMsg) linkPublishMsg.textContent = "已发布";
  void refreshHubLinks();
});

hubsRefreshBtn?.addEventListener("click", () => void refreshHubs());

// ---- Tokens tab ----

const tokensTable = byId<HTMLTableElement>("tokensTable");
const tokensTbody = byId<HTMLTableSectionElement>("tokensTbody");
const tokensEmpty = byId<HTMLElement>("tokensEmpty");
const tokensSummary = byId<HTMLElement>("tokensSummary");
const tokensRefreshBtn = byId<HTMLButtonElement>("tokensRefreshBtn");
const newTokenBtn = byId<HTMLButtonElement>("newTokenBtn");

const createTokenModal = byId<HTMLElement>("createTokenModal");
const createTokenCloseBtn = byId<HTMLButtonElement>("createTokenCloseBtn");
const createTokenCancelBtn = byId<HTMLButtonElement>("createTokenCancelBtn");
const createTokenSubmitBtn = byId<HTMLButtonElement>("createTokenSubmitBtn");
const createTokenLabel = byId<HTMLInputElement>("createTokenLabel");
const createTokenTTL = byId<HTMLInputElement>("createTokenTTL");
const createTokenError = byId<HTMLElement>("createTokenError");

const roleDeviceFields = byId<HTMLElement>("roleDeviceFields");
const roleHubFields = byId<HTMLElement>("roleHubFields");
const createTokenHubId = byId<HTMLSelectElement>("createTokenHubId");
const createTokenHubSlug = byId<HTMLInputElement>("createTokenHubSlug");
const createTokenHubLabel = byId<HTMLInputElement>("createTokenHubLabel");
const createTokenPublicEndpoint = byId<HTMLInputElement>("createTokenPublicEndpoint");
const createTokenMeshCidr = byId<HTMLInputElement>("createTokenMeshCidr");

const plaintextModal = byId<HTMLElement>("plaintextModal");
const plaintextValue = byId<HTMLInputElement>("plaintextValue");
const plaintextCopyBtn = byId<HTMLButtonElement>("plaintextCopyBtn");
const plaintextInstallCmd = byId<HTMLTextAreaElement>("plaintextInstallCmd");
const plaintextInstallCopyBtn = byId<HTMLButtonElement>("plaintextInstallCopyBtn");
const plaintextDoneBtn = byId<HTMLButtonElement>("plaintextDoneBtn");
// Phase 11 C3 — optional Tailscale PreAuthKey block; surfaced only
// when the server responds with `tailscale_authkey` (i.e. embedded
// headscale is enabled at the dock level).
const tsBlock = document.getElementById("tsBlock") as HTMLElement | null;
const tsKeyValue = document.getElementById("tsKeyValue") as HTMLInputElement | null;
const tsKeyCopyBtn = document.getElementById("tsKeyCopyBtn") as HTMLButtonElement | null;
const tsCmdValue = document.getElementById("tsCmdValue") as HTMLTextAreaElement | null;
const tsCmdCopyBtn = document.getElementById("tsCmdCopyBtn") as HTMLButtonElement | null;
if (tsKeyCopyBtn && tsKeyValue) {
  tsKeyCopyBtn.addEventListener("click", async () => {
    await navigator.clipboard.writeText(tsKeyValue.value);
    tsKeyCopyBtn.textContent = "✓";
    setTimeout(() => (tsKeyCopyBtn.textContent = "📋"), 1500);
  });
}
if (tsCmdCopyBtn && tsCmdValue) {
  tsCmdCopyBtn.addEventListener("click", async () => {
    await navigator.clipboard.writeText(tsCmdValue.value);
    tsCmdCopyBtn.textContent = "✓ 已复制";
    setTimeout(() => (tsCmdCopyBtn.textContent = "📋 复制 tailscale up 命令"), 1500);
  });
}

function selectedTokenRole(): "hub" | "device" {
  const checked = document.querySelector<HTMLInputElement>('input[name="tokenRole"]:checked');
  return (checked?.value as "hub" | "device") || "device";
}

function syncTokenCreateHubDropdown(): void {
  createTokenHubId.innerHTML = "";
  if (hubsCache.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = "—— 先创建一个 role=hub token ——";
    opt.disabled = true;
    createTokenHubId.appendChild(opt);
    return;
  }
  for (const h of hubsCache) {
    const opt = document.createElement("option");
    opt.value = String(h.id);
    opt.textContent = `${h.slug}${h.label ? ` (${h.label})` : ""} · ${h.mesh_cidr}${h.bound_device_id ? "" : " ⚠未上线"}`;
    if (!h.bound_device_id) opt.disabled = true;
    createTokenHubId.appendChild(opt);
  }
}

function onRoleChange(): void {
  const role = selectedTokenRole();
  roleDeviceFields.hidden = role !== "device";
  roleHubFields.hidden = role !== "hub";
}
document.querySelectorAll<HTMLInputElement>('input[name="tokenRole"]').forEach((r) => {
  r.addEventListener("change", onRoleChange);
});

function tokenStatusText(tok: WGToken): string {
  if (tok.revoked_at) return `🔴 已撤销 ${fmtRelative(tok.revoked_at)}`;
  if (tok.consumed_at) return `✓ 已使用 ${fmtRelative(tok.consumed_at)}`;
  if (tok.expires_at && new Date(tok.expires_at).getTime() < Date.now()) {
    return `⏰ 已过期 ${fmtRelative(tok.expires_at)}`;
  }
  return "🟢 待使用";
}

function hubLabelByID(id?: number): string {
  if (!id) return "—";
  const h = hubsCache.find((x) => x.id === id);
  return h ? h.slug : `#${id}`;
}

// hubCellForToken — for hub tokens, show slug + the operator-stamped
// public endpoint + mesh CIDR (these are what the operator entered at
// token-mint time; once bound, they're also the row in 🌐 Hubs tab).
// For device tokens, just the slug.
function hubCellForToken(tok: WGToken): string {
  const slug = hubLabelByID(tok.hub_id);
  const h = tok.hub_id ? hubsCache.find((x) => x.id === tok.hub_id) : undefined;
  const tip = [h?.slug, h ? `#${h.id}` : (tok.hub_id ? `#${tok.hub_id}` : ""), (h as { mesh_cidr?: string })?.mesh_cidr]
    .filter(Boolean).join(" · ");
  if (tok.role !== "hub") {
    return `<span title="${escAttr(tip)}">${slug}</span>`;
  }
  const ep = tok.public_endpoint ? `<br><code style="font-size:11px;">${tok.public_endpoint}</code>` : "";
  const cidr = tok.mesh_cidr_pref ? ` <span class="meta-subtitle">${tok.mesh_cidr_pref}</span>` : "";
  return `<span title="${escAttr(tip)}">${slug}</span>${ep}${cidr}`;
}

// escAttr escapes a string for safe use inside an HTML attribute value.
function escAttr(s: string): string {
  return String(s ?? "").replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function renderTokenRow(tok: WGToken): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (tok.revoked_at) tr.style.opacity = "0.5";
  const pfx = tok.token_prefix || "";
  const pfxTail = pfx.length > 8 ? `…${pfx.slice(-8)}` : pfx;
  const cells = [
    tok.label,
    `<span class="wg-role">${tok.role === "hub" ? "🌐 hub" : "💻 device"}</span>`,
    hubCellForToken(tok),
    `<code style="font-size:11px;" title="${escAttr(pfx)}">${pfxTail}</code>` +
      `<button class="wg-copy" type="button" data-copy="${escAttr(pfx)}" title="复制前缀">⧉</button>`,
    tok.expires_at ? new Date(tok.expires_at).toISOString().slice(0, 10) : "永久",
    tokenStatusText(tok),
    fmtRelative(tok.created_at),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  tr.querySelectorAll<HTMLButtonElement>("button.wg-copy").forEach((b) => {
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      void navigator.clipboard?.writeText(b.dataset.copy || "");
      const o = b.textContent; b.textContent = "✓"; setTimeout(() => (b.textContent = o), 1200);
    });
  });
  const cellActions = document.createElement("td");
  if (!tok.revoked_at) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn-inline btn-secondary";
    btn.textContent = "撤销";
    btn.addEventListener("click", async () => {
      if (!window.confirm(`撤销 token "${tok.label}"？已绑定的设备不受影响。`)) return;
      try {
        await revokeWGToken(tok.id);
        await refreshTokens();
      } catch (err) {
        window.alert(`撤销失败：${(err as Error).message}`);
      }
    });
    cellActions.appendChild(btn);
  }
  tr.appendChild(cellActions);
  return tr;
}

async function refreshTokens(): Promise<void> {
  try {
    const { data } = await listWGTokens();
    const all = data.tokens ?? [];
    const hideRevoked = (document.getElementById("hideRevokedCheck") as HTMLInputElement | null)?.checked ?? true;
    const items = hideRevoked ? all.filter((t) => !t.revoked_at) : all;
    tokensTbody.innerHTML = "";
    items.forEach((tok) => tokensTbody.appendChild(renderTokenRow(tok)));
    const revokedN = all.length - all.filter((t) => !t.revoked_at).length;
    tokensSummary.textContent = hideRevoked && revokedN > 0 ? `${items.length} 个（隐藏 ${revokedN} 已撤销）` : `${all.length} 个`;
    tokensTable.hidden = items.length === 0;
    tokensEmpty.hidden = items.length !== 0;
  } catch (err) {
    tokensEmpty.hidden = false;
    tokensEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

function openCreateTokenModal(): void {
  createTokenLabel.value = "";
  createTokenTTL.value = "90";
  createTokenError.textContent = "";
  createTokenHubSlug.value = "";
  createTokenHubLabel.value = "";
  createTokenPublicEndpoint.value = "";
  createTokenMeshCidr.value = "";
  syncTokenCreateHubDropdown();
  onRoleChange();
  createTokenModal.hidden = false;
  createTokenLabel.focus();
}
function closeCreateTokenModal(): void {
  createTokenModal.hidden = true;
}

newTokenBtn?.addEventListener("click", openCreateTokenModal);
createTokenCloseBtn?.addEventListener("click", closeCreateTokenModal);
createTokenCancelBtn?.addEventListener("click", closeCreateTokenModal);
tokensRefreshBtn?.addEventListener("click", () => void refreshTokens());
createTokenModal?.addEventListener("click", (e) => {
  if (e.target === createTokenModal) closeCreateTokenModal();
});

createTokenSubmitBtn?.addEventListener("click", async () => {
  createTokenError.textContent = "";
  const label = createTokenLabel.value.trim();
  if (!label) {
    createTokenError.textContent = "标签必填";
    return;
  }
  const ttl = Number(createTokenTTL.value) || 0;
  const role = selectedTokenRole();
  try {
    let resp;
    if (role === "hub") {
      const slug = createTokenHubSlug.value.trim();
      const hubLabel = createTokenHubLabel.value.trim();
      const endpoint = createTokenPublicEndpoint.value.trim();
      const cidr = createTokenMeshCidr.value.trim();  // empty → server picks from 100.64/10
      if (!slug) {
        createTokenError.textContent = "hub slug 必填";
        return;
      }
      if (!endpoint) {
        createTokenError.textContent = "public endpoint 必填 (e.g. west.example.com:51820)";
        return;
      }
      resp = await createWGTokenForHub(label, ttl, slug, hubLabel, endpoint, cidr);
    } else {
      const hubID = Number(createTokenHubId.value);
      if (!hubID) {
        createTokenError.textContent = "请选一个 hub";
        return;
      }
      resp = await createWGTokenForDevice(label, ttl, hubID);
    }
    const { data } = resp;
    if (data.error || !data.token?.plaintext) {
      createTokenError.textContent = data.error || "服务器没返回 plaintext";
      return;
    }
    closeCreateTokenModal();
    const plaintext = data.token.plaintext;
    plaintextValue.value = plaintext;
    const origin = window.location.origin;
    plaintextInstallCmd.value = `curl -sSL ${origin}/v1/install | sudo bash -s -- --token=${plaintext}`;
    // Phase 11 C3: if embedded headscale was enabled at server boot,
    // a matching tskey-... was minted alongside the wg-mac token.
    // Show both so the operator can hand a unified bundle out.
    const tsKey = (data as { tailscale_authkey?: string }).tailscale_authkey;
    if (tsKey && tsKeyValue && tsCmdValue && tsBlock) {
      tsKeyValue.value = tsKey;
      tsCmdValue.value = `tailscale up --login-server=${origin} --authkey=${tsKey}`;
      tsBlock.hidden = false;
    } else if (tsBlock) {
      tsBlock.hidden = true;
    }
    plaintextModal.hidden = false;
    await refreshTokens();
    await refreshHubs();
  } catch (err) {
    createTokenError.textContent = `创建失败：${(err as Error).message}`;
  }
});

plaintextCopyBtn?.addEventListener("click", async () => {
  await navigator.clipboard.writeText(plaintextValue.value);
  plaintextCopyBtn.textContent = "✓";
  setTimeout(() => (plaintextCopyBtn.textContent = "📋"), 1500);
});
plaintextInstallCopyBtn?.addEventListener("click", async () => {
  await navigator.clipboard.writeText(plaintextInstallCmd.value);
  plaintextInstallCopyBtn.textContent = "✓ 已复制";
  setTimeout(() => (plaintextInstallCopyBtn.textContent = "📋 复制命令"), 1500);
});
plaintextDoneBtn?.addEventListener("click", () => {
  plaintextModal.hidden = true;
});

// ---- Devices tab ----

const devicesTable = byId<HTMLTableElement>("devicesTable");
const devicesTbody = byId<HTMLTableSectionElement>("devicesTbody");
const devicesEmpty = byId<HTMLElement>("devicesEmpty");
const devicesSummary = byId<HTMLElement>("devicesSummary");
const devicesRefreshBtn = byId<HTMLButtonElement>("devicesRefreshBtn");
const includeRemovedCheck = byId<HTMLInputElement>("includeRemovedCheck");

function deviceStatus(dev: WGDevice): string {
  if (dev.removed_at) return `🔴 已下线 ${fmtRelative(dev.removed_at)}`;
  let base: string;
  if (!dev.last_seen_at) {
    base = "⚪ 从未上报";
  } else {
    const ageSec = Math.floor((Date.now() - new Date(dev.last_seen_at).getTime()) / 1000);
    if (ageSec < 600) base = "🟢 在线";
    else if (ageSec < 86400) base = "🟡 待确认";
    else base = "🔴 离线";
  }
  // Append a token-expiry warning if expiry is within 7 days.
  if (dev.token_expires_at) {
    const msLeft = new Date(dev.token_expires_at).getTime() - Date.now();
    if (msLeft <= 0) return `${base} ⚠ token 已过期`;
    if (msLeft < 7 * 86400_000) {
      const days = Math.max(1, Math.floor(msLeft / 86400_000));
      return `${base} ⏰ token ${days}d 后过期`;
    }
  }
  return base;
}

// renderDeviceEgressCell — P2 egress opt-in dropdown. Options = hubs that
// declared advertised_routes. Own-hub options carry full-tunnel rights;
// cross-hub options route subnets only (0.0.0.0/0 stripped server-side).
function renderDeviceEgressCell(dev: WGDevice): HTMLTableCellElement {
  const td = document.createElement("td");
  if (dev.removed_at) {
    td.textContent = "—";
    return td;
  }
  const candidates = hubsCache.filter((h) => (h.advertised_routes ?? []).length > 0);
  if (candidates.length === 0 && !dev.egress_hub_id) {
    td.textContent = "—";
    td.title = "没有 hub 声明出口路由（在 Hubs 标签点「出口」配置）";
    return td;
  }
  const sel = document.createElement("select");
  sel.className = "btn-inline";
  const offOpt = document.createElement("option");
  offOpt.value = "";
  offOpt.textContent = "关闭";
  sel.appendChild(offOpt);
  for (const h of candidates) {
    const opt = document.createElement("option");
    opt.value = String(h.id);
    const kind = h.id === dev.hub_id ? "本hub" : "跨hub仅子网";
    opt.textContent = `${h.slug}（${kind}）`;
    opt.title = (h.advertised_routes ?? []).join(", ");
    sel.appendChild(opt);
  }
  // Current selection may point at a hub that since cleared its routes —
  // keep it visible so the operator can see + turn it off.
  if (dev.egress_hub_id && !candidates.some((h) => h.id === dev.egress_hub_id)) {
    const opt = document.createElement("option");
    opt.value = String(dev.egress_hub_id);
    opt.textContent = `#${dev.egress_hub_id}（已无出口路由）`;
    sel.appendChild(opt);
  }
  sel.value = dev.egress_hub_id ? String(dev.egress_hub_id) : "";
  const prevValue = sel.value;
  sel.addEventListener("change", async () => {
    const next = sel.value ? Number(sel.value) : null;
    const label = next ? sel.options[sel.selectedIndex].textContent : "关闭（不走出口）";
    if (!window.confirm(`把设备 "${dev.hostname || dev.id}" 的出口路由改为「${label}」？此操作会立即改变该设备的流量走向。`)) {
      sel.value = prevValue; // revert the dropdown, no API call
      return;
    }
    try {
      const { data } = await updateWGDeviceEgress(dev.id, next);
      if (data.error) {
        window.alert(data.error);
        await refreshDevices();
        return;
      }
    } catch (err) {
      window.alert(`设置出口失败：${(err as Error).message}`);
      await refreshDevices();
    }
  });
  td.appendChild(sel);
  return td;
}

function renderDeviceRow(dev: WGDevice): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (dev.removed_at) tr.style.opacity = "0.5";
  const hostnameCell = dev.host_id
    ? `<a href="${hostsLinkURL(dev.host_id)}" title="在 Hosts 模块查看该主机">${esc(dev.hostname || dev.host_id)}</a>`
    : esc(dev.hostname || "—");
  const cells: string[] = [
    `<code style="font-size:11px;">${dev.device_id}</code>`,
    hostnameCell,
    dev.hub_slug || `#${dev.hub_id}`,
    dev.site_slug || `#${dev.site_id}`,
    `<code>${dev.device_ip}</code>`,
    dev.wg_endpoint ? `<code>${dev.wg_endpoint}</code>` : "—",
    `${dev.os}/${dev.arch} <span class="meta-subtitle">${dev.agent_ver || ""}</span>`,
    fmtRelative(dev.last_seen_at),
    deviceStatus(dev),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  tr.appendChild(renderDeviceEgressCell(dev));
  const cellActions = document.createElement("td");
  if (!dev.removed_at) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn-inline btn-secondary";
    btn.textContent = "强制下线";
    btn.addEventListener("click", async () => {
      if (!window.confirm(`强制下线设备 "${dev.hostname || dev.device_id}"？`)) return;
      try {
        await removeWGDevice(dev.id);
        await refreshDevices();
        await refreshHubs();
      } catch (err) {
        window.alert(`下线失败：${(err as Error).message}`);
      }
    });
    cellActions.appendChild(btn);
  }
  tr.appendChild(cellActions);
  return tr;
}

async function refreshDevices(): Promise<void> {
  try {
    const { data } = await listWGDevices(includeRemovedCheck.checked);
    const items = data.devices ?? [];
    devicesTbody.innerHTML = "";
    items.forEach((d) => devicesTbody.appendChild(renderDeviceRow(d)));
    devicesSummary.textContent = `${items.length} 台`;
    devicesTable.hidden = items.length === 0;
    devicesEmpty.hidden = items.length !== 0;
  } catch (err) {
    devicesEmpty.hidden = false;
    devicesEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

devicesRefreshBtn?.addEventListener("click", () => void refreshDevices());
includeRemovedCheck?.addEventListener("change", () => void refreshDevices());
document.getElementById("hideRevokedCheck")?.addEventListener("change", () => void refreshTokens());

// ---- Bundles tab ----

const bundlesTable = byId<HTMLTableElement>("bundlesTable");
const bundlesTbody = byId<HTMLTableSectionElement>("bundlesTbody");
const bundlesEmpty = byId<HTMLElement>("bundlesEmpty");
const bundlesSummary = byId<HTMLElement>("bundlesSummary");
const bundlesRefreshBtn = byId<HTMLButtonElement>("bundlesRefreshBtn");
const uploadBundleBtn = byId<HTMLButtonElement>("uploadBundleBtn");

const uploadBundleModal = byId<HTMLElement>("uploadBundleModal");
const uploadBundleCloseBtn = byId<HTMLButtonElement>("uploadBundleCloseBtn");
const uploadBundleCancelBtn = byId<HTMLButtonElement>("uploadBundleCancelBtn");
const uploadBundleSubmitBtn = byId<HTMLButtonElement>("uploadBundleSubmitBtn");
const uploadBundleFile = byId<HTMLInputElement>("uploadBundleFile");
const uploadBundleVersion = byId<HTMLInputElement>("uploadBundleVersion");
const uploadBundleNotes = byId<HTMLInputElement>("uploadBundleNotes");
const uploadBundleSetLatest = byId<HTMLInputElement>("uploadBundleSetLatest");
const uploadBundleError = byId<HTMLElement>("uploadBundleError");

function renderBundleRow(b: WGBundle): HTMLTableRowElement {
  const tr = document.createElement("tr");
  const cells: string[] = [
    `<code>${b.version}</code>`,
    `<code style="font-size:11px;">${b.blob_sha256.slice(0, 12)}…</code>`,
    fmtBytes(b.size_bytes),
    b.is_latest ? "✓ latest" : "",
    b.notes ? `<span class="meta-subtitle">${b.notes}</span>` : "",
    fmtRelative(b.added_at),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  const cellActions = document.createElement("td");
  cellActions.style.whiteSpace = "nowrap";
  const dl = document.createElement("a");
  dl.href = `/v1/bundle/${encodeURIComponent(b.version)}`;
  dl.className = "btn-inline btn-secondary";
  dl.textContent = "⬇";
  dl.style.marginRight = "4px";
  cellActions.appendChild(dl);
  if (!b.is_latest) {
    const setBtn = document.createElement("button");
    setBtn.type = "button";
    setBtn.className = "btn-inline btn-secondary";
    setBtn.textContent = "设为 latest";
    setBtn.style.marginRight = "4px";
    setBtn.addEventListener("click", async () => {
      try {
        await setWGBundleLatest(b.id);
        await refreshBundles();
      } catch (err) {
        window.alert(`标记失败：${(err as Error).message}`);
      }
    });
    cellActions.appendChild(setBtn);
  }
  const delBtn = document.createElement("button");
  delBtn.type = "button";
  delBtn.className = "btn-inline btn-secondary";
  delBtn.textContent = "🗑";
  delBtn.addEventListener("click", async () => {
    if (!window.confirm(`删除 bundle "${b.version}"？已在用的设备不受影响。`)) return;
    try {
      await deleteWGBundle(b.id);
      await refreshBundles();
    } catch (err) {
      window.alert(`删除失败：${(err as Error).message}`);
    }
  });
  cellActions.appendChild(delBtn);
  tr.appendChild(cellActions);
  return tr;
}

async function refreshBundles(): Promise<void> {
  try {
    const { data } = await listWGBundles();
    const items = data.bundles ?? [];
    bundlesTbody.innerHTML = "";
    items.forEach((b) => bundlesTbody.appendChild(renderBundleRow(b)));
    const latest = items.find((b) => b.is_latest);
    bundlesSummary.textContent = `${items.length} 个 · latest=${latest?.version ?? "—"}`;
    bundlesTable.hidden = items.length === 0;
    bundlesEmpty.hidden = items.length !== 0;
  } catch (err) {
    bundlesEmpty.hidden = false;
    bundlesEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

function openUploadBundleModal(): void {
  uploadBundleFile.value = "";
  uploadBundleVersion.value = "";
  uploadBundleNotes.value = "";
  uploadBundleSetLatest.checked = true;
  uploadBundleError.textContent = "";
  uploadBundleModal.hidden = false;
}
function closeUploadBundleModal(): void {
  uploadBundleModal.hidden = true;
}

uploadBundleBtn?.addEventListener("click", openUploadBundleModal);
uploadBundleCloseBtn?.addEventListener("click", closeUploadBundleModal);
uploadBundleCancelBtn?.addEventListener("click", closeUploadBundleModal);
bundlesRefreshBtn?.addEventListener("click", () => void refreshBundles());
uploadBundleModal?.addEventListener("click", (e) => {
  if (e.target === uploadBundleModal) closeUploadBundleModal();
});

uploadBundleSubmitBtn?.addEventListener("click", async () => {
  uploadBundleError.textContent = "";
  const file = uploadBundleFile.files?.[0];
  if (!file) {
    uploadBundleError.textContent = "请选择 .tar.gz 文件";
    return;
  }
  const form = new FormData();
  form.append("file", file);
  const version = uploadBundleVersion.value.trim();
  if (version) form.append("version", version);
  const notes = uploadBundleNotes.value.trim();
  if (notes) form.append("notes", notes);
  if (uploadBundleSetLatest.checked) form.append("set_latest", "1");
  try {
    uploadBundleSubmitBtn.disabled = true;
    uploadBundleSubmitBtn.textContent = "上传中...";
    const { ok, data } = await uploadWGBundle(form);
    if (!ok || data.error) {
      uploadBundleError.textContent = data.error || "上传失败";
      return;
    }
    closeUploadBundleModal();
    await refreshBundles();
  } catch (err) {
    uploadBundleError.textContent = `上传失败：${(err as Error).message}`;
  } finally {
    uploadBundleSubmitBtn.disabled = false;
    uploadBundleSubmitBtn.textContent = "上传";
  }
});

// ---- initial load ----

void refreshStatus();
// Devices render an egress dropdown built from hubsCache — load hubs first.
void refreshHubs().then(() => refreshDevices());
void refreshTokens();
void refreshBundles();
// devices fetch primes the pubkey→device map for the Status tab's
// peer rows. Best-effort: status will show raw pubkey until this
// resolves.
void primePubkeyMap();

// Tab default: ?tab=<key> wins; otherwise "hubs" (operators want the
// hubs list on landing, not the status view — same reason the 15s
// auto-refresh ticker below got removed: don't burn cycles + chrome
// real estate on a view the user didn't ask for).
const initialTab = ((): TabKey => {
  const p = new URLSearchParams(window.location.search).get("tab");
  return (tabs as readonly string[]).includes(p ?? "") ? (p as TabKey) : "hubs";
})();
switchTab(initialTab);

// Manual refresh only — operators wanted the "ui 不要乱刷新"
// behavior, matching the hosts.ts/agents.ts cleanup. Each pane has
// its own refresh button.

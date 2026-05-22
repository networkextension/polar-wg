// /wg-tokens.html — Phase 2 admin UI for wg-mac multi-hub control plane.
//
// Tabs:
//   - 🌐 Hubs     — list of all hubs (slug/label/endpoint/cidr/status/bound device)
//   - 🔑 Tokens   — list + role-aware create modal + plaintext-once + revoke
//   - 💻 Devices  — list with hub column + force-remove
//   - 📦 Bundles  — list + upload + mark-latest

import {
  createWGTokenForDevice,
  createWGTokenForHub,
  deleteWGBundle,
  deleteWGHub,
  listWGBundles,
  listWGDevices,
  listWGHubs,
  listWGTokens,
  removeWGDevice,
  resetWGHubBind,
  revokeWGToken,
  setWGBundleLatest,
  uploadWGBundle,
} from "./api/wg.js";
import { logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, hydrateSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { WGBundle, WGDevice, WGHub, WGToken } from "./types/wg.js";

initStoredTheme();
bindThemeSync();

byId<HTMLButtonElement>("logoutBtn")?.addEventListener("click", async () => {
  await logout();
  window.location.href = "/login.html";
});
void hydrateSiteBrand();
void hydrateSidebarFoot();

// ---- tabs ----

const tabs = ["hubs", "tokens", "devices", "bundles"] as const;
type TabKey = (typeof tabs)[number];

function switchTab(target: TabKey): void {
  tabs.forEach((t) => {
    const cap = t.charAt(0).toUpperCase() + t.slice(1);
    byId<HTMLButtonElement>(`tab${cap}`)?.classList.toggle("lp-tab-active", t === target);
    const pane = byId<HTMLElement>(`pane${cap}`);
    if (pane) pane.hidden = t !== target;
  });
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
  const cells = [
    `<code>${h.slug}</code>`,
    h.label || "—",
    `<code>${h.endpoint || "—"}</code>`,
    `<code>${h.mesh_cidr}</code>`,
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
  } catch (err) {
    hubsEmpty.hidden = false;
    hubsEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

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
  if (tok.role !== "hub") return slug;
  const ep = tok.public_endpoint ? `<br><code style="font-size:11px;">${tok.public_endpoint}</code>` : "";
  const cidr = tok.mesh_cidr_pref ? ` <span class="meta-subtitle">${tok.mesh_cidr_pref}</span>` : "";
  return `${slug}${ep}${cidr}`;
}

function renderTokenRow(tok: WGToken): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (tok.revoked_at) tr.style.opacity = "0.5";
  const cells = [
    tok.label,
    tok.role === "hub" ? `🌐 hub` : `💻 device`,
    hubCellForToken(tok),
    `<code style="font-size:11px;">${tok.token_prefix}…</code>`,
    tok.expires_at ? new Date(tok.expires_at).toISOString().slice(0, 10) : "永久",
    tokenStatusText(tok),
    fmtRelative(tok.created_at),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
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
    const items = data.tokens ?? [];
    tokensTbody.innerHTML = "";
    items.forEach((tok) => tokensTbody.appendChild(renderTokenRow(tok)));
    tokensSummary.textContent = `${items.length} 个`;
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

function renderDeviceRow(dev: WGDevice): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (dev.removed_at) tr.style.opacity = "0.5";
  const cells: string[] = [
    `<code style="font-size:11px;">${dev.device_id}</code>`,
    dev.hostname || "—",
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

// ---- initial load + polling ----

void refreshHubs();
void refreshTokens();
void refreshDevices();
void refreshBundles();

window.setInterval(() => {
  if (!byId<HTMLElement>("paneDevices").hidden) {
    void refreshDevices();
  }
}, 15_000);

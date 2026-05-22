// Typed fetch wrappers for /api/admin/wg-* (Phase 2 — multi-hub).

import { requestJson } from "./http.js";
import type {
  WGBundleListResponse,
  WGBundleResponse,
  WGDeviceListResponse,
  WGHub,
  WGHubListResponse,
  WGHubResponse,
  WGSiteListResponse,
  WGTokenCreateResponse,
  WGTokenListResponse,
} from "../types/wg.js";

// ---- hubs ----

export async function listWGHubs() {
  return requestJson<WGHubListResponse>("/api/admin/wg-hubs");
}

export async function updateWGHub(id: number, payload: Partial<WGHub>) {
  return requestJson<WGHubResponse>(`/api/admin/wg-hubs/${id}`, {
    method: "PUT",
    body: payload,
  });
}

export async function deleteWGHub(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-hubs/${id}`, {
    method: "DELETE",
  });
}

export async function resetWGHubBind(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-hubs/${id}/reset-bind`, {
    method: "POST",
  });
}

// ---- tokens ----

export async function listWGTokens() {
  return requestJson<WGTokenListResponse>("/api/admin/wg-tokens");
}

export async function createWGTokenForDevice(label: string, ttlDays: number, hubID: number) {
  return requestJson<WGTokenCreateResponse>("/api/admin/wg-tokens", {
    method: "POST",
    body: { label, role: "device", ttl_days: ttlDays, hub_id: hubID },
  });
}

export async function createWGTokenForHub(
  label: string,
  ttlDays: number,
  hubSlug: string,
  hubLabel: string,
  publicEndpoint: string,
  meshCIDR: string,
) {
  return requestJson<WGTokenCreateResponse>("/api/admin/wg-tokens", {
    method: "POST",
    body: {
      label,
      role: "hub",
      ttl_days: ttlDays,
      hub_slug: hubSlug,
      hub_label: hubLabel,
      public_endpoint: publicEndpoint,
      mesh_cidr: meshCIDR,
    },
  });
}

export async function revokeWGToken(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-tokens/${id}`, {
    method: "DELETE",
  });
}

// ---- devices ----

export async function listWGDevices(includeRemoved = false) {
  const qs = includeRemoved ? "?include_removed=1" : "";
  return requestJson<WGDeviceListResponse>(`/api/admin/wg-devices${qs}`);
}

export async function removeWGDevice(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-devices/${id}`, {
    method: "DELETE",
  });
}

// ---- sites ----

export async function listWGSites() {
  return requestJson<WGSiteListResponse>("/api/admin/wg-sites");
}

// ---- bundles ----

export async function listWGBundles() {
  return requestJson<WGBundleListResponse>("/api/admin/wg-bundles");
}

export async function uploadWGBundle(form: FormData): Promise<{ ok: boolean; status: number; data: WGBundleResponse }> {
  const resp = await fetch("/api/admin/wg-bundles/upload", {
    method: "POST",
    body: form,
    headers: {
      "X-Workspace-Id": localStorage.getItem("polar_active_workspace_id") || "",
    },
    credentials: "include",
  });
  const data: WGBundleResponse = await resp.json().catch(() => ({} as WGBundleResponse));
  return { ok: resp.ok, status: resp.status, data };
}

export async function setWGBundleLatest(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-bundles/${id}/latest`, {
    method: "PUT",
  });
}

export async function deleteWGBundle(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-bundles/${id}`, {
    method: "DELETE",
  });
}

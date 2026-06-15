// Typed fetch wrappers for /api/admin/wg-* (Phase 2 — multi-hub).

import { requestJson } from "@networkextension/polar-ui-common/api/http";
import type {
  WGBundleListResponse,
  WGBundleResponse,
  WGDevice,
  WGDeviceListResponse,
  WGHub,
  WGHubLinkListResponse,
  WGHubLinkResponse,
  WGHubListResponse,
  WGHubResponse,
  WGHubStatusResponse,
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

// ---- cross-hub links (route publishing) ----

export async function listWGHubLinks() {
  return requestJson<WGHubLinkListResponse>("/api/admin/wg-hub-links");
}

export async function createWGHubLink(hub_a_id: number, hub_b_id: number) {
  return requestJson<WGHubLinkResponse>("/api/admin/wg-hub-links", {
    method: "POST",
    body: { hub_a_id, hub_b_id },
  });
}

export async function deleteWGHubLink(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-hub-links/${id}`, {
    method: "DELETE",
  });
}

export async function resetWGHubBind(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-hubs/${id}/reset-bind`, {
    method: "POST",
  });
}

// ---- hub status (live, polled from dock cache) ----

export async function listWGHubStatus() {
  return requestJson<WGHubStatusResponse>("/api/admin/wg-hub-status");
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
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-tokens/${id}/revoke`, {
    method: "POST",
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

// P2 egress: set (number) or clear (null) a device's egress hub opt-in.
export async function updateWGDeviceEgress(id: number, egressHubID: number | null) {
  return requestJson<{ device?: WGDevice; error?: string }>(`/api/admin/wg-devices/${id}`, {
    method: "PUT",
    body: { egress_hub_id: egressHubID },
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

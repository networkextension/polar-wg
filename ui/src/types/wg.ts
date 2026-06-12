// Mirrors internal/app/dock/wg_store.go (Phase 2).

export type ErrorResponse = {
  error?: string;
  message?: string;
};

export type WGHub = {
  id: number;
  slug: string;
  label: string;
  pubkey: string;
  endpoint: string;
  wg_ip: string;
  mesh_cidr: string;
  keepalive_sec: number;
  refresh_sec: number;
  bound_device_id?: number;
  // P2 egress: operator-declared CIDRs this hub gateways to
  // (e.g. "192.168.10.0/24", "0.0.0.0/0"). Per-device opt-in.
  advertised_routes?: string[];
  created_at: string;
  updated_at: string;
};

export type WGSite = {
  id: number;
  hub_id: number;
  slug: string;
  s_index: number;
  label: string;
  public_ip: string;
  lan_cidr: string;
  created_at: string;
  device_count?: number;
};

export type WGToken = {
  id: number;
  label: string;
  token_prefix: string;
  role: "hub" | "device";
  hub_id?: number;
  public_endpoint?: string;
  mesh_cidr_pref?: string;
  expires_at?: string;
  consumed_at?: string;
  consumed_by_device_id?: number;
  revoked_at?: string;
  created_at: string;
  created_by_user_id?: string;
  plaintext?: string;
};

export type WGLanAddr = { iface: string; cidr: string };

export type WGDevice = {
  id: number;
  device_id: string;
  hub_id: number;
  site_id: number;
  d_index: number;
  device_ip: string;
  pubkey: string;
  hostname: string;
  host_id?: string; // soft ref to polar-hosts host.id (UI cross-link)
  os: string;
  arch: string;
  agent_ver: string;
  wg_listen_port: number;
  lan_addrs?: WGLanAddr[];
  wg_endpoint: string;
  token_expires_at?: string;
  // P2 egress opt-in: hub whose advertised_routes this device uses.
  egress_hub_id?: number | null;
  created_at: string;
  last_seen_at?: string;
  removed_at?: string;
  site_slug?: string;
  hub_slug?: string;
};

export type WGBundle = {
  id: number;
  version: string;
  blob_uri: string;
  blob_sha256: string;
  size_bytes: number;
  is_latest: boolean;
  notes: string;
  added_at: string;
  added_by_user_id: string;
};

export type WGHubListResponse = ErrorResponse & { hubs?: WGHub[] };
export type WGHubResponse = ErrorResponse & { hub?: WGHub };
export type WGTokenListResponse = ErrorResponse & { tokens?: WGToken[] };
export type WGTokenCreateResponse = ErrorResponse & { token?: WGToken; hub?: WGHub; warning?: string };
export type WGDeviceListResponse = ErrorResponse & { devices?: WGDevice[] };
export type WGSiteListResponse = ErrorResponse & { sites?: WGSite[] };
export type WGBundleListResponse = ErrorResponse & { bundles?: WGBundle[] };
export type WGBundleResponse = ErrorResponse & { bundle?: WGBundle; warning?: string; existing?: WGBundle };

// ---- hub status (from dock /internal/v1/wg-peer-status, joined by pubkey) ----

export type WGHubPeerSample = {
  public_key: string;
  endpoint?: string;
  allowed_ips?: string;
  latest_handshake_unix?: number;
  handshake_age_sec?: number;
  bytes_rx?: number;
  bytes_tx?: number;
  keepalive_sec?: number;
  has_preshared_key?: boolean;
};

export type WGHubIfaceNet = {
  addrs?: string[];
  routes?: string[];
};

export type WGHubStatusEntry = {
  host_id: string;
  iface: string;
  recorded_at: string;
  stale: boolean;
  peer_count: number;
  listen_port?: number;
  peers?: WGHubPeerSample[];
  extra?: Record<string, unknown> & { iface_net?: WGHubIfaceNet };
};

export type WGHubStatusRow = {
  id: number;
  slug: string;
  label: string;
  pubkey: string;
  endpoint: string;
  wg_ip: string;
  // P3 topology annotations: the hub's owned /24 + declared egress.
  mesh_cidr?: string;
  advertised_routes?: string[];
  status: WGHubStatusEntry | null;
};

export type WGHubStatusResponse = ErrorResponse & { hubs?: WGHubStatusRow[] };

// Token-create POST payload (multi-role).
export type WGTokenCreatePayload = {
  label: string;
  role: "hub" | "device";
  ttl_days: number;
} & (
  | { role: "hub"; hub_slug: string; hub_label: string; public_endpoint: string; mesh_cidr: string }
  | { role: "device"; hub_id: number }
);

-- ============================================================
-- polar_wg schema — Phase 2 end-state.
--
-- Apply order:
--   1. As a Postgres superuser:
--        CREATE DATABASE polar_wg OWNER ideamesh;
--   2. As `ideamesh` against polar_wg:
--        psql -d polar_wg -f scripts/migrate/wg-schema.sql
--
-- This file is the END state, not the migration history. The dock
-- store.go block (lines ~2870-3064) describes the legacy in-place
-- migration path (singleton wg_hub → plural wg_hubs + role/hub_id
-- on tokens/devices/sites). For a fresh polar_wg DB, none of that
-- legacy fixup is needed.
--
-- Tables owned by wg-svc (and the migration script wg-data.sh that
-- copies rows out of ideamesh into here):
--   - wg_hubs           : hub registry (per-LAN or per-region)
--   - wg_sites          : LAN partition (10.88.<S>.<D>) per hub
--   - wg_tokens         : single-use enroll tokens (role=hub|device)
--   - wg_devices        : joined devices with d_index unique per site
--   - wg_bundles        : admin-uploaded macOS client bundles
--   - wg_heartbeats     : append-only ping log (rx/tx + endpoint)
--
-- Cross-domain references that move to internal-API lookups:
--   - wg_tokens.created_by_user_id  → resolved via /internal/v1/users/:id
--   - wg_bundles.added_by_user_id   → same
-- These columns stay TEXT in polar_wg (no FK across DBs).
--
-- See doc/arch/database-ownership.md for the full ownership map.
-- ============================================================

CREATE TABLE IF NOT EXISTS wg_hubs (
    id BIGSERIAL PRIMARY KEY,
    slug TEXT UNIQUE NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    pubkey TEXT NOT NULL DEFAULT '',
    endpoint TEXT NOT NULL DEFAULT '',
    wg_ip TEXT NOT NULL DEFAULT '100.64.0.1',
    mesh_cidr TEXT NOT NULL DEFAULT '100.64.0.0/24',
    keepalive_sec INT NOT NULL DEFAULT 25,
    refresh_sec INT NOT NULL DEFAULT 300,
    bound_device_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wg_sites (
    id BIGSERIAL PRIMARY KEY,
    slug TEXT UNIQUE NOT NULL,
    -- s_index is unique PER HUB (see wg_sites_hub_sindex_uq below), NOT globally:
    -- every hub's "<hub>:hub" site sits at s_index=0, so a global UNIQUE rejects
    -- the 2nd hub. Kept un-inlined here; the scoped index is created post-table.
    s_index INT NOT NULL CHECK (s_index BETWEEN 0 AND 254),
    label TEXT NOT NULL DEFAULT '',
    public_ip TEXT NOT NULL DEFAULT '',
    lan_cidr TEXT NOT NULL DEFAULT '',
    hub_id BIGINT REFERENCES wg_hubs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_wg_sites_public_ip ON wg_sites(public_ip);
CREATE INDEX IF NOT EXISTS ix_wg_sites_hub ON wg_sites(hub_id);

CREATE TABLE IF NOT EXISTS wg_tokens (
    id BIGSERIAL PRIMARY KEY,
    label TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'device' CHECK (role IN ('hub', 'device')),
    hub_id BIGINT REFERENCES wg_hubs(id) ON DELETE SET NULL,
    public_endpoint TEXT,
    mesh_cidr_pref TEXT,
    expires_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    consumed_by_device_id BIGINT,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_user_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_wg_tokens_unused
    ON wg_tokens(token_hash) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_wg_tokens_hub ON wg_tokens(hub_id);

CREATE TABLE IF NOT EXISTS wg_devices (
    id BIGSERIAL PRIMARY KEY,
    device_id TEXT NOT NULL UNIQUE,
    site_id BIGINT NOT NULL REFERENCES wg_sites(id),
    d_index INT NOT NULL CHECK (d_index BETWEEN 1 AND 254),
    -- device_ip / pubkey / (site_id,d_index) uniqueness is enforced by the
    -- PARTIAL indexes below (WHERE removed_at IS NULL), NOT full constraints:
    -- a soft-removed device must not block reuse of its slot (pickFreeDIndex
    -- only counts live rows). Full UNIQUE here would 500 every re-register
    -- into a previously-removed d_index.
    device_ip TEXT NOT NULL,
    pubkey TEXT NOT NULL,
    hostname TEXT NOT NULL DEFAULT '',
    os TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL DEFAULT '',
    agent_ver TEXT NOT NULL DEFAULT '',
    wg_listen_port INT NOT NULL DEFAULT 51820,
    lan_addrs_json JSONB,
    wg_endpoint TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL,
    token_expires_at TIMESTAMPTZ,
    hub_id BIGINT REFERENCES wg_hubs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ,
    removed_at TIMESTAMPTZ,
    -- soft ref to polar-hosts host.id (UI cross-link only; no FK across plugins)
    host_id TEXT NOT NULL DEFAULT ''
);
-- Migration for DBs created before the partial-unique fix: drop the full
-- UNIQUE constraints so soft-removed devices stop blocking slot reuse.
ALTER TABLE wg_devices DROP CONSTRAINT IF EXISTS wg_devices_device_ip_key;
ALTER TABLE wg_devices DROP CONSTRAINT IF EXISTS wg_devices_pubkey_key;
ALTER TABLE wg_devices DROP CONSTRAINT IF EXISTS wg_devices_site_id_d_index_key;
-- Uniqueness scoped to LIVE rows only (matches pickFreeDIndex()).
CREATE UNIQUE INDEX IF NOT EXISTS wg_devices_device_ip_uq   ON wg_devices(device_ip)        WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS wg_devices_pubkey_uq      ON wg_devices(pubkey)           WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS wg_devices_site_dindex_uq ON wg_devices(site_id, d_index) WHERE removed_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_wg_devices_site ON wg_devices(site_id) WHERE removed_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_wg_devices_token_hash ON wg_devices(token_hash) WHERE removed_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_wg_devices_hub ON wg_devices(hub_id) WHERE removed_at IS NULL;
-- Soft ref to polar-hosts host.id for UI cross-linking (added 2026-06-07).
ALTER TABLE wg_devices ADD COLUMN IF NOT EXISTS host_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS ix_wg_devices_host_id ON wg_devices(host_id) WHERE host_id <> '';

CREATE TABLE IF NOT EXISTS wg_bundles (
    id BIGSERIAL PRIMARY KEY,
    version TEXT UNIQUE NOT NULL,
    blob_uri TEXT NOT NULL,
    blob_sha256 TEXT UNIQUE NOT NULL,
    size_bytes BIGINT NOT NULL,
    is_latest BOOLEAN NOT NULL DEFAULT FALSE,
    notes TEXT NOT NULL DEFAULT '',
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    added_by_user_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_wg_bundles_latest ON wg_bundles(is_latest) WHERE is_latest = TRUE;

CREATE TABLE IF NOT EXISTS wg_heartbeats (
    id BIGSERIAL PRIMARY KEY,
    device_id BIGINT NOT NULL REFERENCES wg_devices(id) ON DELETE CASCADE,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    rx_bytes BIGINT,
    tx_bytes BIGINT,
    last_handshake_sec BIGINT,
    wg_endpoint TEXT NOT NULL DEFAULT '',
    lan_addrs_json JSONB
);
CREATE INDEX IF NOT EXISTS ix_wg_heartbeats_device_ts
    ON wg_heartbeats(device_id, received_at DESC);

-- ── 2026-06-09 fix: s_index is unique PER HUB, not globally ─────────────────
-- The original table inlined `s_index INT UNIQUE`, but the Phase-2 multi-hub
-- allocator (pickFreeSIndexInHub / pickFreeDIndex) scopes every index to a hub.
-- Each hub's "<hub>:hub" site is created at s_index=0, so the global UNIQUE
-- rejected the SECOND hub's registration with
--   duplicate key value violates unique constraint "wg_sites_s_index_key"
-- → 500 from /v1/register. Drop the global constraint, re-scope to (hub_id, s_index).
ALTER TABLE wg_sites DROP CONSTRAINT IF EXISTS wg_sites_s_index_key;
CREATE UNIQUE INDEX IF NOT EXISTS wg_sites_hub_sindex_uq ON wg_sites(hub_id, s_index);

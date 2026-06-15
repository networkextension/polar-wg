package wg

// wg-mac join control plane — typed CRUD for the wg_hubs, wg_sites,
// wg_tokens, wg_devices, wg_bundles, wg_heartbeats tables defined in
// store.go. Phase 2 (multi-hub).
//
// See doc/JOIN_PROTOCOL.md §2 and doc/playbook.md §12.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- hubs (plural) ----

// WGHub is one row of wg_hubs. Phase 2: multiple hubs supported, each
// with its own slug + mesh_cidr + pubkey/endpoint pair. Hub becomes
// "bound" when the operator runs install.sh against a role=hub token
// and that device claims the hub slot (bound_device_id set).
type WGHub struct {
	ID            int64     `json:"id"`
	Slug          string    `json:"slug"`
	Label         string    `json:"label"`
	Pubkey        string    `json:"pubkey"`
	Endpoint      string    `json:"endpoint"`
	WGIP          string    `json:"wg_ip"`
	MeshCIDR      string    `json:"mesh_cidr"`
	KeepaliveSec  int       `json:"keepalive_sec"`
	RefreshSec    int       `json:"refresh_sec"`
	BoundDeviceID *int64    `json:"bound_device_id,omitempty"`
	// AdvertisedRoutes — operator-declared egress CIDRs this hub gateways
	// to (e.g. a datacenter LAN "192.168.10.0/24", or "0.0.0.0/0" full
	// tunnel). Per-spoke opt-in via wg_devices.egress_hub_id — never
	// auto-distributed. See doc/wg-multi-hub-routing.md 出口 section.
	AdvertisedRoutes []string  `json:"advertised_routes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Configured iff a device has claimed this hub (pubkey + endpoint set).
// Operator-created token has the endpoint pre-stamped but not pubkey;
// only after the bind is the hub usable as a peer for other devices.
func (h *WGHub) Configured() bool {
	return strings.TrimSpace(h.Pubkey) != ""
}

const wgHubColumns = `id, slug, label, pubkey, endpoint, wg_ip, mesh_cidr, keepalive_sec, refresh_sec, bound_device_id, advertised_routes_json, created_at, updated_at`

func scanWGHub(scanner interface{ Scan(...any) error }) (*WGHub, error) {
	var h WGHub
	var bound sql.NullInt64
	var routesJSON sql.NullString
	if err := scanner.Scan(&h.ID, &h.Slug, &h.Label, &h.Pubkey, &h.Endpoint, &h.WGIP, &h.MeshCIDR,
		&h.KeepaliveSec, &h.RefreshSec, &bound, &routesJSON, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	if bound.Valid {
		v := bound.Int64
		h.BoundDeviceID = &v
	}
	if routesJSON.Valid && routesJSON.String != "" {
		_ = json.Unmarshal([]byte(routesJSON.String), &h.AdvertisedRoutes)
	}
	return &h, nil
}

func (p *Plugin) listWGHubs() ([]WGHub, error) {
	rows, err := p.DB.Query(`SELECT ` + wgHubColumns + ` FROM wg_hubs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGHub, 0)
	for rows.Next() {
		h, err := scanWGHub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

func (p *Plugin) getWGHubByID(id int64) (*WGHub, error) {
	row := p.DB.QueryRow(`SELECT `+wgHubColumns+` FROM wg_hubs WHERE id = $1`, id)
	h, err := scanWGHub(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return h, nil
}

func (p *Plugin) getWGHubBySlug(slug string) (*WGHub, error) {
	row := p.DB.QueryRow(`SELECT `+wgHubColumns+` FROM wg_hubs WHERE slug = $1`, slug)
	h, err := scanWGHub(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return h, nil
}

// getWGHubByIDTx is the tx-aware variant used by the allocator inside
// its SERIALIZABLE transaction.
func getWGHubByIDTx(tx *sql.Tx, id int64) (*WGHub, error) {
	row := tx.QueryRow(`SELECT `+wgHubColumns+` FROM wg_hubs WHERE id = $1`, id)
	h, err := scanWGHub(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return h, nil
}

// createWGHub inserts a new wg_hubs row. Called when a role=hub token
// is minted — operator pre-stamps slug/label/public_endpoint/mesh_cidr.
// pubkey + bound_device_id stay empty until the corresponding
// /v1/register call.
func (p *Plugin) createWGHub(slug, label, publicEndpoint, meshCIDR string) (*WGHub, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, errors.New("hub slug required")
	}
	if meshCIDR == "" {
		meshCIDR = "100.64.0.0/24"
	}
	wgIP, err := hubWGIPFromCIDR(meshCIDR)
	if err != nil {
		return nil, err
	}
	var id int64
	err = p.DB.QueryRow(
		`INSERT INTO wg_hubs (slug, label, endpoint, mesh_cidr, wg_ip)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		slug, strings.TrimSpace(label), strings.TrimSpace(publicEndpoint), meshCIDR, wgIP,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return p.getWGHubByID(id)
}

// updateWGHubBind atomically claims a hub for a device: sets pubkey +
// bound_device_id. Called by the allocator inside the register txn.
func updateWGHubBindTx(tx *sql.Tx, hubID int64, pubkey, observedEndpoint string, deviceID int64, now time.Time) error {
	pubkey = strings.TrimSpace(pubkey)
	// observedEndpoint overrides the operator-prestamped endpoint only
	// if the prestamp was empty — operator's value is authoritative
	// (they might have set a DDNS name that's more correct than the
	// observed publicIP).
	_, err := tx.Exec(
		`UPDATE wg_hubs
		    SET pubkey = $1,
		        endpoint = CASE WHEN endpoint = '' THEN $2 ELSE endpoint END,
		        bound_device_id = $3,
		        updated_at = $4
		  WHERE id = $5`,
		pubkey, observedEndpoint, deviceID, now, hubID,
	)
	return err
}

// clearWGHubBind resets pubkey + bound_device_id (called from
// /v1/leave when the hub device itself leaves). The hub row stays —
// it can be re-bound by a fresh hub-token register, or admin can
// delete it.
func (p *Plugin) clearWGHubBind(hubID int64, now time.Time) error {
	_, err := p.DB.Exec(
		`UPDATE wg_hubs
		    SET pubkey = '', bound_device_id = NULL, updated_at = $2
		  WHERE id = $1`,
		hubID, now,
	)
	return err
}

func (p *Plugin) updateWGHub(h *WGHub, now time.Time) (*WGHub, error) {
	if h == nil {
		return nil, errors.New("nil hub")
	}
	if h.WGIP == "" {
		h.WGIP = "100.64.0.1"
	}
	if h.MeshCIDR == "" {
		h.MeshCIDR = "100.64.0.0/24"
	}
	if h.KeepaliveSec <= 0 {
		h.KeepaliveSec = 25
	}
	if h.RefreshSec <= 0 {
		h.RefreshSec = 300
	}
	var routesJSON []byte
	if len(h.AdvertisedRoutes) > 0 {
		routesJSON, _ = json.Marshal(h.AdvertisedRoutes)
	}
	_, err := p.DB.Exec(
		`UPDATE wg_hubs
		    SET label = $2, endpoint = $3, wg_ip = $4, mesh_cidr = $5,
		        keepalive_sec = $6, refresh_sec = $7, advertised_routes_json = $8,
		        updated_at = $9
		  WHERE id = $1`,
		h.ID, strings.TrimSpace(h.Label), strings.TrimSpace(h.Endpoint), h.WGIP, h.MeshCIDR,
		h.KeepaliveSec, h.RefreshSec, nullJSONB(routesJSON), now,
	)
	if err != nil {
		return nil, err
	}
	return p.getWGHubByID(h.ID)
}

func (p *Plugin) deleteWGHub(id int64) error {
	// Refuse if hub has live devices. ON DELETE CASCADE on wg_devices
	// would otherwise silently kill them; safer to make admin clean up
	// explicitly.
	var n int
	if err := p.DB.QueryRow(
		`SELECT COUNT(*) FROM wg_devices WHERE hub_id = $1 AND removed_at IS NULL`, id,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("hub has %d live devices; remove them first", n)
	}
	res, err := p.DB.Exec(`DELETE FROM wg_hubs WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---- wg_hub_links: operator-published cross-hub interconnects ----
//
// A row is an undirected, published link between two hubs (stored normalized
// with hub_a_id < hub_b_id). Only linked hub pairs cross-advertise their /24s;
// absence of a link means no cross-hub routing. This makes route distribution
// an explicit operator action ("publish") rather than automatic full-mesh.

// WGHubLink is one published interconnect (with both hubs' slugs for the UI).
type WGHubLink struct {
	ID        int64     `json:"id"`
	HubAID    int64     `json:"hub_a_id"`
	HubBID    int64     `json:"hub_b_id"`
	HubASlug  string    `json:"hub_a_slug,omitempty"`
	HubBSlug  string    `json:"hub_b_slug,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func normHubPair(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

// hubLinkSet returns the set of hub IDs linked to hubID (published links only).
func (p *Plugin) hubLinkSet(hubID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	rows, err := p.DB.Query(
		`SELECT hub_a_id, hub_b_id FROM wg_hub_links WHERE hub_a_id = $1 OR hub_b_id = $1`, hubID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		if a == hubID {
			out[b] = true
		} else {
			out[a] = true
		}
	}
	return out, rows.Err()
}

func (p *Plugin) listWGHubLinks() ([]WGHubLink, error) {
	rows, err := p.DB.Query(`
		SELECT l.id, l.hub_a_id, l.hub_b_id, a.slug, b.slug, l.created_at
		  FROM wg_hub_links l
		  JOIN wg_hubs a ON a.id = l.hub_a_id
		  JOIN wg_hubs b ON b.id = l.hub_b_id
		 ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGHubLink, 0)
	for rows.Next() {
		var l WGHubLink
		if err := rows.Scan(&l.ID, &l.HubAID, &l.HubBID, &l.HubASlug, &l.HubBSlug, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// createWGHubLink publishes a link between two distinct hubs (idempotent).
func (p *Plugin) createWGHubLink(a, b int64) (*WGHubLink, error) {
	if a == b {
		return nil, fmt.Errorf("cannot link a hub to itself")
	}
	a, b = normHubPair(a, b)
	var l WGHubLink
	err := p.DB.QueryRow(`
		INSERT INTO wg_hub_links (hub_a_id, hub_b_id) VALUES ($1, $2)
		ON CONFLICT (hub_a_id, hub_b_id) DO UPDATE SET hub_a_id = EXCLUDED.hub_a_id
		RETURNING id, hub_a_id, hub_b_id, created_at`, a, b,
	).Scan(&l.ID, &l.HubAID, &l.HubBID, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (p *Plugin) deleteWGHubLink(id int64) error {
	res, err := p.DB.Exec(`DELETE FROM wg_hub_links WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// suggestFreeMeshCIDR returns the lowest unused /24 from RFC 6598
// CGNAT (100.64.0.0/10), iterating 100.64.0.0/24 → 100.64.1.0/24 →
// … → 100.127.255.0/24. CGNAT is reserved for carrier-grade NAT,
// virtually never appears in home/office LANs, so mesh traffic
// doesn't fight with 10.x/172.16.x/192.168.x routes on the device.
// Tailscale uses the same range; we deliberately pick non-colliding
// /24s rather than reusing slots inside one /16.
func (p *Plugin) suggestFreeMeshCIDR() (string, error) {
	rows, err := p.DB.Query(`SELECT mesh_cidr FROM wg_hubs`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	used := make(map[string]bool)
	for rows.Next() {
		var cidr string
		if err := rows.Scan(&cidr); err != nil {
			return "", err
		}
		used[strings.TrimSpace(cidr)] = true
	}
	// 100.64.0.0/10 spans 100.64.0.0 .. 100.127.255.255.
	for x := 64; x <= 127; x++ {
		for y := 0; y <= 255; y++ {
			c := fmt.Sprintf("100.%d.%d.0/24", x, y)
			if !used[c] {
				return c, nil
			}
		}
	}
	return "", errors.New("no free /24 in 100.64.0.0/10 (impossible: 16K slots)")
}

// hubWGIPFromCIDR derives the canonical hub IP (network address + 1)
// from a CIDR like "10.88.0.0/24" → "10.88.0.1".
func hubWGIPFromCIDR(cidr string) (string, error) {
	net, err := parseMeshCIDR(cidr)
	if err != nil {
		return "", err
	}
	return net.deviceIP(1), nil
}

// ---- sites ----

type WGSite struct {
	ID          int64     `json:"id"`
	HubID       int64     `json:"hub_id"`
	Slug        string    `json:"slug"`
	SIndex      int       `json:"s_index"`
	Label       string    `json:"label"`
	PublicIP    string    `json:"public_ip"`
	LANCIDR     string    `json:"lan_cidr"`
	CreatedAt   time.Time `json:"created_at"`
	DeviceCount int       `json:"device_count,omitempty"`
}

func (p *Plugin) listWGSites() ([]WGSite, error) {
	rows, err := p.DB.Query(
		`SELECT s.id, COALESCE(s.hub_id, 0), s.slug, s.s_index, s.label, s.public_ip, s.lan_cidr, s.created_at,
		        COALESCE((SELECT COUNT(*) FROM wg_devices d
		                   WHERE d.site_id = s.id AND d.removed_at IS NULL), 0)::INT
		   FROM wg_sites s
		  ORDER BY s.hub_id, s.s_index`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGSite, 0)
	for rows.Next() {
		var x WGSite
		if err := rows.Scan(&x.ID, &x.HubID, &x.Slug, &x.SIndex, &x.Label, &x.PublicIP, &x.LANCIDR, &x.CreatedAt, &x.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- tokens ----

type WGToken struct {
	ID                 int64      `json:"id"`
	Label              string     `json:"label"`
	Prefix             string     `json:"token_prefix"`
	Role               string     `json:"role"` // "hub" | "device"
	HubID              *int64     `json:"hub_id,omitempty"`
	PublicEndpoint     *string    `json:"public_endpoint,omitempty"` // hub tokens only
	MeshCIDRPref       *string    `json:"mesh_cidr_pref,omitempty"`  // hub tokens only
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
	ConsumedByDeviceID *int64     `json:"consumed_by_device_id,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	CreatedByUserID    string     `json:"created_by_user_id,omitempty"`
	// Plaintext is set ONLY on create. Never read back from DB.
	Plaintext string `json:"plaintext,omitempty"`
}

const wgTokenPlaintextPrefix = "polar_wg_"

func newWGTokenPlaintext() (plaintext, prefix string) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	plaintext = wgTokenPlaintextPrefix + hex.EncodeToString(b)
	prefix = plaintext[:len(wgTokenPlaintextPrefix)+8]
	return
}

func hashWGToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

const wgTokenColumns = `id, label, token_prefix, role, hub_id, public_endpoint, mesh_cidr_pref, expires_at, consumed_at, consumed_by_device_id, revoked_at, created_at, created_by_user_id`

func scanWGToken(scanner interface{ Scan(...any) error }) (*WGToken, error) {
	var t WGToken
	var hubID, consumedBy sql.NullInt64
	var publicEndpoint, meshCIDR sql.NullString
	var expiresAt, consumedAt, revokedAt sql.NullTime
	if err := scanner.Scan(
		&t.ID, &t.Label, &t.Prefix, &t.Role, &hubID, &publicEndpoint, &meshCIDR,
		&expiresAt, &consumedAt, &consumedBy, &revokedAt, &t.CreatedAt, &t.CreatedByUserID,
	); err != nil {
		return nil, err
	}
	if hubID.Valid {
		v := hubID.Int64
		t.HubID = &v
	}
	if publicEndpoint.Valid {
		v := publicEndpoint.String
		t.PublicEndpoint = &v
	}
	if meshCIDR.Valid {
		v := meshCIDR.String
		t.MeshCIDRPref = &v
	}
	if expiresAt.Valid {
		v := expiresAt.Time
		t.ExpiresAt = &v
	}
	if consumedAt.Valid {
		v := consumedAt.Time
		t.ConsumedAt = &v
	}
	if consumedBy.Valid {
		v := consumedBy.Int64
		t.ConsumedByDeviceID = &v
	}
	if revokedAt.Valid {
		v := revokedAt.Time
		t.RevokedAt = &v
	}
	return &t, nil
}

// createWGTokenForDevice mints a device-role token bound to an
// existing hub. UI/CLI rejects creation if 0 hubs exist.
func (p *Plugin) createWGTokenForDevice(label, createdByUserID string, hubID int64, expiresAt *time.Time, now time.Time) (*WGToken, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, errors.New("label required")
	}
	// Sanity: hub exists.
	hub, err := p.getWGHubByID(hubID)
	if err != nil {
		return nil, err
	}
	if hub == nil {
		return nil, errors.New("hub_id not found")
	}
	plaintext, prefix := newWGTokenPlaintext()
	hash := hashWGToken(plaintext)
	var newID int64
	err = p.DB.QueryRow(
		`INSERT INTO wg_tokens (label, token_hash, token_prefix, expires_at, created_at, created_by_user_id, role, hub_id)
		 VALUES ($1, $2, $3, $4, $5, $6, 'device', $7)
		 RETURNING id`,
		label, hash, prefix, expiresAt, now, createdByUserID, hubID,
	).Scan(&newID)
	if err != nil {
		return nil, err
	}
	out, err := p.getWGTokenByID(newID)
	if err != nil {
		return nil, err
	}
	out.Plaintext = plaintext
	return out, nil
}

// createWGTokenForHub mints a hub-role token AND pre-creates the
// wg_hubs row (so device tokens can target it before the hub goes
// live). The hub row starts with pubkey="" and is "configured" only
// after the corresponding /v1/register call binds a device.
//
// If meshCIDR is empty, picks the lowest unused /24 from the RFC 6598
// CGNAT range 100.64.0.0/10 to avoid colliding with the operator's
// existing 10.x / 172.16.x / 192.168.x LANs.
func (p *Plugin) createWGTokenForHub(label, createdByUserID, hubSlug, hubLabel, publicEndpoint, meshCIDR string, expiresAt *time.Time, now time.Time) (*WGToken, *WGHub, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, nil, errors.New("label required")
	}
	hubSlug = strings.TrimSpace(hubSlug)
	if hubSlug == "" {
		return nil, nil, errors.New("hub slug required for role=hub")
	}
	publicEndpoint = strings.TrimSpace(publicEndpoint)
	if publicEndpoint == "" {
		return nil, nil, errors.New("public_endpoint required for role=hub (e.g. west.example.com:51820)")
	}
	if meshCIDR == "" {
		picked, err := p.suggestFreeMeshCIDR()
		if err != nil {
			return nil, nil, fmt.Errorf("suggest mesh_cidr: %w", err)
		}
		meshCIDR = picked
	}
	if _, err := parseMeshCIDR(meshCIDR); err != nil {
		return nil, nil, fmt.Errorf("invalid mesh_cidr: %w", err)
	}
	// Reuse existing hub by slug if present (re-issue scenario: the
	// previous hub-token was lost or the hub device crashed).
	hub, err := p.getWGHubBySlug(hubSlug)
	if err != nil {
		return nil, nil, err
	}
	if hub == nil {
		// Mint-time disjointness guard: cross-hub routing (P1) requires
		// every hub's /24 to be unique; an operator-supplied mesh_cidr
		// colliding with an existing hub would make route ownership
		// ambiguous. suggestFreeMeshCIDR-picked values pass trivially.
		allHubs, lerr := p.listWGHubs()
		if lerr != nil {
			return nil, nil, lerr
		}
		if verr := validateMeshCIDRDisjoint(meshCIDR, allHubs, 0); verr != nil {
			return nil, nil, verr
		}
		hub, err = p.createWGHub(hubSlug, hubLabel, publicEndpoint, meshCIDR)
		if err != nil {
			return nil, nil, err
		}
	}
	plaintext, prefix := newWGTokenPlaintext()
	hash := hashWGToken(plaintext)
	var newID int64
	err = p.DB.QueryRow(
		`INSERT INTO wg_tokens (label, token_hash, token_prefix, expires_at, created_at, created_by_user_id, role, hub_id, public_endpoint, mesh_cidr_pref)
		 VALUES ($1, $2, $3, $4, $5, $6, 'hub', $7, $8, $9)
		 RETURNING id`,
		label, hash, prefix, expiresAt, now, createdByUserID, hub.ID, publicEndpoint, meshCIDR,
	).Scan(&newID)
	if err != nil {
		return nil, nil, err
	}
	out, err := p.getWGTokenByID(newID)
	if err != nil {
		return nil, nil, err
	}
	out.Plaintext = plaintext
	return out, hub, nil
}

func (p *Plugin) getWGTokenByID(id int64) (*WGToken, error) {
	row := p.DB.QueryRow(`SELECT `+wgTokenColumns+` FROM wg_tokens WHERE id = $1`, id)
	t, err := scanWGToken(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

func (p *Plugin) listWGTokens() ([]WGToken, error) {
	rows, err := p.DB.Query(`SELECT ` + wgTokenColumns + ` FROM wg_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGToken, 0)
	for rows.Next() {
		t, err := scanWGToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (p *Plugin) revokeWGToken(id int64, now time.Time) error {
	res, err := p.DB.Exec(
		`UPDATE wg_tokens
		    SET revoked_at = COALESCE(revoked_at, $2)
		  WHERE id = $1`,
		id, now,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// lookupUnusedWGToken returns the row matching plaintext IFF the token
// is not yet consumed, not revoked, and not expired. Caller binds the
// token to a device within the same transaction.
func (p *Plugin) lookupUnusedWGToken(tx *sql.Tx, plaintext string) (*WGToken, error) {
	hash := hashWGToken(plaintext)
	row := tx.QueryRow(`SELECT `+wgTokenColumns+` FROM wg_tokens WHERE token_hash = $1`, hash)
	t, err := scanWGToken(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if t.ExpiresAt != nil && time.Now().UTC().After(*t.ExpiresAt) {
		return nil, errWGTokenExpired
	}
	if t.RevokedAt != nil {
		return nil, errWGTokenRevoked
	}
	return t, nil
}

var (
	errWGTokenExpired = errors.New("wg token expired")
	errWGTokenRevoked = errors.New("wg token revoked")
)

// ---- devices ----

type WGLanAddr struct {
	Iface string `json:"iface"`
	CIDR  string `json:"cidr"`
}

type WGDevice struct {
	ID             int64       `json:"id"`
	DeviceID       string      `json:"device_id"`
	HubID          int64       `json:"hub_id"`
	SiteID         int64       `json:"site_id"`
	DIndex         int         `json:"d_index"`
	DeviceIP       string      `json:"device_ip"`
	Pubkey         string      `json:"pubkey"`
	Hostname       string      `json:"hostname"`
	HostID         string      `json:"host_id,omitempty"` // soft ref to polar-hosts host.id (UI cross-link)
	OS             string      `json:"os"`
	Arch           string      `json:"arch"`
	AgentVer       string      `json:"agent_ver"`
	WGListenPort   int         `json:"wg_listen_port"`
	LANAddrs       []WGLanAddr `json:"lan_addrs,omitempty"`
	WGEndpoint     string      `json:"wg_endpoint"`
	TokenHash      string      `json:"-"`
	TokenExpiresAt *time.Time  `json:"token_expires_at,omitempty"`
	// EgressHubID — per-device egress opt-in: the hub whose
	// advertised_routes this device wants. NULL/nil = no egress.
	// 0.0.0.0/0 only takes effect when this equals the device's own hub.
	EgressHubID *int64     `json:"egress_hub_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	RemovedAt   *time.Time `json:"removed_at,omitempty"`
	SiteSlug    string     `json:"site_slug,omitempty"`
	HubSlug     string     `json:"hub_slug,omitempty"`
}

// scanWGDevice supports two column shapes used in different queries.
// Set hasSlug=true when the SELECT joined site/hub for *_slug columns.
func scanWGDevice(scanner interface{ Scan(...any) error }, hasSlug bool) (*WGDevice, error) {
	var d WGDevice
	var lanJSON sql.NullString
	var tokenExpiresAt, lastSeenAt, removedAt sql.NullTime
	var egressHubID sql.NullInt64
	var siteSlug, hubSlug sql.NullString
	dest := []any{
		&d.ID, &d.DeviceID, &d.HubID, &d.SiteID, &d.DIndex, &d.DeviceIP, &d.Pubkey,
		&d.Hostname, &d.OS, &d.Arch, &d.AgentVer, &d.WGListenPort, &lanJSON, &d.WGEndpoint,
		&d.TokenHash, &tokenExpiresAt, &d.CreatedAt, &lastSeenAt, &removedAt, &d.HostID,
		&egressHubID,
	}
	if hasSlug {
		dest = append(dest, &siteSlug, &hubSlug)
	}
	if err := scanner.Scan(dest...); err != nil {
		return nil, err
	}
	if lanJSON.Valid && lanJSON.String != "" {
		_ = json.Unmarshal([]byte(lanJSON.String), &d.LANAddrs)
	}
	if egressHubID.Valid {
		v := egressHubID.Int64
		d.EgressHubID = &v
	}
	if tokenExpiresAt.Valid {
		v := tokenExpiresAt.Time
		d.TokenExpiresAt = &v
	}
	if lastSeenAt.Valid {
		v := lastSeenAt.Time
		d.LastSeenAt = &v
	}
	if removedAt.Valid {
		v := removedAt.Time
		d.RemovedAt = &v
	}
	if hasSlug {
		if siteSlug.Valid {
			d.SiteSlug = siteSlug.String
		}
		if hubSlug.Valid {
			d.HubSlug = hubSlug.String
		}
	}
	return &d, nil
}

const wgDeviceColumns = `id, device_id, hub_id, site_id, d_index, device_ip, pubkey, hostname, os, arch, agent_ver, wg_listen_port, lan_addrs_json, wg_endpoint, token_hash, token_expires_at, created_at, last_seen_at, removed_at, host_id, egress_hub_id`

func (p *Plugin) getWGDeviceByID(id int64) (*WGDevice, error) {
	row := p.DB.QueryRow(`SELECT `+wgDeviceColumns+` FROM wg_devices WHERE id = $1`, id)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (p *Plugin) getWGDeviceByDeviceID(deviceID string) (*WGDevice, error) {
	row := p.DB.QueryRow(`SELECT `+wgDeviceColumns+` FROM wg_devices WHERE device_id = $1`, deviceID)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (p *Plugin) getWGDeviceByTokenHash(hash string) (*WGDevice, error) {
	row := p.DB.QueryRow(
		`SELECT `+wgDeviceColumns+`
		   FROM wg_devices
		  WHERE token_hash = $1 AND removed_at IS NULL`,
		hash,
	)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (p *Plugin) listWGDevices(includeRemoved bool) ([]WGDevice, error) {
	q := `SELECT d.id, d.device_id, d.hub_id, d.site_id, d.d_index, d.device_ip, d.pubkey,
	             d.hostname, d.os, d.arch, d.agent_ver, d.wg_listen_port, d.lan_addrs_json,
	             d.wg_endpoint, d.token_hash, d.token_expires_at, d.created_at, d.last_seen_at,
	             d.removed_at, d.host_id, d.egress_hub_id, s.slug, h.slug
	        FROM wg_devices d
	   LEFT JOIN wg_sites s ON s.id = d.site_id
	   LEFT JOIN wg_hubs  h ON h.id = d.hub_id`
	if !includeRemoved {
		q += ` WHERE d.removed_at IS NULL`
	}
	q += ` ORDER BY d.hub_id, d.site_id, d.d_index`
	rows, err := p.DB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGDevice, 0)
	for rows.Next() {
		d, err := scanWGDevice(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// listWGDevicesInHub returns un-removed devices in a given hub,
// optionally excluding one (the requesting peer / hub itself).
func (p *Plugin) listWGDevicesInHub(hubID, excludeID int64) ([]WGDevice, error) {
	rows, err := p.DB.Query(
		`SELECT `+wgDeviceColumns+`
		   FROM wg_devices
		  WHERE hub_id = $1 AND removed_at IS NULL AND id <> $2
		  ORDER BY d_index`,
		hubID, excludeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGDevice, 0)
	for rows.Next() {
		d, err := scanWGDevice(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// listWGDevicesInSite — like listWGDevicesInHub but scoped to a site
// (used for LAN-direct peer list within a single LAN).
func (p *Plugin) listWGDevicesInSite(siteID, excludeID int64) ([]WGDevice, error) {
	rows, err := p.DB.Query(
		`SELECT `+wgDeviceColumns+`
		   FROM wg_devices
		  WHERE site_id = $1 AND removed_at IS NULL AND id <> $2
		  ORDER BY d_index`,
		siteID, excludeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGDevice, 0)
	for rows.Next() {
		d, err := scanWGDevice(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// hubIDsForHost returns the set of hub IDs that have a live device on the same
// physical host — matched by host_id when present, falling back to hostname.
// Used to suppress a cross-hub /24 for a mesh the host already joins directly
// (a dual-homed box). Over-matching on a duplicate hostname only drops a
// redundant cross-route, never a security boundary, so the fallback is safe.
func (p *Plugin) hubIDsForHost(hostID, hostname string) (map[int64]bool, error) {
	out := map[int64]bool{}
	hostID = strings.TrimSpace(hostID)
	hostname = strings.TrimSpace(hostname)
	if hostID == "" && hostname == "" {
		return out, nil
	}
	rows, err := p.DB.Query(
		`SELECT DISTINCT hub_id
		   FROM wg_devices
		  WHERE removed_at IS NULL
		    AND ( ($1 <> '' AND host_id = $1) OR ($2 <> '' AND hostname = $2) )`,
		hostID, hostname,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// updateWGDeviceEgressHub sets (or clears, with nil) a device's egress
// opt-in. The egress hub must exist; cross-hub vs own-hub semantics are
// enforced at the handler layer (full tunnel only via own hub).
func (p *Plugin) updateWGDeviceEgressHub(deviceID int64, egressHubID *int64) error {
	var v any
	if egressHubID != nil {
		v = *egressHubID
	}
	res, err := p.DB.Exec(
		`UPDATE wg_devices SET egress_hub_id = $2 WHERE id = $1 AND removed_at IS NULL`,
		deviceID, v,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (p *Plugin) markWGDeviceRemoved(id int64, now time.Time) error {
	res, err := p.DB.Exec(
		`UPDATE wg_devices
		    SET removed_at = COALESCE(removed_at, $2)
		  WHERE id = $1`,
		id, now,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// linkWGDeviceHostByPubkey stamps host_id on the live device whose pubkey
// matches, for the WG→Hosts UI cross-link (doc/arch/wg-host-crosslink.md).
// Idempotent and additive: only writes when the value differs (so a
// repeated hello is a no-op) and never clears an existing link with an
// empty host_id. Returns the number of rows changed (0 when the pubkey is
// unknown to this hub or already linked).
// wgDeviceHostIDsByPubkey returns pubkey → host_id for every live device that
// carries a host_id. Used to enrich the hub-status peer samples (which the
// live wgctl poll keys only by pubkey) so the UI can deep-link a peer to its
// polar-hosts host. doc/arch/wg-host-crosslink.md.
func (p *Plugin) wgDeviceHostIDsByPubkey() (map[string]string, error) {
	rows, err := p.DB.Query(
		`SELECT pubkey, host_id FROM wg_devices
		  WHERE removed_at IS NULL AND host_id <> ''`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var pk, hid string
		if err := rows.Scan(&pk, &hid); err != nil {
			return nil, err
		}
		if pk = strings.TrimSpace(pk); pk != "" {
			out[pk] = hid
		}
	}
	return out, rows.Err()
}

// linkWGDeviceHostByDeviceIP stamps host_id on the live device with this mesh
// IP (wg_devices.device_ip). The NE-proof variant of linkWGDeviceHostByPubkey:
// the agent's mesh IP is readable on every box (ifconfig), unlike the WG
// pubkey on wg-mac NE clients. Idempotent + additive (IS DISTINCT FROM, never
// clears). doc/arch/wg-host-crosslink.md.
func (p *Plugin) linkWGDeviceHostByDeviceIP(deviceIP, hostID string) (int, error) {
	res, err := p.DB.Exec(
		`UPDATE wg_devices
		    SET host_id = $2
		  WHERE device_ip = $1 AND removed_at IS NULL AND host_id IS DISTINCT FROM $2`,
		strings.TrimSpace(deviceIP), strings.TrimSpace(hostID),
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (p *Plugin) linkWGDeviceHostByPubkey(pubkey, hostID string) (int, error) {
	res, err := p.DB.Exec(
		`UPDATE wg_devices
		    SET host_id = $2
		  WHERE pubkey = $1 AND removed_at IS NULL AND host_id IS DISTINCT FROM $2`,
		strings.TrimSpace(pubkey), strings.TrimSpace(hostID),
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (p *Plugin) updateWGDeviceTokenHash(id int64, newHash string, expiresAt *time.Time) error {
	_, err := p.DB.Exec(
		`UPDATE wg_devices
		    SET token_hash = $2, token_expires_at = $3
		  WHERE id = $1 AND removed_at IS NULL`,
		id, newHash, expiresAt,
	)
	return err
}

// ---- bundles ---- (unchanged shape, see Phase 1)

type WGBundle struct {
	ID            int64     `json:"id"`
	Version       string    `json:"version"`
	OS            string    `json:"os"`   // darwin|linux|windows; packaging is polar-wg-app's job
	Arch          string    `json:"arch"` // amd64|arm64, or "" = universal/any
	BlobURI       string    `json:"blob_uri"`
	BlobSHA256    string    `json:"blob_sha256"`
	SizeBytes     int64     `json:"size_bytes"`
	IsLatest      bool      `json:"is_latest"`
	Notes         string    `json:"notes"`
	AddedAt       time.Time `json:"added_at"`
	AddedByUserID string    `json:"added_by_user_id"`
}

// wgBundleCols / scanWGBundle keep the column list in one place so the os/arch
// columns stay in sync across every getter.
const wgBundleCols = `id, version, os, arch, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id`

func scanWGBundle(s interface{ Scan(...any) error }) (*WGBundle, error) {
	var b WGBundle
	if err := s.Scan(&b.ID, &b.Version, &b.OS, &b.Arch, &b.BlobURI, &b.BlobSHA256, &b.SizeBytes, &b.IsLatest, &b.Notes, &b.AddedAt, &b.AddedByUserID); err != nil {
		return nil, err
	}
	return &b, nil
}

func (p *Plugin) listWGBundles() ([]WGBundle, error) {
	rows, err := p.DB.Query(`SELECT ` + wgBundleCols + ` FROM wg_bundles ORDER BY added_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGBundle, 0)
	for rows.Next() {
		b, err := scanWGBundle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (p *Plugin) getWGBundleByID(id int64) (*WGBundle, error) {
	b, err := scanWGBundle(p.DB.QueryRow(`SELECT `+wgBundleCols+` FROM wg_bundles WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func (p *Plugin) getWGBundleBySHA(sha string) (*WGBundle, error) {
	b, err := scanWGBundle(p.DB.QueryRow(`SELECT `+wgBundleCols+` FROM wg_bundles WHERE blob_sha256 = $1`, sha))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// getWGBundleForVersion resolves a specific version scoped to a platform.
// Arch fallback: an exact (os,arch) match wins, else a universal (os,arch="")
// bundle for the same OS. os defaults to "darwin" when empty (back-compat with
// the original macOS-only bundles).
func (p *Plugin) getWGBundleForVersion(version, os, arch string) (*WGBundle, error) {
	if strings.TrimSpace(os) == "" {
		os = "darwin"
	}
	b, err := scanWGBundle(p.DB.QueryRow(
		`SELECT `+wgBundleCols+` FROM wg_bundles
		  WHERE version = $1 AND os = $2 AND (arch = $3 OR arch = '')
		  ORDER BY (arch = $3) DESC LIMIT 1`,
		version, os, arch))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// getLatestWGBundleFor returns the latest bundle for a platform (same arch
// fallback as getWGBundleForVersion).
func (p *Plugin) getLatestWGBundleFor(os, arch string) (*WGBundle, error) {
	if strings.TrimSpace(os) == "" {
		os = "darwin"
	}
	b, err := scanWGBundle(p.DB.QueryRow(
		`SELECT `+wgBundleCols+` FROM wg_bundles
		  WHERE is_latest = TRUE AND os = $1 AND (arch = $2 OR arch = '')
		  ORDER BY (arch = $2) DESC LIMIT 1`,
		os, arch))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// getLatestWGBundle (legacy, no platform) → latest darwin bundle. Kept so
// existing callers compile; new code uses getLatestWGBundleFor.
func (p *Plugin) getLatestWGBundle() (*WGBundle, error) {
	return p.getLatestWGBundleFor("darwin", "")
}

func (p *Plugin) insertWGBundle(b *WGBundle) (*WGBundle, error) {
	if b == nil {
		return nil, errors.New("nil bundle")
	}
	if strings.TrimSpace(b.Version) == "" || strings.TrimSpace(b.BlobSHA256) == "" {
		return nil, errors.New("version and blob_sha256 required")
	}
	if strings.TrimSpace(b.OS) == "" {
		b.OS = "darwin"
	}
	err := p.DB.QueryRow(
		`INSERT INTO wg_bundles (version, os, arch, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id, added_at`,
		b.Version, b.OS, b.Arch, b.BlobURI, b.BlobSHA256, b.SizeBytes, b.IsLatest, b.Notes, time.Now().UTC(), b.AddedByUserID,
	).Scan(&b.ID, &b.AddedAt)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// setWGBundleLatest marks a bundle latest for ITS platform only — the previous
// latest for the same (os, arch) is cleared, leaving other platforms' latest
// untouched.
func (p *Plugin) setWGBundleLatest(id int64) error {
	tx, err := p.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var os, arch string
	if err := tx.QueryRow(`SELECT os, arch FROM wg_bundles WHERE id = $1`, id).Scan(&os, &arch); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE wg_bundles SET is_latest = FALSE WHERE is_latest = TRUE AND os = $1 AND arch = $2`, os, arch); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE wg_bundles SET is_latest = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (p *Plugin) deleteWGBundle(id int64) error {
	res, err := p.DB.Exec(`DELETE FROM wg_bundles WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---- heartbeats ----

type WGHeartbeatRecord struct {
	DeviceID         int64
	RXBytes          *int64
	TXBytes          *int64
	LastHandshakeSec *int64
	WGEndpoint       string
	LANAddrs         []WGLanAddr
	ReceivedAt       time.Time
}

func (p *Plugin) insertWGHeartbeat(rec *WGHeartbeatRecord, now time.Time) error {
	if rec == nil {
		return errors.New("nil heartbeat")
	}
	rec.ReceivedAt = now
	var lanJSON []byte
	if len(rec.LANAddrs) > 0 {
		lanJSON, _ = json.Marshal(rec.LANAddrs)
	}
	tx, err := p.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO wg_heartbeats (device_id, received_at, rx_bytes, tx_bytes, last_handshake_sec, wg_endpoint, lan_addrs_json)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rec.DeviceID, now,
		nullInt64(rec.RXBytes), nullInt64(rec.TXBytes), nullInt64(rec.LastHandshakeSec),
		rec.WGEndpoint, nullJSONB(lanJSON),
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE wg_devices
		    SET last_seen_at = $2,
		        wg_endpoint = CASE WHEN $3 = '' THEN wg_endpoint ELSE $3 END,
		        lan_addrs_json = COALESCE($4, lan_addrs_json)
		  WHERE id = $1`,
		rec.DeviceID, now, rec.WGEndpoint, nullJSONB(lanJSON),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullJSONB(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func newWGDeviceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "dev_" + hex.EncodeToString(b)
}

func validatePubkey(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return errors.New("pubkey required")
	}
	if len(p) > 128 {
		return fmt.Errorf("pubkey too long (%d > 128)", len(p))
	}
	return nil
}

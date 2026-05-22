package wg

// Phase 2 allocator for /v1/register. Role-aware:
//   - token.role = "hub": claim the hub slot (pubkey + bound_device_id)
//     on the token's pre-stamped wg_hubs row; device_ip = hub.wg_ip
//     (typically the .1 of mesh_cidr); site = "<hub.slug>:hub" at s_index=0.
//   - token.role = "device": allocate within token.hub_id's mesh_cidr;
//     site = auto by (publicIP, lanCIDR network) per-hub.
//
// One SERIALIZABLE transaction per register; retried once on PG 40001.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

type wgRegisterInput struct {
	Token        string
	Pubkey       string
	Hostname     string
	OS           string
	Arch         string
	AgentVer     string
	LANAddrs     []WGLanAddr
	WGListenPort int
	// SiteSlug overrides the auto-by-LAN site allocation. Empty = auto.
	SiteSlug string
	// PublicIP observed by the server (X-Forwarded-For chain or RemoteAddr).
	PublicIP string
}

type wgAllocateResult struct {
	Device         *WGDevice
	Hub            *WGHub
	Role           string // "hub" | "device"
	PlaintextToken string
	TokenExpiresAt *time.Time
}

var (
	errWGTokenInvalid       = errors.New("token invalid")
	errWGTokenAlreadyBound  = errors.New("token already bound to another device")
	errWGPubkeyTaken        = errors.New("pubkey already registered to a different device/token")
	errWGSiteExhausted      = errors.New("site exhausted (d_index range full); manual site sharding required")
	errWGSiteSlotsExhausted = errors.New("no free s_index in 1..254; hub mesh is full")
	errWGHubAlreadyBound    = errors.New("hub slot already claimed by another device; revoke its token + leave first")
	errWGHubNotConfigured   = errors.New("target hub has no live binding yet; ask operator to run the hub-token install first")
	errWGTokenNoHub         = errors.New("token has no hub_id; reissue")
)

func (p *Plugin) allocateWGDevice(ctx context.Context, in wgRegisterInput) (*wgAllocateResult, error) {
	if err := validatePubkey(in.Pubkey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Token) == "" {
		return nil, errWGTokenInvalid
	}
	if in.WGListenPort <= 0 || in.WGListenPort > 65535 {
		in.WGListenPort = 51820
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, err := p.doAllocate(ctx, in)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "could not serialize") {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}

func (p *Plugin) doAllocate(ctx context.Context, in wgRegisterInput) (*wgAllocateResult, error) {
	tx, err := p.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tok, err := p.lookupUnusedWGToken(tx, in.Token)
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, errWGTokenInvalid
	}
	if tok.HubID == nil {
		return nil, errWGTokenNoHub
	}
	hub, err := getWGHubByIDTx(tx, *tok.HubID)
	if err != nil {
		return nil, err
	}
	if hub == nil {
		return nil, errWGTokenNoHub
	}

	// Idempotent re-register (same pubkey + same token).
	pubkey := strings.TrimSpace(in.Pubkey)
	if existing, err := lookupDeviceByPubkeyTx(tx, pubkey); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.TokenHash == hashWGToken(in.Token) && existing.RemovedAt == nil {
			return p.refreshExistingDevice(tx, existing, in, tok, hub)
		}
		return nil, errWGPubkeyTaken
	}

	if tok.ConsumedAt != nil {
		return nil, errWGTokenAlreadyBound
	}

	switch tok.Role {
	case "hub":
		return p.bindHubDevice(tx, in, tok, hub)
	case "device":
		return p.allocateDevice(tx, in, tok, hub)
	default:
		return nil, fmt.Errorf("unknown token role %q", tok.Role)
	}
}

// refreshExistingDevice handles the idempotent re-register case:
// install.sh re-running against the same token + pubkey should not
// re-allocate; it just refreshes hostname/agent_ver/lan_addrs.
func (p *Plugin) refreshExistingDevice(tx *sql.Tx, existing *WGDevice, in wgRegisterInput, tok *WGToken, hub *WGHub) (*wgAllocateResult, error) {
	lanJSON, _ := json.Marshal(in.LANAddrs)
	if _, err := tx.Exec(
		`UPDATE wg_devices
		    SET hostname = $2, os = $3, arch = $4, agent_ver = $5,
		        lan_addrs_json = $6, wg_listen_port = $7
		  WHERE id = $1`,
		existing.ID,
		strings.TrimSpace(in.Hostname), strings.TrimSpace(in.OS), strings.TrimSpace(in.Arch),
		strings.TrimSpace(in.AgentVer), nullJSONB(lanJSON), in.WGListenPort,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	refreshed, err := p.getWGDeviceByID(existing.ID)
	if err != nil {
		return nil, err
	}
	role := "device"
	if hub.BoundDeviceID != nil && *hub.BoundDeviceID == existing.ID {
		role = "hub"
	}
	return &wgAllocateResult{
		Device:         refreshed,
		Hub:            hub,
		Role:           role,
		PlaintextToken: in.Token,
		TokenExpiresAt: tok.ExpiresAt,
	}, nil
}

// bindHubDevice claims the hub slot for the calling device. Inserts
// wg_devices at hub.wg_ip in a dedicated "<hub_slug>:hub" site
// (auto-created at s_index=0), then sets hub.pubkey + bound_device_id.
func (p *Plugin) bindHubDevice(tx *sql.Tx, in wgRegisterInput, tok *WGToken, hub *WGHub) (*wgAllocateResult, error) {
	if hub.BoundDeviceID != nil {
		return nil, errWGHubAlreadyBound
	}
	site, err := getOrCreateSiteTx(tx, hub.ID, hub.Slug+":hub", 0, "hub site", in.PublicIP, "")
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s:%d", strings.TrimSpace(in.PublicIP), in.WGListenPort)
	if strings.TrimSpace(in.PublicIP) == "" {
		endpoint = fmt.Sprintf("%s:%d", firstLANIP(firstLANCIDR(in.LANAddrs)), in.WGListenPort)
	}

	deviceID := newWGDeviceID()
	deviceIP := hub.WGIP
	if deviceIP == "" {
		ip, _ := hubWGIPFromCIDR(hub.MeshCIDR)
		deviceIP = ip
	}
	lanJSON, _ := json.Marshal(in.LANAddrs)
	tokenHash := hashWGToken(in.Token)
	now := time.Now().UTC()

	var newID int64
	err = tx.QueryRow(
		`INSERT INTO wg_devices (
		    device_id, hub_id, site_id, d_index, device_ip, pubkey, hostname, os, arch, agent_ver,
		    wg_listen_port, lan_addrs_json, wg_endpoint, token_hash, token_expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		 RETURNING id`,
		deviceID, hub.ID, site.ID, 1, deviceIP, strings.TrimSpace(in.Pubkey),
		strings.TrimSpace(in.Hostname), strings.TrimSpace(in.OS), strings.TrimSpace(in.Arch), strings.TrimSpace(in.AgentVer),
		in.WGListenPort, nullJSONB(lanJSON), endpoint, tokenHash, tok.ExpiresAt, now,
	).Scan(&newID)
	if err != nil {
		return nil, err
	}

	if err := updateWGHubBindTx(tx, hub.ID, in.Pubkey, endpoint, newID, now); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE wg_tokens SET consumed_at = $2, consumed_by_device_id = $3 WHERE id = $1`,
		tok.ID, now, newID,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	dev, err := p.getWGDeviceByID(newID)
	if err != nil {
		return nil, err
	}
	dev.HubSlug = hub.Slug
	dev.SiteSlug = site.Slug
	hub, _ = p.getWGHubByID(hub.ID)
	return &wgAllocateResult{
		Device:         dev,
		Hub:            hub,
		Role:           "hub",
		PlaintextToken: in.Token,
		TokenExpiresAt: tok.ExpiresAt,
	}, nil
}

// allocateDevice allocates a regular device IP within the target hub's
// mesh. Site is hashed by (publicIP, network-masked LAN CIDR) so same-
// LAN boxes bunch into one site.
func (p *Plugin) allocateDevice(tx *sql.Tx, in wgRegisterInput, tok *WGToken, hub *WGHub) (*wgAllocateResult, error) {
	if !hub.Configured() {
		return nil, errWGHubNotConfigured
	}
	mesh, err := parseMeshCIDR(hub.MeshCIDR)
	if err != nil {
		return nil, fmt.Errorf("hub mesh_cidr invalid: %w", err)
	}
	site, err := p.resolveOrCreateWGSite(tx, hub.ID, in, mesh)
	if err != nil {
		return nil, err
	}
	dIndex, err := pickFreeDIndex(tx, site.ID)
	if err != nil {
		return nil, err
	}
	deviceID := newWGDeviceID()
	deviceIP := mesh.deviceIP(dIndex)
	lanJSON, _ := json.Marshal(in.LANAddrs)
	tokenHash := hashWGToken(in.Token)
	now := time.Now().UTC()

	var newID int64
	err = tx.QueryRow(
		`INSERT INTO wg_devices (
		    device_id, hub_id, site_id, d_index, device_ip, pubkey, hostname, os, arch, agent_ver,
		    wg_listen_port, lan_addrs_json, token_hash, token_expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 RETURNING id`,
		deviceID, hub.ID, site.ID, dIndex, deviceIP, strings.TrimSpace(in.Pubkey),
		strings.TrimSpace(in.Hostname), strings.TrimSpace(in.OS), strings.TrimSpace(in.Arch), strings.TrimSpace(in.AgentVer),
		in.WGListenPort, nullJSONB(lanJSON), tokenHash, tok.ExpiresAt, now,
	).Scan(&newID)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE wg_tokens SET consumed_at = $2, consumed_by_device_id = $3 WHERE id = $1`,
		tok.ID, now, newID,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	dev, err := p.getWGDeviceByID(newID)
	if err != nil {
		return nil, err
	}
	dev.HubSlug = hub.Slug
	dev.SiteSlug = site.Slug
	return &wgAllocateResult{
		Device:         dev,
		Hub:            hub,
		Role:           "device",
		PlaintextToken: in.Token,
		TokenExpiresAt: tok.ExpiresAt,
	}, nil
}

// resolveOrCreateWGSite within a hub: explicit slug wins; otherwise
// hash by (publicIP, network-masked LAN CIDR).
func (p *Plugin) resolveOrCreateWGSite(tx *sql.Tx, hubID int64, in wgRegisterInput, mesh *meshNet) (*WGSite, error) {
	_ = mesh // reserved for future per-hub /16+ allocation
	if slug := strings.TrimSpace(in.SiteSlug); slug != "" {
		fullSlug := fmt.Sprintf("%s:%s", currentHubSlug(tx, hubID), slug)
		if site, err := lookupSiteBySlugInHub(tx, hubID, fullSlug); err != nil {
			return nil, err
		} else if site != nil {
			return site, nil
		}
		return createSite(tx, hubID, fullSlug, "", in.PublicIP, lanCIDRNetwork(firstLANCIDR(in.LANAddrs)))
	}
	pubIP := strings.TrimSpace(in.PublicIP)
	lan := lanCIDRNetwork(firstLANCIDR(in.LANAddrs))
	site, err := lookupSiteByAutoKeyInHub(tx, hubID, pubIP, lan)
	if err != nil {
		return nil, err
	}
	if site != nil {
		return site, nil
	}
	return createSite(tx, hubID, "", "", pubIP, lan)
}

func currentHubSlug(tx *sql.Tx, hubID int64) string {
	var slug string
	_ = tx.QueryRow(`SELECT slug FROM wg_hubs WHERE id = $1`, hubID).Scan(&slug)
	return slug
}

func lookupDeviceByPubkeyTx(tx *sql.Tx, pubkey string) (*WGDevice, error) {
	row := tx.QueryRow(`SELECT `+wgDeviceColumns+` FROM wg_devices WHERE pubkey = $1`, pubkey)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func lookupSiteBySlugInHub(tx *sql.Tx, hubID int64, slug string) (*WGSite, error) {
	var x WGSite
	err := tx.QueryRow(
		`SELECT id, COALESCE(hub_id, 0), slug, s_index, label, public_ip, lan_cidr, created_at
		   FROM wg_sites WHERE hub_id = $1 AND slug = $2`,
		hubID, slug,
	).Scan(&x.ID, &x.HubID, &x.Slug, &x.SIndex, &x.Label, &x.PublicIP, &x.LANCIDR, &x.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &x, nil
}

func lookupSiteByAutoKeyInHub(tx *sql.Tx, hubID int64, publicIP, lanCIDR string) (*WGSite, error) {
	var x WGSite
	err := tx.QueryRow(
		`SELECT id, COALESCE(hub_id, 0), slug, s_index, label, public_ip, lan_cidr, created_at
		   FROM wg_sites
		  WHERE hub_id = $1 AND public_ip = $2 AND lan_cidr = $3
		  ORDER BY id LIMIT 1`,
		hubID, publicIP, lanCIDR,
	).Scan(&x.ID, &x.HubID, &x.Slug, &x.SIndex, &x.Label, &x.PublicIP, &x.LANCIDR, &x.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &x, nil
}

// getOrCreateSiteTx is used by bindHubDevice: idempotent — if the
// hub-site for this hub already exists (re-bind after admin reset),
// return it; else create it.
func getOrCreateSiteTx(tx *sql.Tx, hubID int64, slug string, sIndex int, label, publicIP, lanCIDR string) (*WGSite, error) {
	site, err := lookupSiteBySlugInHub(tx, hubID, slug)
	if err != nil {
		return nil, err
	}
	if site != nil {
		return site, nil
	}
	var newID int64
	var createdAt time.Time
	err = tx.QueryRow(
		`INSERT INTO wg_sites (slug, s_index, label, public_ip, lan_cidr, hub_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		slug, sIndex, label, publicIP, lanCIDR, hubID,
	).Scan(&newID, &createdAt)
	if err != nil {
		return nil, err
	}
	return &WGSite{
		ID:        newID,
		HubID:     hubID,
		Slug:      slug,
		SIndex:    sIndex,
		Label:     label,
		PublicIP:  publicIP,
		LANCIDR:   lanCIDR,
		CreatedAt: createdAt,
	}, nil
}

func createSite(tx *sql.Tx, hubID int64, slug, label, publicIP, lanCIDR string) (*WGSite, error) {
	sIdx, err := pickFreeSIndexInHub(tx, hubID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(slug) == "" {
		slug = fmt.Sprintf("%s:site_%d", currentHubSlug(tx, hubID), sIdx)
	}
	var newID int64
	var createdAt time.Time
	err = tx.QueryRow(
		`INSERT INTO wg_sites (slug, s_index, label, public_ip, lan_cidr, hub_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		slug, sIdx, label, publicIP, lanCIDR, hubID,
	).Scan(&newID, &createdAt)
	if err != nil {
		return nil, err
	}
	return &WGSite{
		ID:        newID,
		HubID:     hubID,
		Slug:      slug,
		SIndex:    sIdx,
		Label:     label,
		PublicIP:  publicIP,
		LANCIDR:   lanCIDR,
		CreatedAt: createdAt,
	}, nil
}

// pickFreeSIndexInHub: lowest unused s_index in [1,254] within the
// given hub. s_index=0 is reserved for "<hub>:hub" site.
func pickFreeSIndexInHub(tx *sql.Tx, hubID int64) (int, error) {
	rows, err := tx.Query(`SELECT s_index FROM wg_sites WHERE hub_id = $1 ORDER BY s_index`, hubID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := make(map[int]bool, 64)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		used[v] = true
	}
	for v := 1; v <= 254; v++ {
		if !used[v] {
			return v, nil
		}
	}
	return 0, errWGSiteSlotsExhausted
}

func pickFreeDIndex(tx *sql.Tx, siteID int64) (int, error) {
	rows, err := tx.Query(
		`SELECT d_index FROM wg_devices WHERE site_id = $1 AND removed_at IS NULL ORDER BY d_index`,
		siteID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := make(map[int]bool, 64)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		used[v] = true
	}
	for v := 2; v <= 254; v++ {
		if !used[v] {
			return v, nil
		}
	}
	return 0, errWGSiteExhausted
}

// firstLANCIDR returns the device's reported CIDR for its first
// network interface. Pipe through lanCIDRNetwork() for the network
// address used as the site auto-allocation key.
func firstLANCIDR(addrs []WGLanAddr) string {
	for _, a := range addrs {
		if strings.TrimSpace(a.CIDR) != "" {
			return strings.TrimSpace(a.CIDR)
		}
	}
	return ""
}

// lanCIDRNetwork normalizes "192.168.11.42/24" → "192.168.11.0/24"
// so that two boxes on the same LAN deterministically hash into the
// same site. Returns "" on unparseable input.
func lanCIDRNetwork(cidr string) string {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return ""
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	return ipnet.String()
}

type meshNet struct {
	ipnet *net.IPNet
}

func parseMeshCIDR(cidr string) (*meshNet, error) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return nil, errors.New("mesh_cidr required")
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	if ipnet.IP.To4() == nil {
		return nil, errors.New("mesh_cidr must be IPv4")
	}
	return &meshNet{ipnet: ipnet}, nil
}

// deviceIP returns the network address + the given last octet. For
// "10.88.0.0/24" + d=5 → "10.88.0.5". /16 with sub-/24 partitioning
// is a future PR (when needed). For /24, this gives 1..254 devices.
func (m *meshNet) deviceIP(d int) string {
	base := m.ipnet.IP.To4()
	if base == nil {
		return ""
	}
	out := make(net.IP, 4)
	copy(out, base)
	out[3] = byte(d)
	return out.String()
}

func extractPublicIP(xff, xrealip, remoteAddr string) string {
	if v := strings.TrimSpace(xff); v != "" {
		if comma := strings.Index(v, ","); comma >= 0 {
			v = v[:comma]
		}
		return strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(xrealip); v != "" {
		return v
	}
	if v := strings.TrimSpace(remoteAddr); v != "" {
		if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[i+1:], ":") {
			return v[:i]
		}
		return v
	}
	return ""
}

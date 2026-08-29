# IDC WG hub deploy — on-site runbook (2026-06-10)

Target: bring hub **id=4 `124.221.22.9`** (mesh 100.64.0.1, token label "idc") into
the mesh + connect spoke(s). The `s_index` 500 is already fixed in prod; the
`join-linux.sh` here has the hub-aware refresh + ip_forward fix (polar-wg-app PR #21).

Hub token:  `<HUB_TOKEN>`  (role=hub, unconsumed+live)
Server:     `https://wg.4950.store:2443`
Admin UI:   `https://<dock>/wg-tokens.html`   (mint device tokens, see endpoints)

---

## 0. Pre-flight (on the IDC box)
- [ ] Cloud **security group: inbound UDP 51820** open.
- [ ] Deps: `sudo apt install -y wireguard-tools curl python3 iproute2`
- [ ] Copy `join-linux.sh` to the box:  `scp join-linux.sh root@124.221.22.9:/root/`

## 1. Join the hub (Option 2 — primary)
```bash
sudo bash join-linux.sh \
  --token=<HUB_TOKEN> \
  --server=https://wg.4950.store:2443 \
  --iface=wgc0
```
Expected: `✓ joined mesh (linux/amd64)  device_ip: 100.64.0.1  iface: wgc0`.
It writes /etc/wireguard/wgc0.conf, `systemctl enable --now wg-quick@wgc0`,
installs `wgctl-refresh@wgc0.timer` (hub-aware → /v1/hub/peers), enables ip_forward.

## 2. Verify hub
```bash
sudo wg show wgc0                       # iface up, listen 51820, peers listed
sudo /usr/local/sbin/wgctl-refresh-linux wgc0   # force a refresh; no error
sysctl net.ipv4.ip_forward              # = 1
systemctl status wgctl-refresh@wgc0.timer       # active
```
In admin /wg-tokens.html: hub id=4 bound, **endpoint = 124.221.22.9:51820**.
If endpoint blank/wrong → PATCH it via admin (spokes need it to dial in).

## 3. Bring up spoke(s)
- Mint a **device-role** token per spoke in /wg-tokens.html (role=device, hub=124.221.22.9).
- On each spoke:
  - Linux: `sudo bash join-linux.sh --token=<DEV_TOKEN> --server=https://wg.4950.store:2443`
  - macOS: `sudo bash join.sh --token=<DEV_TOKEN> --server=https://wg.4950.store:2443`
- Test both directions:
  - from spoke: `ping 100.64.0.1`        (the hub)
  - from hub:   `ping <spoke wg_ip>`      (keepalive=25 lets the hub learn NAT'd spokes)

## 4. Fallback — manual peer add (Option 1, if refresh stalls)
Read each peer's `pubkey / wg_ip / endpoint` from admin, then on the hub:
```bash
sudo wg set wgc0 peer <PUBKEY> allowed-ips <WG_IP>/32        # [endpoint <ip:port>]
sudo wg-quick save wgc0       # persist to conf
```

## 5. Fleet agent — polar-agent (Option 3, optional, non-WG-owning)
Install for shell/VNC/host-info + host↔wg_pubkey crosslink. It must NOT manage wgc0.
```bash
# install binary, then:
polar-agent register --token=<ENROLL_TOKEN_from_/hosts.html> --server=https://<dock>
# sudoers (read wg for crosslink): /etc/sudoers.d/polar-wg
#   <user> ALL=(root) NOPASSWD: /usr/bin/wg, /usr/bin/wg-quick
export POLAR_AGENT_WIREGUARD_DISABLED=true     # so it never fights wgc0
# wrap `polar-agent attach` in a systemd unit
```

## Leave / rollback (if needed)
```bash
sudo wg-quick down wgc0
sudo systemctl disable --now wg-quick@wgc0 wgctl-refresh@wgc0.timer
curl -fsSL -X POST https://wg.4950.store:2443/v1/leave -H 'Content-Type: application/json' \
  -d '{"device_id":"<from /etc/wgctl/wgc0.json>","token":"<hub token>"}'
sudo rm -f /etc/wireguard/wgc0.conf /etc/wgctl/wgc0.json
```

## Gotchas
- Use THIS `join-linux.sh` (hub-aware), **not** the box's old `join-linux2.sh` (no refresh timer).
- Hub→spoke only works if the hub has a reachable public endpoint (it does: 124.221.22.9) and spokes keepalive. Two-NAT pairs won't hairpin (separate issue).
- A successful register **consumes the hub token** — only run step 1 once.

## Acceptance
1. `wg show wgc0` on hub lists every spoke; each spoke lists the hub.
2. Bidirectional ping across mesh IPs.
3. `wgctl-refresh@wgc0.timer` active; force-run hits /v1/hub/peers.
4. Reboot survives: wg-quick@wgc0 + timer enabled, ip_forward persisted.

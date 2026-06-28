# Puck provisioning (Ansible)

The quickstart (control-node setup, inventory and secrets, running a play) is in
the [repo README](../README.md). This covers configuration and what each role
does.

No secrets or per-host data live in the repo. The admin SSH key
(`../keys/puck-admin`) is gitignored, and the Tailscale auth key and `pi`
password come from `secrets.yml` (also gitignored). Hostname derives from the
inventory name, so there are no `host_vars` files.

## Configuration

### Inventory

`inventory/hosts.yml` lists your pucks under the `pucks` group. The inventory
name becomes the box's hostname and Tailscale identity, so use stable, DNS-safe
names.

Optional roles are off by default. Enable one for a single puck with a host var:

```yaml
pucks:
  hosts:
    puck-home:
      pihole_enabled: true     # this box runs Pi-hole; others don't
    puck-parents:
```

Host vars outrank role defaults, so only that puck enables it. For a one-off run,
`-e pihole_enabled=true` does the same thing.

### Secrets

`group_vars/all/secrets.yml` is auto-loaded (no CLI flags, no prompts):

| Var | Required | Purpose |
|---|---|---|
| `pi_password` | yes | `pi` account password (serial/break-glass login). The run aborts if unset. |
| `ts_authkey` | no | Tailscale enrollment key. Empty leaves Tailscale untouched. |
| `pihole_admin_password` | no | Pi-hole admin UI password. Blank lets Pi-hole generate one (`docker logs pihole`). |

Everything in `group_vars/all/` is merged automatically. `-e var=...` overrides
any value for a single run.

### Variable precedence

role defaults < `group_vars/all/` < inventory host vars < `-e` on the command line.

## First contact and credentials

The play detects how to log in: the admin key if the box is already provisioned,
otherwise the factory `pi` password for a fresh or re-flashed board. A fresh
board isn't on the tailnet yet, so reach it by bench IP and enroll one at a time:

```bash
ansible-playbook site.yml -l puck-home -e ansible_host=NanoPi-Zero2
```

`pi_password`'s hash is deterministic per host, so re-runs don't churn the
password. `ts_authkey` only acts on first enrollment; later runs leave Tailscale
alone (use `tailscale set` for changes).

## Roles

Applied in this order:

1. **provision**. Bootstrap `python3`, install the admin key, passwordless sudo,
   set the `pi` password, hostname, IP-forwarding sysctls, base sshd hardening
   (root login off).
2. **watchdog**. Arm the systemd hardware watchdog (auto-reboot on hang).
3. **unattended-upgrades**. Automatic security patching.
4. **tailscale**. Skipped without `ts_authkey`. Otherwise `tailscale up` once
   with `--ssh --advertise-exit-node`.
5. **docker**. Docker Engine and the Compose plugin (official apt repo).
6. **puckview**. The Wake-on-LAN and diagnostics dashboard, a bare-metal systemd
   service. Binds loopback:8091 and is served at `http://<hostname>` (`:80`) over
   the tailnet (serve is skipped until the node is enrolled). It auto-derives the
   LAN interface and CIDR from the kernel and renders a service catalogue
   (`puckview_services`) linking every enabled service. Binary source and the
   local-build override are documented in
   [puckview/README.md](../puckview/README.md).
7. **pihole** (optional). LAN-local ad-blocking DNS in a host-networked
   container. Answers DNS on `:53` for the puck's local subnet
   (`FTLCONF_dns_listeningMode=LOCAL`); point LAN devices at the puck's LAN IP to
   use it. This is not the tailnet resolver. The admin UI is at
   `http://<hostname>:8080/admin` (its own port). The role frees `:53` from
   `systemd-resolved`'s stub listener if present.
8. **lockdown**. Runs an out-of-band publickey-only SSH probe and disables SSH
   password auth only if it succeeds. A failed probe leaves password auth on, so
   there's no lockout. `pi` keeps its password for serial-console recovery;
   `PasswordAuthentication no` governs SSH, not the console getty.

## Recovery and safety model

- Primary lifeline: Tailscale SSH. Secondary: key-only SSH over the LAN.
  Break-glass: SD rescue boot, mount the eMMC rootfs (`/dev/mmcblk2p8`), and
  repair offline (no password, network, or login needed). Serial console
  (1500000 8N1) is a further fallback with a UART adaptor.
- Nothing disables the password path until an independent key login is verified,
  and sshd configs are written with `validate: sshd -t` (a malformed config is
  refused before reload), so a failed run leaves the box reachable.

## Gotchas

- **Updating a container.** The `pihole` image is pinned to an exact tag
  (`pihole_version`) with `pull: missing`, so each box stays on what it first
  pulled. Bump the version var and re-run to update.
- **Reusable auth key.** Tag it so nodes don't expire when you revoke it, and
  revoke it once all units are enrolled.
- **Re-flashing a box.** It keeps its name but gets a new SSH host key, so SSH
  refuses to connect. Clear the stale entry first:
  ```bash
  ssh-keygen -R puck-home     # and -R NanoPi-Zero2 for a fresh first-contact
  ```

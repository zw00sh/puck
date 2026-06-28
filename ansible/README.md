# Puck provisioning (Ansible)

Provisions "pucks" (NanoPi Zero2 boards): stock Debian Core → hardened,
key-only-SSH, Tailscale exit node. No image surgery; all config is applied at
runtime and is idempotent (safe to re-run).

No secrets and no per-host data live in this repo. The private SSH key
(`../keys/puck-admin`) is gitignored; the Tailscale auth key and the `pi`
password are supplied at invocation time. Hostname derives from the inventory
name, so there are no `host_vars`.

## Control node (one-time)

```bash
ansible-galaxy collection install ansible.posix      # authorized_key, sysctl
ansible-galaxy collection install community.docker   # docker_compose_v2 (pihole)
sudo apt-get install -y python3-passlib              # password_hash (py3.13 dropped crypt)
```

Confirm `keys/puck-admin` (private) is present, `chmod 600`, matching
`keys/puck-admin.pub`. `admin_private_key` in `group_vars/all/main.yml` already
points at it. If you don't have one yet, generate the dedicated admin keypair:

```bash
ssh-keygen -t ed25519 -f ../keys/puck-admin -N "" -C puck-admin && chmod 600 ../keys/puck-admin
```

### Inventory (one-time)

Your fleet roster lives in `inventory/hosts.yml`, which is gitignored — copy the
committed template and put your own puck names in it (the name becomes the box's
hostname + Tailscale identity, so pick stable, DNS-safe names):

```bash
cp inventory/hosts.yml.example inventory/hosts.yml
$EDITOR inventory/hosts.yml     # one line per puck, under the `pucks` group
```

**Optional roles per host.** Roles like `pihole` are off by default; turn one on
for a specific puck by setting its flag as a host variable in `hosts.yml` (the file
is gitignored, so per-host config stays local):

```yaml
pucks:
  hosts:
    puck-home:
      pihole_enabled: true     # this box runs Pi-hole; others don't
    puck-parents:
```

Inventory host vars outrank the role default, so only that puck enables it. The
equivalent one-off is `-e pihole_enabled=true` on a single run.

### Secrets (one-time)

Sensitive/required values are auto-loaded from a gitignored file — no CLI flags,
no prompts. Copy the template and fill it in once:

```bash
cp group_vars/all/secrets.yml.example group_vars/all/secrets.yml
$EDITOR group_vars/all/secrets.yml     # set pi_password (required), ts_authkey, …
```

`secrets.yml` holds `pi_password` (**required**), `ts_authkey`, and
`pihole_admin_password`. It is gitignored; the committed
`.example` documents the shape. Everything in `group_vars/all/` is merged
automatically, and `-e var=...` still overrides any value for an ad-hoc run.

## Provision a box

With `secrets.yml` filled in, the routine flow needs only the target name (plus
the bench IP on a fresh box's first contact).

First contact + enroll (fresh box on the bench LAN, ONE at a time):

```bash
ansible-playbook site.yml -l puck-home -e ansible_host=NanoPi-Zero2
```

Re-run over Tailscale by name:

```bash
ansible-playbook site.yml -l puck-home
```

- `ts_authkey` lives in `secrets.yml`; empty leaves Tailscale untouched (day-2
  re-runs don't disturb enrollment). Revoke the reusable key once all units are in.
- `pi_password` is **required** — the run aborts up front if it's unset. Its hash
  is deterministic per host, so re-runs are idempotent (no password churn).

## What it does, in order

1. **Detect creds** — key if the box is already provisioned, else the factory
   `pi` password for a fresh/re-flashed box.
2. **provision** — bootstrap `python3`, install the dedicated key, passwordless
   sudo, set the `pi` password (required, from `secrets.yml`; serial/break-glass
   login), hostname, IP-forwarding sysctls, base sshd hardening (root off).
3. **watchdog** — arm the systemd hardware watchdog (auto-reboot if the box hangs).
4. **unattended-upgrades** — install + enable automatic security patching.
5. **tailscale** — skipped unless `ts_authkey` is supplied; otherwise install +
   `tailscale up` once with `--ssh --advertise-exit-node` (later changes use
   `tailscale set`, per doctrine).
6. **docker** — install Docker Engine + Compose plugin (official apt repo). Also
   the base for future containers on these boxes.
7. **puckview** — the Wake-on-LAN + LAN-presence + box-diagnostics dashboard, a
   bare-metal systemd service (no container, no Go toolchain on the puck — the
   pinned release binary is downloaded). It binds to **loopback:8091** so it's
   never on the LAN, and `tailscale serve` exposes it at the bare
   `http://<hostname>` (`:80`) over the tailnet (WireGuard-encrypted). Serve is
   skipped until the node is enrolled. It auto-derives the LAN interface/CIDR from
   the kernel and renders a service catalogue (`puckview_services`) so every
   enabled service is one click away.
8. **pihole** *(optional, off by default)* — **LAN-local** ad-blocking DNS
   resolver as a `host`-networked container. Enable it per host with
   `pihole_enabled: true` (see *Inventory* above), or one-off with
   `-e pihole_enabled=true`.
   DNS answers on `:53` for the puck's local subnet(s) (`FTLCONF_dns_listeningMode=LOCAL`)
   — point LAN devices at the puck's LAN IP to use it. This is **not** the tailnet
   resolver. The admin UI binds to **loopback:8080** and is served on the tailnet at
   `http://<hostname>:8080/admin` (its own port). The role frees host `:53` from
   `systemd-resolved`'s stub listener if present. Set the admin password via
   `pihole_admin_password` (in `secrets.yml`; blank => Pi-hole self-generates one,
   read `docker logs pihole`).
9. **lockdown** — an out-of-band publickey-only SSH probe; only if it succeeds
   is SSH password auth disabled. Otherwise the run fails with password auth
   left ON — no lockout. `pi` keeps its password for serial-console recovery;
   SSH itself stays key-only (`PasswordAuthentication no` governs SSH, not the
   console getty).

## Recovery / safety model

- **Primary lifeline:** Tailscale SSH. **Secondary:** key-only SSH over the LAN.
  **Break-glass:** SD rescue boot → mount the eMMC rootfs (`/dev/mmcblk2p8`) and
  repair offline — needs no password, no network, no working login. (Serial
  console, 1500000 8N1, is a further fallback if you have a UART adaptor.)
- Nothing disables the password path until an independent key login is verified,
  and sshd configs are written with `validate: sshd -t` (a malformed config is
  refused before reload), so a failed run leaves the box reachable.

## Gotchas

- **Updating a container:** the `pihole` image is pinned to an exact tag
  (`pihole_version`) and `pull: missing` freezes each box at what it first pulled.
  To update, bump the version var and re-run — the new tag is absent, so it pulls.
- **Reusable auth key:** revoke it in the admin console once all units are
  enrolled. Make it tagged so the nodes don't expire after you revoke it.
- **Re-flashing a box:** it keeps its name but gets a NEW SSH host key, so SSH
  refuses to connect ("host key changed"). Clear the stale entry first:
  ```bash
  ssh-keygen -R puck-home     # and -R NanoPi-Zero2 for a fresh first-contact
  ```

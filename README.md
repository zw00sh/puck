# puck

Provisions a stock **NanoPi Zero2** into a hardened, tailnet-only mini-server (a
"puck") with one Ansible run. No image surgery: stock Debian Core in; key-only
SSH, a Tailscale exit node, and a Wake-on-LAN/diagnostics dashboard out. Every
step is idempotent, so re-running is safe.

The main app is **[puckview](puckview/)**: a single static Go binary for
Wake-on-LAN with diagnostics — multi-signal presence (ARP / ICMP / TCP), LAN
discovery, and box metrics, served over the tailnet.

## Provision a puck

### 1. Control node (one-time)

```bash
ansible-galaxy collection install ansible.posix      # authorized_key, sysctl
ansible-galaxy collection install community.docker   # docker_compose_v2 (pihole)
sudo apt-get install -y python3-passlib              # password_hash (py3.13 dropped crypt)
```

Generate the dedicated admin keypair if you don't have one:

```bash
ssh-keygen -t ed25519 -f keys/puck-admin -N "" -C puck-admin && chmod 600 keys/puck-admin
```

### 2. Inventory + secrets (one-time)

Both files are gitignored — copy the committed templates and fill them in:

```bash
cd ansible
cp inventory/hosts.yml.example inventory/hosts.yml   # one line per puck; name = hostname + tailnet identity
cp group_vars/all/secrets.yml.example group_vars/all/secrets.yml
$EDITOR group_vars/all/secrets.yml                   # pi_password (required), ts_authkey, …
```

### 3. Run it

```bash
# Fresh board on the bench LAN (one at a time), first contact + enroll (can use IP in place of hostname):
ansible-playbook site.yml -l puck-home -e ansible_host=NanoPi-Zero2

# Day-2: re-run over the tailnet, by name:
ansible-playbook site.yml -l puck-home
```

`pi_password` is required (the run aborts if unset). `ts_authkey` enrolls the
node on first run; leave it empty afterwards and Tailscale is left untouched.

See [ansible/README.md](ansible/README.md) for the full reference, optional
roles, and the recovery model.

## How it works

`site.yml` applies these roles in order, each idempotent:

| Role | Does |
|---|---|
| **provision** | Bootstrap python, install the admin key, passwordless sudo, hostname, sshd hardening (root off) |
| **watchdog** | Arm the hardware watchdog (auto-reboot on hang) |
| **unattended-upgrades** | Automatic security patching |
| **tailscale** | `tailscale up --ssh --advertise-exit-node` (only if `ts_authkey` given) |
| **docker** | Docker Engine + Compose plugin |
| **puckview** | The WoL + presence + diagnostics dashboard (bare-metal systemd service) |
| **pihole** *(opt-in)* | LAN-local ad-blocking DNS resolver |
| **lockdown** | Disable SSH password auth, but only after an independent key login is verified |

Services bind loopback and are exposed at the bare hostname via `tailscale serve`
(WireGuard-encrypted), not on the LAN. The tailnet is the security boundary;
there is no app-level auth. Nothing disables the password login path until a key
login is independently verified, so a failed run leaves a box reachable.

## Layout

```
ansible/      provisioning (roles, inventory, site.yml)   → ansible/README.md
puckview/     the Go dashboard app                        → puckview/README.md
keys/         admin SSH keypair (gitignored)
```

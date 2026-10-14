# puckview

A Wake-on-LAN, LAN-presence, and box-diagnostics dashboard for pucks (NanoPi
Zero2 mini-servers). A single static Go binary, served over the tailnet.

For provisioning see the [repo README](../README.md). This file covers building,
deploying, and developing the Go app.

## What it does

- **Wake-on-LAN** with diagnostics: sends an L2 frame (`0x0842`),
  subnet/limited broadcast, and unicast on UDP 9/7, with the broadcast address
  derived from the interface (no manual netmask). After sending, it streams
  ARP/ICMP/TCP state until the host responds.
- **Presence** via the kernel neighbour cache (ARP), plus active ICMP and TCP
  probes. Liveness is multi-signal because ICMP alone is unreliable.
- **Discovery**: the passive ARP cache plus an active, size-capped ARP sweep of
  the local subnet. Discovered hosts can be promoted to tracked devices.
- **Naming**: rDNS (against the LAN resolver, not MagicDNS), NetBIOS, OUI vendor.
- **Diagnostics**: CPU/per-core/load/temp, mem/swap/disk, network sparklines,
  tailnet state, and a service catalogue with health checks.
- **Activity-gated**: active probing runs only while someone is actually looking.
  The gate is a proof-of-life pulse from the page (`POST /api/pulse` every 10s
  while the tab is visible), not an open socket — a `tailscale serve` proxy or a
  suspended phone holds a socket open indefinitely. Passive reads continue
  regardless.

## Architecture

Pure Go (`CGO_ENABLED=0`), so it cross-compiles to a single static
`linux/arm64` file. SQLite via `modernc.org/sqlite`; netlink and raw sockets via
`mdlayher/*`; the web UI is `go:embed`-ed (no JS build step). Linux-specific
code (`/proc`, `/sys`, netlink neigh, AF_PACKET) is build-tagged with macOS dev
stubs so `make run` works on a Mac.

```
cmd/puckview        entrypoint
internal/server     HTTP + SSE (activity gate) + REST API
                    state.go  — in-memory presence model (the read path)
                    frames.go — pre-marshalled SSE frame set (O(1) connect)
internal/store      SQLite device store (MAC-keyed); the journal, not the read path
internal/neigh      kernel neighbour cache (netlink / arp)
internal/scan       active ARP sweep
internal/wol        magic-packet sender
internal/probe      TCP + ICMP
internal/naming     rDNS / NetBIOS / OUI
internal/oui        embedded IEEE OUI table
internal/sysinfo    box diagnostics
internal/tailscale  tailnet status (CLI)
internal/catalogue  provisioned service list
web/                embedded dashboard (HTML/CSS/JS)
```

### Presence model

Presence is three derived scalars per device — `icmp_seen`, `last_seen_up`,
`last_seen` — stored as columns on `devices`. They are owned in memory and
flushed back to SQLite every 30s (`store.Flush`, one transaction); `seen_events`
keeps a **transition-only** history, a row when a device changes up/down state.

Nothing on the per-second render path touches the database. That matters: the
v0 schema instead kept an append-only `seen_history` sample log, wrote a row per
probe cycle per device, and answered "has this MAC ever replied to ICMP" with
`SELECT DISTINCT mac ... WHERE signal='icmp'` once per second. On a three-month-
old puck that was a 1.6M-row / 119MB table, a 3.3s scan per frame, a core pegged
around the clock, and 26s to first byte on `/events`.

Opening a v0 database migrates it automatically: the schema change is O(1) and
runs before the listener, while the one-off backfill runs in the background
(proving a device never answered ICMP still costs a scan of its rows). It then
drops the log and `VACUUM`s — 119MB to 33KB on the box this was found on.

The loops are independent, each with its own cadence and gate — render (1s,
ungated), probe (2s, pulse-gated), names (4s, pulse-gated), meta (5s, gated on
attached clients), maintenance (30s flush + hourly prune, ungated).

## Develop

```sh
make run     # http://127.0.0.1:8091  (throwaway DB in /tmp)
make test
make arm64   # static linux/arm64 binary -> dist/puckview-linux-arm64
```

`make arm64` runs `scripts/update-oui.sh` to regenerate the embedded IEEE OUI
vendor table; it caches the registry and falls back to the committed table when
offline. Refresh it on its own with `make oui` (`--force` to bypass the cache).

## Configuration (env)

| Var | Default | Purpose |
|---|---|---|
| `PUCKVIEW_LISTEN` | `127.0.0.1:8091` | HTTP listen address |
| `PUCKVIEW_DB` | `/var/lib/puckview/puckview.db` | SQLite path |
| `PUCKVIEW_IFACE` | auto | LAN interface override |
| `PUCKVIEW_GRACE` | `30s` | probe-stop grace after the dashboard's last pulse |
| `PUCKVIEW_CATALOGUE` | `/opt/puckview/catalogue.json` | service catalogue |
| `PUCKVIEW_LAN_RESOLVER` | gateway | rDNS resolver |

## CI and releases

`.github/workflows/ci.yml` vets, tests, and cross-compiles the puck target on
every push and PR (from the committed `oui.txt`, no network). Pushing a
`puckview-v<version>` tag runs `release.yml`, which cross-compiles
arm64/amd64/darwin, writes `.sha256` sidecars, and publishes them as a GitHub
release.

## Deploy

The Ansible `puckview` role downloads the pinned release binary by default
(`puckview_version` selects the tag), so the puck needs no Go toolchain. For dev
iteration or an unreleased build, build locally and have the role copy that
instead:

```sh
make arm64                       # -> dist/puckview-linux-arm64
cd ../ansible && ansible-playbook site.yml -l <host> --tags puckview \
  -e puckview_use_local_build=true
```

Set `puckview_sha256` to pin the download's checksum. The role binds the binary
to loopback and exposes it over the tailnet; see
[ansible/README.md](../ansible/README.md).

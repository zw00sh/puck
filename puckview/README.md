# puckview

A Wake-on-LAN, LAN-presence, and box-diagnostics dashboard for pucks (NanoPi
Zero2 mini-servers). A single static Go binary, served over the tailnet.

For provisioning and deployment see the [repo README](../README.md). This file
covers building and developing the Go app.

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
- **Activity-gated**: active probing runs only while a dashboard client is
  connected (the SSE connection is the gate); passive reads continue regardless.

## Architecture

Pure Go (`CGO_ENABLED=0`), so it cross-compiles to a single static
`linux/arm64` file. SQLite via `modernc.org/sqlite`; netlink and raw sockets via
`mdlayher/*`; the web UI is `go:embed`-ed (no JS build step). Linux-specific
code (`/proc`, `/sys`, netlink neigh, AF_PACKET) is build-tagged with macOS dev
stubs so `make run` works on a Mac.

```
cmd/puckview        entrypoint
internal/server     HTTP + SSE (activity gate) + REST API
internal/store      SQLite device store (MAC-keyed)
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
| `PUCKVIEW_GRACE` | `15s` | probe-stop grace after the last client leaves |
| `PUCKVIEW_CATALOGUE` | `/opt/puckview/catalogue.json` | service catalogue |
| `PUCKVIEW_LAN_RESOLVER` | gateway | rDNS resolver |

## CI / releases

`.github/workflows/ci.yml` vets, tests, and cross-compiles the puck target on
every push and PR (from the committed `oui.txt`, no network). Pushing a
`puckview-v<version>` tag runs `release.yml`, which cross-compiles
arm64/amd64/darwin, writes `.sha256` sidecars, and publishes them as a GitHub
release — the binaries the Ansible role downloads by default.

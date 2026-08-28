#!/bin/sh
# Managed by Ansible. Cause-agnostic backstop for Pi-hole DNS.
#
# BIND (dnsmasq bind-interfaces) binds :53 to eth0's *address* and is evaluated
# once at FTL startup. If eth0 has no IPv4 yet at that moment, FTL logs
# `unknown interface eth0`, falls back to loopback only, and never rebinds — the
# socket is open but nothing answers, so the container's own healthcheck (a dig
# to 127.0.0.1) fails and Docker marks it `unhealthy`. `restart: unless-stopped`
# never fires because FTL doesn't exit. This restarts it so the next start (with
# eth0 up) binds cleanly. Also covers any other FTL/DNS wedge, not just boot.
#
# Only acts on a definitive "unhealthy" so it never fights the transient
# "starting" state during a normal boot or gravity update.
set -eu

status=$(docker inspect -f '{{ .State.Health.Status }}' pihole 2>/dev/null || echo missing)

if [ "$status" = "unhealthy" ]; then
  logger -t pihole-healthcheck "pihole container unhealthy (status=$status) — restarting"
  docker restart pihole >/dev/null 2>&1 || logger -t pihole-healthcheck "docker restart pihole failed"
fi

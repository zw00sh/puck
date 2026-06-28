// Package config loads puckview's runtime configuration from the environment.
// Every knob has a sane default so the binary runs with zero configuration; the
// Ansible role writes an EnvironmentFile to override them.
package config

import (
	"os"
	"time"
)

type Config struct {
	Listen      string        // PUCKVIEW_LISTEN   — HTTP listen address
	DBPath      string        // PUCKVIEW_DB       — SQLite file
	Iface       string        // PUCKVIEW_IFACE    — LAN interface ("" = auto-detect)
	Grace       time.Duration // PUCKVIEW_GRACE    — probe-stop grace after last client leaves
	Catalogue   string        // PUCKVIEW_CATALOGUE — service catalogue JSON path
	LANResolver string        // PUCKVIEW_LAN_RESOLVER — rDNS target ("" = default gateway)
}

func Load() Config {
	return Config{
		Listen:      env("PUCKVIEW_LISTEN", "127.0.0.1:8091"),
		DBPath:      env("PUCKVIEW_DB", "/var/lib/puckview/puckview.db"),
		Iface:       env("PUCKVIEW_IFACE", ""),
		Grace:       envDuration("PUCKVIEW_GRACE", 15*time.Second),
		Catalogue:   env("PUCKVIEW_CATALOGUE", "/opt/puckview/catalogue.json"),
		LANResolver: env("PUCKVIEW_LAN_RESOLVER", ""),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

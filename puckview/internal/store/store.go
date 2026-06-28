// Package store is puckview's device database: the dynamic, app-owned record of
// tracked devices, their TCP watchdogs, and last-seen history. Keyed by MAC (the
// durable identity); IP/name/vendor are re-learnable attributes. Uses the
// pure-Go modernc.org/sqlite driver so the binary stays static (no cgo).
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Device struct {
	MAC        string `json:"mac"`
	IP         string `json:"ip"`
	Name       string `json:"name"`
	NameSource string `json:"name_source"`
	Vendor     string `json:"vendor"`
	Link       string `json:"link"`
	Notes      string `json:"notes"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	Probes     []int  `json:"-"` // TCP watchdog ports
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS devices(
  mac         TEXT PRIMARY KEY,
  ip          TEXT NOT NULL DEFAULT '',
  name        TEXT NOT NULL DEFAULT '',
  name_source TEXT NOT NULL DEFAULT '',
  vendor      TEXT NOT NULL DEFAULT '',
  link        TEXT NOT NULL DEFAULT '',
  notes       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS probes(
  mac  TEXT NOT NULL,
  port INTEGER NOT NULL,
  PRIMARY KEY(mac, port)
);
CREATE TABLE IF NOT EXISTS seen_history(
  mac    TEXT NOT NULL,
  ts     INTEGER NOT NULL,
  state  TEXT NOT NULL,
  signal TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_seen_mac ON seen_history(mac, ts);

-- Untracked hosts ever observed on the LAN, so the ARP cache can show recently-
-- seen devices that have since dropped out of the kernel neigh cache.
CREATE TABLE IF NOT EXISTS observed(
  mac       TEXT PRIMARY KEY,
  ip        TEXT NOT NULL DEFAULT '',
  name      TEXT NOT NULL DEFAULT '',
  last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_observed_seen ON observed(last_seen);
`

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; simplest correct model for our load
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NormMAC canonicalises a MAC to lowercase, colon-separated, zero-padded form
// (e.g. "AA-BB-CC-DD-E-FF" → "aa:bb:cc:dd:0e:ff"). Padding matters because the
// macOS `arp` command emits unpadded octets while Linux netlink emits full bytes;
// both must hash to the same identity.
func NormMAC(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	m = strings.ReplaceAll(m, "-", ":")
	parts := strings.Split(m, ":")
	if len(parts) != 6 {
		return m
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

func now() int64 { return time.Now().Unix() }

func (s *Store) List() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT mac,ip,name,name_source,vendor,link,notes,created_at,updated_at FROM devices ORDER BY name<>'' DESC, name, ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.MAC, &d.IP, &d.Name, &d.NameSource, &d.Vendor, &d.Link, &d.Notes, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Probes, _ = s.probesLocked(out[i].MAC)
	}
	return out, nil
}

func (s *Store) Get(mac string) (Device, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(NormMAC(mac))
}

func (s *Store) getLocked(mac string) (Device, bool, error) {
	var d Device
	err := s.db.QueryRow(`SELECT mac,ip,name,name_source,vendor,link,notes,created_at,updated_at FROM devices WHERE mac=?`, mac).
		Scan(&d.MAC, &d.IP, &d.Name, &d.NameSource, &d.Vendor, &d.Link, &d.Notes, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return Device{}, false, nil
	}
	if err != nil {
		return Device{}, false, err
	}
	d.Probes, _ = s.probesLocked(mac)
	return d, true, nil
}

// Add inserts a new device (idempotent: existing rows keep their data but the IP
// and name are refreshed when supplied).
func (s *Store) Add(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.MAC = NormMAC(d.MAC)
	if d.MAC == "" {
		return fmt.Errorf("empty mac")
	}
	if d.Name == "" {
		d.Name = d.IP
	}
	t := now()
	_, err := s.db.Exec(`
		INSERT INTO devices(mac,ip,name,name_source,vendor,link,notes,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mac) DO UPDATE SET
		  ip=excluded.ip,
		  name=CASE WHEN devices.name='' THEN excluded.name ELSE devices.name END,
		  updated_at=excluded.updated_at`,
		d.MAC, d.IP, d.Name, d.NameSource, d.Vendor, d.Link, d.Notes, t, t)
	return err
}

// Patch updates the supplied non-nil fields of a device.
type Patch struct {
	IP         *string
	Name       *string
	NameSource *string
	Vendor     *string
	Link       *string
	Notes      *string
}

func (s *Store) Patch(mac string, p Patch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac = NormMAC(mac)
	var sets []string
	var args []any
	add := func(col string, v *string) {
		if v != nil {
			sets = append(sets, col+"=?")
			args = append(args, *v)
		}
	}
	add("ip", p.IP)
	add("name", p.Name)
	add("name_source", p.NameSource)
	add("vendor", p.Vendor)
	add("link", p.Link)
	add("notes", p.Notes)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), mac)
	_, err := s.db.Exec(`UPDATE devices SET `+strings.Join(sets, ",")+` WHERE mac=?`, args...)
	return err
}

// ReconcileIP updates a tracked device's IP if the neigh cache shows it moved.
func (s *Store) ReconcileIP(mac, ip string) error {
	mac = NormMAC(mac)
	if ip == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE devices SET ip=?, updated_at=? WHERE mac=? AND ip<>?`, ip, now(), mac, ip)
	return err
}

func (s *Store) Delete(mac string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac = NormMAC(mac)
	if _, err := s.db.Exec(`DELETE FROM probes WHERE mac=?`, mac); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM devices WHERE mac=?`, mac)
	return err
}

func (s *Store) AddProbe(mac string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO probes(mac,port) VALUES(?,?)`, NormMAC(mac), port)
	return err
}

func (s *Store) DeleteProbe(mac string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM probes WHERE mac=? AND port=?`, NormMAC(mac), port)
	return err
}

func (s *Store) probesLocked(mac string) ([]int, error) {
	rows, err := s.db.Query(`SELECT port FROM probes WHERE mac=? ORDER BY port`, mac)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Ints(out)
	return out, rows.Err()
}

// RecordSeen appends a presence snapshot (used to extend last-seen history
// beyond the kernel neigh cache's short retention).
func (s *Store) RecordSeen(mac, state, signal string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO seen_history(mac,ts,state,signal) VALUES(?,?,?,?)`, NormMAC(mac), now(), state, signal)
	return err
}

// ICMPSeenSet returns the set of MACs that have ever answered an ICMP probe
// (from seen_history). Used to decide whether an ARP-only presence is suspicious
// — i.e. a host we'd expect to answer ICMP showing only an L2 ARP reply.
func (s *Store) ICMPSeenSet() (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT DISTINCT mac FROM seen_history WHERE signal='icmp'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var m string
		if rows.Scan(&m) == nil {
			out[m] = true
		}
	}
	return out, rows.Err()
}

// LastSeenUp returns the most recent timestamp at which a MAC was confirmed UP
// via an active probe (ICMP or TCP), or 0. ARP sightings are excluded — they are
// presence, not liveness.
func (s *Store) LastSeenUp(mac string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ts int64
	s.db.QueryRow(`SELECT ts FROM seen_history WHERE mac=? AND signal IN ('icmp','tcp') ORDER BY ts DESC LIMIT 1`, NormMAC(mac)).Scan(&ts)
	return ts
}

// LastSeen returns the most recent snapshot timestamp for a MAC, or 0.
func (s *Store) LastSeen(mac string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ts int64
	s.db.QueryRow(`SELECT ts FROM seen_history WHERE mac=? ORDER BY ts DESC LIMIT 1`, NormMAC(mac)).Scan(&ts)
	return ts
}

// Observed is an untracked host previously seen on the LAN.
type Observed struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"`
}

// UpsertObserved records/refreshes a sighting of an untracked host. A non-empty
// name overwrites a stored one; otherwise the existing name is kept.
func (s *Store) UpsertObserved(mac, ip, name string, ts int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO observed(mac,ip,name,last_seen) VALUES(?,?,?,?)
		ON CONFLICT(mac) DO UPDATE SET
		  ip=excluded.ip,
		  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE observed.name END,
		  last_seen=excluded.last_seen`,
		NormMAC(mac), ip, name, ts)
	return err
}

// ListObserved returns observed hosts seen at or after sinceTS, newest first.
func (s *Store) ListObserved(sinceTS int64) ([]Observed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT mac,ip,name,last_seen FROM observed WHERE last_seen>=? ORDER BY last_seen DESC`, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observed
	for rows.Next() {
		var o Observed
		if err := rows.Scan(&o.MAC, &o.IP, &o.Name, &o.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PruneObserved deletes sightings older than beforeTS (the retention window).
func (s *Store) PruneObserved(beforeTS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM observed WHERE last_seen<?`, beforeTS)
	return err
}

// DeleteObserved removes a host from the observed list (e.g. once it's tracked).
func (s *Store) DeleteObserved(mac string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM observed WHERE mac=?`, NormMAC(mac))
	return err
}

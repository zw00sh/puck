// Package store is puckview's device database: the dynamic, app-owned record of
// tracked devices, their TCP watchdogs, and their presence state. Keyed by MAC
// (the durable identity); IP/name/vendor are re-learnable attributes. Uses the
// pure-Go modernc.org/sqlite driver so the binary stays static (no cgo).
//
// Presence is stored as per-device DERIVED SCALARS (icmp_seen, last_seen_up,
// last_seen), not as an event log. Those three values are the only thing the
// dashboard ever asked the old `seen_history` log for, and answering "has this
// MAC ever replied to ICMP" by scanning a million-row log once per second is
// what used to peg the box (see BackfillLegacy). `seen_events` keeps a
// transition-only history for forensics: a row when a device changes up/down
// state, not one per probe.
//
// The server treats this store as a journal, not as its read path: it loads
// state into memory at startup and flushes changes back through Flush. Nothing
// on the per-second render path queries SQLite.
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

// schemaVersion is stamped into PRAGMA user_version. v0 is the unversioned
// pre-derived-column layout (an append-only seen_history log).
const schemaVersion = 1

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

	// Derived presence scalars. Monotonic: once a device has answered ICMP it has
	// answered ICMP, and a last-seen only moves forward.
	IcmpSeen   bool  `json:"icmp_seen"`    // has ever answered an ICMP probe
	LastSeenUp int64 `json:"last_seen_up"` // last confirmed UP via ICMP/TCP (0 = never)
	LastSeen   int64 `json:"last_seen"`    // last sighting of any kind, ARP included
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

// baseSchema is the current (v1) layout. Every statement is IF NOT EXISTS so it
// is safe to run against an existing database; a legacy table keeps its own
// columns and picks up the new ones via migrate's ALTERs.
const baseSchema = `
CREATE TABLE IF NOT EXISTS devices(
  mac          TEXT PRIMARY KEY,
  ip           TEXT NOT NULL DEFAULT '',
  name         TEXT NOT NULL DEFAULT '',
  name_source  TEXT NOT NULL DEFAULT '',
  vendor       TEXT NOT NULL DEFAULT '',
  link         TEXT NOT NULL DEFAULT '',
  notes        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL DEFAULT 0,
  icmp_seen    INTEGER NOT NULL DEFAULT 0,
  last_seen_up INTEGER NOT NULL DEFAULT 0,
  last_seen    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS probes(
  mac  TEXT NOT NULL,
  port INTEGER NOT NULL,
  PRIMARY KEY(mac, port)
);

-- Transition-only presence history: one row when a device CHANGES up/down
-- state. Deliberately not a sample log — the v0 schema appended a row per probe
-- cycle per device and reached 1.6M rows in three months.
CREATE TABLE IF NOT EXISTS seen_events(
  mac    TEXT NOT NULL,
  ts     INTEGER NOT NULL,
  state  TEXT NOT NULL,
  signal TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_seen_events_mac ON seen_events(mac, ts);

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

// derivedColumns are the v1 additions to `devices`, applied to pre-v1 databases
// that already have the table. Ordered so migration logs read deterministically.
var derivedColumns = []struct{ name, ddl string }{
	{"icmp_seen", "INTEGER NOT NULL DEFAULT 0"},
	{"last_seen_up", "INTEGER NOT NULL DEFAULT 0"},
	{"last_seen", "INTEGER NOT NULL DEFAULT 0"},
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; simplest correct model for our load
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate brings the schema to schemaVersion. It is fast and unconditional:
// creating the v1 tables and adding the derived columns is O(1) in SQLite, so it
// is safe to run before the HTTP listener starts. Collapsing a legacy
// seen_history log into those columns is NOT done here — see BackfillLegacy.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(baseSchema); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	have, err := s.columns("devices")
	if err != nil {
		return fmt.Errorf("migrate introspect: %w", err)
	}
	for _, c := range derivedColumns {
		if have[c.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE devices ADD COLUMN ` + c.name + ` ` + c.ddl); err != nil {
			return fmt.Errorf("migrate add %s: %w", c.name, err)
		}
	}
	// user_version takes no bind parameters, hence the formatted constant.
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("migrate stamp version: %w", err)
	}
	return nil
}

// columns returns the column set of a table.
func (s *Store) columns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
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

const deviceCols = `mac,ip,name,name_source,vendor,link,notes,created_at,updated_at,icmp_seen,last_seen_up,last_seen`

func scanDevice(sc interface{ Scan(...any) error }, d *Device) error {
	return sc.Scan(&d.MAC, &d.IP, &d.Name, &d.NameSource, &d.Vendor, &d.Link, &d.Notes,
		&d.CreatedAt, &d.UpdatedAt, &d.IcmpSeen, &d.LastSeenUp, &d.LastSeen)
}

// List returns every tracked device in display order. The server calls this at
// startup and after a config mutation, never on the render path.
func (s *Store) List() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT ` + deviceCols + ` FROM devices ORDER BY name<>'' DESC, name, ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := scanDevice(rows, &d); err != nil {
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
	err := scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE mac=?`, mac), &d)
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

func (s *Store) Delete(mac string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac = NormMAC(mac)
	for _, q := range []string{
		`DELETE FROM probes WHERE mac=?`,
		`DELETE FROM seen_events WHERE mac=?`,
		`DELETE FROM devices WHERE mac=?`,
	} {
		if _, err := s.db.Exec(q, mac); err != nil {
			return err
		}
	}
	return nil
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

// --- write-behind ------------------------------------------------------------

// SeenUpdate is one device's presence scalars, as held in memory by the server.
type SeenUpdate struct {
	MAC        string
	IcmpSeen   bool
	LastSeenUp int64
	LastSeen   int64
}

// SeenEvent is an up/down transition for the history table.
type SeenEvent struct {
	MAC    string
	TS     int64
	State  string // up | down
	Signal string // icmp | tcp | arp | none
}

// ObservedUpdate is a sighting of an untracked host.
type ObservedUpdate struct {
	MAC      string
	IP       string
	Name     string
	LastSeen int64
}

// IPMove is an ARP-observed address change for a tracked device.
type IPMove struct {
	MAC string
	IP  string
}

// Batch is one write-behind flush: everything the server has accumulated in
// memory since the last commit.
type Batch struct {
	Seen    []SeenUpdate
	Events  []SeenEvent
	Obs     []ObservedUpdate
	IPMoves []IPMove
}

// Empty reports whether there is nothing to write.
func (b Batch) Empty() bool {
	return len(b.Seen) == 0 && len(b.Events) == 0 && len(b.Obs) == 0 && len(b.IPMoves) == 0
}

// Flush persists a batch of in-memory changes in ONE transaction, so a flush
// costs a single commit rather than one per row. The scalars are written with
// MAX() guards so a flush can never move a last-seen backwards (a flush can race
// a reload, and monotonicity is the invariant the UI depends on).
func (s *Store) Flush(b Batch) error {
	if b.Empty() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, u := range b.Seen {
		if _, err := tx.Exec(`
			UPDATE devices SET
			  icmp_seen    = MAX(icmp_seen, ?),
			  last_seen_up = MAX(last_seen_up, ?),
			  last_seen    = MAX(last_seen, ?)
			WHERE mac=?`,
			boolInt(u.IcmpSeen), u.LastSeenUp, u.LastSeen, NormMAC(u.MAC)); err != nil {
			return err
		}
	}
	for _, m := range b.IPMoves {
		if m.IP == "" {
			continue
		}
		if _, err := tx.Exec(`UPDATE devices SET ip=?, updated_at=? WHERE mac=? AND ip<>?`,
			m.IP, now(), NormMAC(m.MAC), m.IP); err != nil {
			return err
		}
	}
	for _, e := range b.Events {
		if _, err := tx.Exec(`INSERT INTO seen_events(mac,ts,state,signal) VALUES(?,?,?,?)`,
			NormMAC(e.MAC), e.TS, e.State, e.Signal); err != nil {
			return err
		}
	}
	for _, o := range b.Obs {
		if _, err := tx.Exec(`
			INSERT INTO observed(mac,ip,name,last_seen) VALUES(?,?,?,?)
			ON CONFLICT(mac) DO UPDATE SET
			  ip=excluded.ip,
			  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE observed.name END,
			  last_seen=MAX(observed.last_seen, excluded.last_seen)`,
			NormMAC(o.MAC), o.IP, o.Name, o.LastSeen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- observed ----------------------------------------------------------------

// Observed is an untracked host previously seen on the LAN.
type Observed struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"`
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

// PruneSeenEvents deletes transitions older than beforeTS.
func (s *Store) PruneSeenEvents(beforeTS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM seen_events WHERE ts<?`, beforeTS)
	return err
}

// --- v0 → v1 backfill --------------------------------------------------------

// LegacyBackfillPending reports whether the pre-v1 seen_history log is still
// present. Its existence — not user_version — is the pending marker, so an
// interrupted backfill simply repeats on the next start.
func (s *Store) LegacyBackfillPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='seen_history'`).Scan(&n)
	return n > 0
}

// BackfillLegacy collapses the v0 seen_history log into the per-device derived
// columns, then drops it and reclaims the space.
//
// The queries are deliberately per-device rather than one global aggregate: the
// old `SELECT DISTINCT mac FROM seen_history WHERE signal='icmp'` scanned the
// whole table (3.3s against 1.6M rows on a NanoPi Zero2), whereas a per-device
// `LIMIT 1` rides idx_seen_mac(mac, ts) and stops at the first hit. Proving a
// negative — a device that never once answered ICMP — still costs a scan of that
// one device's rows, so this is slow enough to belong in a background goroutine
// but nowhere near the cost of the aggregate.
//
// The lock is taken per device, not for the whole run, so a config mutation
// arriving mid-backfill waits for one device rather than for all of them.
func (s *Store) BackfillLegacy() (devices int, freed int64, err error) {
	before := s.fileSize()

	macs, err := s.legacyMACs()
	if err != nil {
		return 0, 0, err
	}
	for _, mac := range macs {
		if err := s.backfillOne(mac); err != nil {
			return devices, 0, fmt.Errorf("backfill %s: %w", mac, err)
		}
		devices++
	}

	s.mu.Lock()
	_, err = s.db.Exec(`DROP TABLE IF EXISTS seen_history`)
	s.mu.Unlock()
	if err != nil {
		return devices, 0, fmt.Errorf("drop seen_history: %w", err)
	}
	// VACUUM reclaims the freed pages; it cannot run inside a transaction. In WAL
	// mode it rebuilds the database into the write-ahead log, so the main file
	// keeps its old size until a checkpoint rewrites it — without the explicit
	// TRUNCATE checkpoint the 119MB would sit on disk until something else
	// happened to checkpoint (and the reclaimed figure below would read as zero).
	s.mu.Lock()
	if _, err = s.db.Exec(`VACUUM`); err == nil {
		_, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	s.mu.Unlock()
	if err != nil {
		return devices, 0, fmt.Errorf("vacuum: %w", err)
	}
	if after := s.fileSize(); before > 0 && after > 0 {
		freed = before - after
	}
	return devices, freed, nil
}

func (s *Store) legacyMACs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT mac FROM devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) backfillOne(mac string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var icmpSeen int
	s.db.QueryRow(`SELECT 1 FROM seen_history WHERE mac=? AND signal='icmp' LIMIT 1`, mac).Scan(&icmpSeen)

	var lastUp, lastAny int64
	s.db.QueryRow(`SELECT ts FROM seen_history WHERE mac=? AND signal IN ('icmp','tcp') ORDER BY ts DESC LIMIT 1`, mac).Scan(&lastUp)
	s.db.QueryRow(`SELECT ts FROM seen_history WHERE mac=? ORDER BY ts DESC LIMIT 1`, mac).Scan(&lastAny)

	_, err := s.db.Exec(`
		UPDATE devices SET
		  icmp_seen    = MAX(icmp_seen, ?),
		  last_seen_up = MAX(last_seen_up, ?),
		  last_seen    = MAX(last_seen, ?)
		WHERE mac=?`, icmpSeen, lastUp, lastAny, mac)
	return err
}

func (s *Store) fileSize() int64 {
	fi, err := os.Stat(s.path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

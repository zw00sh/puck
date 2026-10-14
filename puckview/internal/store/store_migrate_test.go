package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// legacySchema is the v0 layout: no derived columns on `devices`, and an
// append-only seen_history sample log.
const legacySchema = `
CREATE TABLE devices(
  mac TEXT PRIMARY KEY, ip TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  name_source TEXT NOT NULL DEFAULT '', vendor TEXT NOT NULL DEFAULT '',
  link TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE probes(mac TEXT NOT NULL, port INTEGER NOT NULL, PRIMARY KEY(mac, port));
CREATE TABLE seen_history(mac TEXT NOT NULL, ts INTEGER NOT NULL, state TEXT NOT NULL, signal TEXT NOT NULL);
CREATE INDEX idx_seen_mac ON seen_history(mac, ts);
CREATE TABLE observed(mac TEXT PRIMARY KEY, ip TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '', last_seen INTEGER NOT NULL DEFAULT 0);
CREATE INDEX idx_observed_seen ON observed(last_seen);
`

// writeLegacyDB builds a v0 database with two devices: one that has answered
// ICMP, and one that only ever answered a TCP watchdog (the case that forces the
// backfill to prove a negative).
func writeLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	for _, mac := range []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"} {
		if _, err := db.Exec(`INSERT INTO devices(mac,ip,name,created_at) VALUES(?,?,?,?)`,
			mac, "192.0.2.1", "dev", 1000); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		mac, signal string
		ts          int64
	}{
		{"aa:bb:cc:dd:ee:01", "arp", 1100},
		{"aa:bb:cc:dd:ee:01", "icmp", 1200},
		{"aa:bb:cc:dd:ee:01", "icmp", 1500}, // newest up
		{"aa:bb:cc:dd:ee:01", "arp", 1600},  // newest sighting of any kind
		{"aa:bb:cc:dd:ee:02", "arp", 1300},
		{"aa:bb:cc:dd:ee:02", "tcp", 1400}, // up, but never ICMP
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO seen_history(mac,ts,state,signal) VALUES(?,?,?,?)`,
			r.mac, r.ts, "up", r.signal); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateLegacyAndBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDB(t, path)

	st, err := Open(path) // migrate runs here
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// The schema migration is synchronous, so the derived columns must exist and
	// the version must be stamped before Open returns.
	cols, err := st.columns("devices")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"icmp_seen", "last_seen_up", "last_seen"} {
		if !cols[c] {
			t.Errorf("column %s missing after migrate", c)
		}
	}
	var v int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The backfill is deferred, so it must still be pending and the log present.
	if !st.LegacyBackfillPending() {
		t.Fatal("LegacyBackfillPending() = false, want true before backfill")
	}

	n, _, err := st.BackfillLegacy()
	if err != nil {
		t.Fatalf("BackfillLegacy: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled %d devices, want 2", n)
	}
	if st.LegacyBackfillPending() {
		t.Error("seen_history still present after backfill")
	}

	want := map[string]Device{
		"aa:bb:cc:dd:ee:01": {IcmpSeen: true, LastSeenUp: 1500, LastSeen: 1600},
		"aa:bb:cc:dd:ee:02": {IcmpSeen: false, LastSeenUp: 1400, LastSeen: 1400},
	}
	for mac, w := range want {
		d, ok, err := st.Get(mac)
		if err != nil || !ok {
			t.Fatalf("Get(%s) ok=%v err=%v", mac, ok, err)
		}
		if d.IcmpSeen != w.IcmpSeen || d.LastSeenUp != w.LastSeenUp || d.LastSeen != w.LastSeen {
			t.Errorf("%s: icmpSeen=%v lastSeenUp=%d lastSeen=%d; want %v/%d/%d",
				mac, d.IcmpSeen, d.LastSeenUp, d.LastSeen, w.IcmpSeen, w.LastSeenUp, w.LastSeen)
		}
	}

	// Reopening a migrated database must be a no-op, not a repeat migration.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if st2.LegacyBackfillPending() {
		t.Error("backfill reported pending on an already-migrated database")
	}
}

func TestBackfillReclaimsSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.db")
	writeLegacyDB(t, path)

	// Pad the log so the drop has something material to reclaim.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	for i := 0; i < 60000; i++ {
		tx.Exec(`INSERT INTO seen_history(mac,ts,state,signal) VALUES(?,?,?,?)`,
			"aa:bb:cc:dd:ee:01", 2000+i, "up", "icmp")
	}
	tx.Commit()
	db.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := fi.Size()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, freed, err := st.BackfillLegacy()
	if err != nil {
		t.Fatalf("BackfillLegacy: %v", err)
	}
	if freed <= 0 {
		t.Errorf("freed = %d, want > 0 (the WAL checkpoint should shrink the main file)", freed)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= before {
		t.Errorf("file did not shrink: %d → %d bytes", before, fi.Size())
	}
}

func TestFlushIsMonotonic(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mac := "aa:bb:cc:dd:ee:ff"
	if err := st.Add(Device{MAC: mac, IP: "192.0.2.9"}); err != nil {
		t.Fatal(err)
	}

	if err := st.Flush(Batch{Seen: []SeenUpdate{{MAC: mac, IcmpSeen: true, LastSeenUp: 500, LastSeen: 900}}}); err != nil {
		t.Fatal(err)
	}
	// A stale flush (e.g. one that raced a reload) must not walk values backwards.
	if err := st.Flush(Batch{Seen: []SeenUpdate{{MAC: mac, IcmpSeen: false, LastSeenUp: 100, LastSeen: 200}}}); err != nil {
		t.Fatal(err)
	}
	d, _, err := st.Get(mac)
	if err != nil {
		t.Fatal(err)
	}
	if !d.IcmpSeen || d.LastSeenUp != 500 || d.LastSeen != 900 {
		t.Errorf("stale flush regressed state: icmpSeen=%v lastSeenUp=%d lastSeen=%d",
			d.IcmpSeen, d.LastSeenUp, d.LastSeen)
	}

	// IP moves and transition rows ride the same batch.
	if err := st.Flush(Batch{
		IPMoves: []IPMove{{MAC: mac, IP: "192.0.2.10"}},
		Events:  []SeenEvent{{MAC: mac, TS: 1000, State: "down", Signal: "none"}},
	}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := st.Get(mac); d.IP != "192.0.2.10" {
		t.Errorf("IP = %s, want 192.0.2.10", d.IP)
	}
	var events int
	st.db.QueryRow(`SELECT count(*) FROM seen_events WHERE mac=?`, mac).Scan(&events)
	if events != 1 {
		t.Errorf("seen_events rows = %d, want 1", events)
	}

	// Retention prunes transitions by timestamp.
	if err := st.PruneSeenEvents(2000); err != nil {
		t.Fatal(err)
	}
	st.db.QueryRow(`SELECT count(*) FROM seen_events WHERE mac=?`, mac).Scan(&events)
	if events != 0 {
		t.Errorf("seen_events rows after prune = %d, want 0", events)
	}
}

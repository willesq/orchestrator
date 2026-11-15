package toollog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRulesRevColumn is spaces D-66 in the command log: a log of schema 2
// gains events.rules_rev when opened, its records keep their hashes and
// read as revision 0, and a record stored with a revision reads back with
// it and verifies.
func TestRulesRevColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toollog.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	o := session("sess")
	w := NewWriter(s, DefaultQueue, nil)
	w.Enqueue(key(o), 100, stamped("echo old", "logged", t0))
	flush(t, w)
	w.Close()
	_ = s.Close()

	// Make it a schema 2 log: the table as it was, without the column.
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	cols := strings.Replace(recordColumns, " rules_rev,", "", 1)
	v2 := strings.Replace(strings.Split(schema, "CREATE INDEX")[0], "rules_rev       INTEGER NOT NULL DEFAULT 0,", "", 1)
	for _, q := range []string{
		`ALTER TABLE events RENAME TO events_new`,
		v2,
		`INSERT INTO events (` + cols + `) SELECT ` + cols + ` FROM events_new`,
		`DROP TABLE events_new`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = old.Close()
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	if rep := verify(t, s, o); !rep.OK() || rep.Records != 1 {
		t.Fatalf("old record after the upgrade: %+v", rep)
	}
	w = NewWriter(s, DefaultQueue, nil)
	w.Enqueue(key(o), 100, func() Record {
		r := stamped("echo new", "logged", t0)()
		r.RulesRev = 5
		return r
	})
	flush(t, w)
	w.Close()
	recs, _, _, err := s.Page(context.Background(), o, 0, 10, 1<<20)
	if err != nil || len(recs) != 2 || recs[0].RulesRev != 0 || recs[1].RulesRev != 5 {
		t.Fatalf("records: %+v %v", recs, err)
	}
	if rep := verify(t, s, o); !rep.OK() || rep.Records != 2 {
		t.Fatalf("chain: %+v", rep)
	}
	// The revision is covered by the hash.
	if _, err := s.db.Exec(`UPDATE events SET rules_rev = 1 WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	if rep := verify(t, s, o); rep.OK() {
		t.Error("a changed rules revision verified")
	}
	// Opening again is a no-op.
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
}

package reviews

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw    string
		repo   string
		number int
		ok     bool
	}{
		{"https://github.com/willesq/theCalculator/pull/7", "willesq/theCalculator", 7, true},
		{"https://github.com/a/b.c_d-e/pull/123", "a/b.c_d-e", 123, true},
		{"http://github.com/a/b/pull/1", "", 0, false},
		{"https://github.com/a/b/pull/0", "", 0, false},
		{"https://github.com/a/b/issues/1", "", 0, false},
		{"https://github.com/a/../pull/1", "", 0, false},
		{"https://github.com/a/b/pull/1?x=1", "", 0, false},
		{"https://github.com/a/b/pull/1/files", "", 0, false},
		{"https://evil.com/a/b/pull/1", "", 0, false},
		{"https://github.com/-a/b/pull/1", "", 0, false},
		{"", "", 0, false},
	}
	for _, tt := range tests {
		repo, n, err := ParseURL(tt.raw)
		if (err == nil) != tt.ok || repo != tt.repo || n != tt.number {
			t.Errorf("ParseURL(%q) = %q, %d, %v; want %q, %d, ok=%v", tt.raw, repo, n, err, tt.repo, tt.number, tt.ok)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testDB(t))
	clock := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return clock }

	if _, found, err := s.Get(ctx, "https://github.com/a/b/pull/1"); err != nil || found {
		t.Fatalf("Get on empty store = %v, %v", found, err)
	}
	r := Review{URL: "https://github.com/a/b/pull/1", Repo: "a/b", Number: 1, Mode: ModeApprove, State: StateVerdict,
		HeadSHA: "h", Verdict: VerdictApprove, Reason: "fine", Comment: "LGTM", VerdictAt: clock}
	if err := s.Put(ctx, &r); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Get(ctx, r.URL)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if got.Mode != ModeApprove || got.Verdict != VerdictApprove || got.Comment != "LGTM" || !got.CreatedAt.Equal(clock) ||
		!got.VerdictAt.Equal(clock) || got.Number != 1 {
		t.Errorf("round trip = %+v", got)
	}

	clock = clock.Add(time.Hour)
	other := Review{URL: "https://github.com/a/b/pull/2", Repo: "a/b", Number: 2, Mode: ModeVerdict, State: StateWaiting}
	if err := s.Put(ctx, &other); err != nil {
		t.Fatal(err)
	}
	got.Note = "changed"
	clock = clock.Add(time.Hour)
	if err := s.Put(ctx, &got); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if list[0].URL != r.URL || list[0].Note != "changed" || !list[0].CreatedAt.Equal(time.Unix(1_700_000_000, 0)) {
		t.Errorf("List order or update wrong: %+v", list)
	}
	if !list[1].VerdictAt.IsZero() {
		t.Errorf("unset verdict time = %v, want zero", list[1].VerdictAt)
	}

	if err := s.Delete(ctx, r.URL); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Get(ctx, r.URL); found {
		t.Error("deleted review still found")
	}
	if err := s.Delete(ctx, r.URL); err != nil {
		t.Errorf("deleting a missing review: %v", err)
	}
}

func TestStoreErrors(t *testing.T) {
	conn := testDB(t)
	s := NewStore(conn)
	_ = conn.Close()
	ctx := context.Background()
	if _, err := s.List(ctx); err == nil {
		t.Error("List on a closed db succeeded")
	}
	if _, _, err := s.Get(ctx, "x"); err == nil {
		t.Error("Get on a closed db succeeded")
	}
	if err := s.Put(ctx, &Review{URL: "x"}); err == nil {
		t.Error("Put on a closed db succeeded")
	}
}

func TestActiveAndValidMode(t *testing.T) {
	for _, st := range []string{StateWaiting, StateReviewing, StateVerdict, StateFailed} {
		if !(Review{State: st}).Active() {
			t.Errorf("%s not active", st)
		}
	}
	for _, st := range []string{StateClosed, StateStopped} {
		if (Review{State: st}).Active() {
			t.Errorf("%s active", st)
		}
	}
	for _, m := range []string{ModeVerdict, ModeApprove, ModeComment} {
		if !ValidMode(m) {
			t.Errorf("%s invalid", m)
		}
	}
	if ValidMode("merge") || ValidMode("") {
		t.Error("unknown mode accepted")
	}
}

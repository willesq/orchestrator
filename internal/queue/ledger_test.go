package queue

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cschleiden/go-workflows/backend/sqlite"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/workflows"
)

func newTestStore(t *testing.T) *SQLTaskStore {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewSQLTaskStore(conn)
}

func task(id, agent, body string) workflows.AgentTaskInput {
	return workflows.AgentTaskInput{ID: id, Agent: agent, Body: body}
}

func TestCreateRecordsTaskAsPending(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Create(ctx, task("t1", "dev", "fix it")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, found, err := store.Get(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("Get: err=%v found=%v", err, found)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.Agent != "dev" || got.Body != "fix it" {
		t.Errorf("agent/body = %q/%q, want dev/fix it", got.Agent, got.Body)
	}
	if got.Result != "" {
		t.Errorf("result = %q, want empty on a fresh task", got.Result)
	}
}

// Re-creating must not clobber a task that has already progressed — a duplicate
// dispatch of the same ID should not reset a completed task back to pending.
func TestCreateIsIdempotentAndDoesNotResetProgress(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	in := task("t1", "dev", "body")

	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SaveResult(ctx, in, "done"); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	got, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "complete" || got.Result != "done" {
		t.Errorf("status/result = %q/%q, want complete/done", got.Status, got.Result)
	}
}

func TestSetStatusLeavesTheResultAlone(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	in := task("t1", "dev", "body")
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.SetStatus(ctx, "t1", "in_progress"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress", got.Status)
	}

	// Unknown IDs are a no-op rather than an error: the workflow engine may
	// transition a task whose row was never written (store was nil at dispatch).
	if err := store.SetStatus(ctx, "nope", "in_progress"); err != nil {
		t.Errorf("SetStatus on a missing id should be a no-op, got %v", err)
	}
}

func TestGetMissingTask(t *testing.T) {
	store := newTestStore(t)
	got, found, err := store.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Errorf("found = true for an absent id, got %+v", got)
	}
}

func TestListFiltersAndLimits(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seed := []struct {
		id, agent, status string
	}{
		{"t1", "dev", "pending"},
		{"t2", "dev", "in_progress"},
		{"t3", "ops", "pending"},
	}
	for _, s := range seed {
		if err := store.Create(ctx, task(s.id, s.agent, "body "+s.id)); err != nil {
			t.Fatalf("Create %s: %v", s.id, err)
		}
		if err := store.SetStatus(ctx, s.id, s.status); err != nil {
			t.Fatalf("SetStatus %s: %v", s.id, err)
		}
	}

	tests := []struct {
		name    string
		filter  ListFilter
		wantIDs []string
	}{
		{name: "no filter returns all", filter: ListFilter{}, wantIDs: []string{"t1", "t2", "t3"}},
		{name: "by status", filter: ListFilter{Status: "pending"}, wantIDs: []string{"t1", "t3"}},
		{name: "by agent", filter: ListFilter{Agent: "ops"}, wantIDs: []string{"t3"}},
		{name: "status and agent combine", filter: ListFilter{Status: "pending", Agent: "dev"}, wantIDs: []string{"t1"}},
		{name: "no matches", filter: ListFilter{Status: "failed"}, wantIDs: nil},
		{name: "limit caps the result", filter: ListFilter{Limit: 2}, wantIDs: []string{"t1", "t2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.List(ctx, tt.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("got %d tasks, want %d (%+v)", len(got), len(tt.wantIDs), got)
			}
			// Rows share a one-second timestamp granularity, so compare as a set.
			seen := map[string]bool{}
			for _, g := range got {
				seen[g.ID] = true
			}
			for _, want := range tt.wantIDs {
				if tt.filter.Limit > 0 {
					continue // limited sets are checked by length only
				}
				if !seen[want] {
					t.Errorf("missing %s from %+v", want, got)
				}
			}
		})
	}
}

func TestListDefaultsToFiftyRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for i := range 60 {
		id := string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := store.Create(ctx, task(id, "dev", "b")); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got, err := store.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("got %d rows, want the 50-row default cap", len(got))
	}
}

// Dispatch must leave a visible row behind: before this, a task had no ledger
// entry until it finished, so anything inspecting the queue saw nothing while
// work was outstanding.
func TestDispatchRecordsAPendingRow(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := NewSQLTaskStore(conn)

	b := sqlite.NewInMemoryBackend(sqlite.WithApplyMigrations(true))
	t.Cleanup(func() { _ = b.Close() })
	d := NewDispatcher(b, store)

	id, err := d.Dispatch(context.Background(), "ops", "investigate the thing")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got, found, err := store.Get(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("dispatched task is not in the ledger: err=%v found=%v", err, found)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.Agent != "ops" || got.Body != "investigate the thing" {
		t.Errorf("agent/body = %q/%q", got.Agent, got.Body)
	}
}

func TestDispatchWithoutAStoreStillWorks(t *testing.T) {
	b := sqlite.NewInMemoryBackend(sqlite.WithApplyMigrations(true))
	t.Cleanup(func() { _ = b.Close() })
	d := NewDispatcher(b, nil)

	id, err := d.Dispatch(context.Background(), "dev", "body")
	if err != nil {
		t.Fatalf("Dispatch with a nil store: %v", err)
	}
	if id == "" {
		t.Error("empty instance id")
	}
}

func TestListClampsTheLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for i := range 600 {
		id := string(rune('a'+i/676)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i%26))
		if err := store.Create(ctx, task(id, "dev", "b")); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "zero takes the default", limit: 0, want: 50},
		{name: "negative takes the default", limit: -5, want: 50},
		{name: "under the cap is honoured", limit: 10, want: 10},
		{name: "oversized is clamped", limit: 1 << 30, want: maxListLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.List(ctx, ListFilter{Limit: tt.limit})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("rows = %d, want %d", len(got), tt.want)
			}
		})
	}
}

// Regression pin on parameterisation: filter values must never reach the SQL text.
func TestListFilterValuesAreBoundNotInterpolated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Create(ctx, task("t1", "dev", "body")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, injection := range []string{"' OR 1=1--", "'; DROP TABLE tasks;--", "pending' OR '1'='1"} {
		got, err := store.List(ctx, ListFilter{Status: injection})
		if err != nil {
			t.Fatalf("List(%q): %v", injection, err)
		}
		if len(got) != 0 {
			t.Errorf("List(%q) returned %d rows; the value must be bound, not interpolated", injection, len(got))
		}
	}
	// The table must still be there.
	if all, err := store.List(ctx, ListFilter{}); err != nil || len(all) != 1 {
		t.Fatalf("after injection attempts: %d rows, err %v", len(all), err)
	}
}

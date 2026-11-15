package daemon

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

var appOperator = as(resolvedCaller{kind: callerOperator, operatorBy: operatorApp})

// statusCounts is the counts ctx's caller gets from Status.
func statusCounts(t *testing.T, s *server, ctx context.Context) *orchdv1.SpacesCounts {
	t.Helper()
	resp, err := s.Status(ctx, &orchdv1.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetSpaces().GetCounts()
}

func countedSpaces(c *orchdv1.SpacesCounts) map[string][2]int32 {
	out := map[string][2]int32{}
	for _, sc := range c.GetSpaces() {
		out[sc.GetSpace()] = [2]int32{sc.GetChunks(), sc.GetStale()}
	}
	return out
}

// pinAt records a live session pinned to space whose first folder is cwd.
func pinAt(t *testing.T, s *server, id, cwd, space string) {
	t.Helper()
	p := sessionPin{SessionID: id, Harness: "claude", Process: procstat.Identity{PID: 900, Started: claudeStart},
		FirstCwd: cwd, Space: space, Revision: 1, PinnedAt: time.Now()}
	if err := s.pins.insert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// TestStatusCountsOnlyWhatTheCallerReads is SP-031.1.a1: the operator
// (Orch.app) sees every space's chunks, the relabel totals over every
// collection and the conflicts; a session, a task or any other caller sees
// only its own space and the public ones, and no conflicts.
func TestStatusCountsOnlyWhatTheCallerReads(t *testing.T) {
	e := newReadEnv(t)
	// Two acme chunks a relabel has not reached yet.
	for _, id := range []string{"old-1", "old-2"} {
		e.q.seed("c__acme", id, map[string]any{"text": "old", "source_uri": "note://acme/old", "chunk_index": 0.0,
			"space": "acme", "labels_v": e.labels - 1})
	}
	// A live session whose folder acme's root and globex's remote both
	// claim (a conflict), one in a scratch folder no root holds, and an
	// ended one in the conflict.
	conflict := e.code + "/acme/globex-fork"
	checkout(t, conflict, "globex", "x")
	e.s.repos = newRepoLookups((&fakeRepos{repos: map[string][3]string{"globex/x": {"globex", "x", "private"}}}).run)
	scratch := t.TempDir()
	pinAt(t, e.s, "conflicted", conflict, spaces.Quarantine)
	pinAt(t, e.s, "scratch", scratch, spaces.Quarantine)
	pinAt(t, e.s, "ended", conflict, spaces.Quarantine)
	if _, err := e.s.pins.db.Exec(`UPDATE session_pins SET ended_at = ? WHERE session_id = 'ended'`, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	op := statusCounts(t, e.s, appOperator)
	if !op.GetEverySpace() || !op.GetConflictsCounted() || op.GetConflicts() != 1 || op.GetError() != "" {
		t.Fatalf("operator counts = %+v; want every space and 1 conflict", op)
	}
	want := map[string][2]int32{"acme": {chunksPerSpace + 3, 2}, "globex": {chunksPerSpace + 1, 0},
		"oss": {chunksPerSpace + 1, 0}, "personal": {chunksPerSpace, 0}}
	if got := countedSpaces(op); !mapsEqual(got, want) {
		t.Fatalf("operator spaces = %v, want %v", got, want)
	}
	// Every collection: the four spaces, 10 quarantined and 10 unplaced
	// chunks stored before spaces, which are stale.
	if op.GetChunks() != 4*chunksPerSpace+5+20 || op.GetStale() != 12 || op.GetCountedAt() == 0 {
		t.Fatalf("operator totals = %d chunks, %d stale", op.GetChunks(), op.GetStale())
	}
	for _, sc := range op.GetSpaces() {
		if sc.GetSpace() == "oss" && sc.GetKind() != string(spaces.Public) {
			t.Fatalf("oss kind = %q", sc.GetKind())
		}
	}

	for _, tt := range []struct {
		name   string
		ctx    context.Context
		spaces []string
	}{
		{"a session in acme", inSpace("acme"), []string{"acme", "oss", "personal"}},
		{"a session in globex", inSpace("globex"), []string{"globex", "oss"}},
		{"a session in quarantine", inSpace(spaces.Quarantine), []string{"oss"}},
		{"a task dispatched from globex", as(resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "t1", Space: "globex"}}), []string{"globex", "oss"}},
		{"the operator, not Orch.app", as(resolvedCaller{kind: callerOperator}), []string{"oss"}},
		{"an unidentified caller", context.Background(), []string{"oss"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := statusCounts(t, e.s, tt.ctx)
			got := countedSpaces(c)
			var names []string
			var chunks, stale int32
			for name, n := range got {
				names = append(names, name)
				chunks += n[0]
				stale += n[1]
			}
			slices.Sort(names)
			if !slices.Equal(names, tt.spaces) {
				t.Fatalf("spaces = %v, want %v", names, tt.spaces)
			}
			if c.GetEverySpace() || c.GetConflictsCounted() || c.GetConflicts() != 0 {
				t.Fatalf("a caller other than the operator got %+v", c)
			}
			if c.GetChunks() != chunks || c.GetStale() != stale {
				t.Fatalf("totals %d/%d, want the readable spaces' %d/%d", c.GetChunks(), c.GetStale(), chunks, stale)
			}
		})
	}
}

func mapsEqual(a, b map[string][2]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// spaceCountStore counts how often the spaces are counted, and can fail.
type spaceCountStore struct {
	ragService
	calls int
	err   error
}

func (c *spaceCountStore) CountSpaces(context.Context, int64) ([]rag.CollectionCount, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return []rag.CollectionCount{{Space: "oss", Chunks: 4, Stale: 1}}, nil
}

// TestStatusCountsAreCached: counts are taken at most once per TTL for one
// revision, and a failed count reports neither the store's error (it names
// collections) nor retries until the TTL ends.
func TestStatusCountsAreCached(t *testing.T) {
	e := newReadEnv(t)
	st := &spaceCountStore{ragService: e.s.rag}
	e.s.rag = st
	for range 3 {
		if c := statusCounts(t, e.s, inSpace("acme")); c.GetChunks() != 4 || c.GetStale() != 1 {
			t.Fatalf("counts = %+v", c)
		}
	}
	if st.calls != 1 {
		t.Fatalf("counted %d times, want once", st.calls)
	}
	e.s.spaceCounts.at = time.Now().Add(-spaceCountTTL)
	statusCounts(t, e.s, inSpace("acme"))
	if st.calls != 2 {
		t.Fatalf("an expired count was not taken again (%d calls)", st.calls)
	}

	e.s.spaceCounts = spaceCountCache{}
	st.err = errors.New(`collection "c__globex" is unreachable`)
	for range 2 {
		c := statusCounts(t, e.s, appOperator)
		if c.GetError() != errCountsUnavailable || len(c.GetSpaces()) != 0 {
			t.Fatalf("failed counts = %+v", c)
		}
	}
	if st.calls != 3 {
		t.Fatalf("a failed count was retried within the TTL (%d calls)", st.calls)
	}
}

// TestStatusCountsNeedAPolicy: with no policy, or no store that counts,
// Status carries no counts (INV-10).
func TestStatusCountsNeedAPolicy(t *testing.T) {
	s := spacesServer(t)
	s.rag = &spaceCountStore{}
	if c := statusCounts(t, s, appOperator); c != nil {
		t.Fatalf("counts with no policy: %+v", c)
	}
	e := newReadEnv(t)
	e.s.rag = struct{ ragService }{e.s.rag}
	if c := statusCounts(t, e.s, appOperator); c != nil {
		t.Fatalf("counts from a store that cannot count: %+v", c)
	}
	e.s.pins = nil
	e.s.rag = &spaceCountStore{}
	if c := statusCounts(t, e.s, appOperator); c.GetConflicts() != 0 || !c.GetConflictsCounted() {
		t.Fatalf("counts without pins = %+v", c)
	}
}

// TestSearchReportsWhatWasSearched is SP-031's rag why: Search names the
// caller's space, whether it was scoped, and the collections read, and a
// scoped caller's collections are only its readable spaces'.
func TestSearchReportsWhatWasSearched(t *testing.T) {
	e := newReadEnv(t)
	for _, tt := range []struct {
		name   string
		ctx    context.Context
		space  string
		scoped bool
		want   []string
	}{
		{"a session in acme", inSpace("acme"), "acme", true, []string{"c__acme", "c__oss", "c__personal"}},
		{"a task from globex", as(resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "t1", Space: "globex"}}), "globex", true, []string{"c__globex", "c__oss"}},
		{"an untracked task", as(resolvedCaller{kind: callerTask}), "", true, []string{"c__oss"}},
		{"the operator", appOperator, "", false, []string{"c", "c__acme", "c__globex", "c__oss", "c__personal", "c__quarantine"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := e.s.Search(tt.ctx, &orchdv1.SearchRequest{Query: "ledger code", TopK: 5})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCallerSpace() != tt.space || resp.GetScoped() != tt.scoped || !slices.Equal(resp.GetCollections(), tt.want) {
				t.Fatalf("search reported space %q scoped %v collections %v; want %q %v %v",
					resp.GetCallerSpace(), resp.GetScoped(), resp.GetCollections(), tt.space, tt.scoped, tt.want)
			}
		})
	}
}

// failingCollector cannot name its collections.
type failingCollector struct{ ragService }

func (failingCollector) SearchCollections(context.Context, []string) ([]string, error) {
	return nil, os.ErrDeadlineExceeded
}

// TestSearchWithoutCollections: a store that cannot name the collections
// still answers the search, with the caller's space and no collections.
func TestSearchWithoutCollections(t *testing.T) {
	e := newReadEnv(t)
	e.s.rag = failingCollector{e.s.rag}
	resp, err := e.s.Search(inSpace("acme"), &orchdv1.SearchRequest{Query: "ledger code", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetHits()) == 0 || resp.GetCallerSpace() != "acme" || !resp.GetScoped() || len(resp.GetCollections()) != 0 {
		t.Fatalf("resp = space %q scoped %v collections %v, %d hits", resp.GetCallerSpace(), resp.GetScoped(), resp.GetCollections(), len(resp.GetHits()))
	}
}

// TestSearchUnscopedHidesCollections: with memory filtering switched off a
// session's search is unscoped, and it is not told every collection's name
// (each holds a space's); Orch.app still is.
func TestSearchUnscopedHidesCollections(t *testing.T) {
	e := newReadEnv(t)
	setSwitch(t, e.s, "memory", spaces.SwitchOff, 0)
	captureReadLog(t)
	resp, err := e.s.Search(inSpace("acme"), &orchdv1.SearchRequest{Query: "ledger code", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetScoped() || resp.GetCallerSpace() != "acme" || len(resp.GetCollections()) != 0 {
		t.Fatalf("unscoped session search reported scoped %v space %q collections %v", resp.GetScoped(), resp.GetCallerSpace(), resp.GetCollections())
	}
	resp, err = e.s.Search(appOperator, &orchdv1.SearchRequest{Query: "ledger code", TopK: 5})
	if err != nil || len(resp.GetCollections()) != 6 {
		t.Fatalf("operator collections %v, %v", resp.GetCollections(), err)
	}
	s := spacesServer(t)
	s.rag = e.s.rag
	resp, err = s.Search(context.Background(), &orchdv1.SearchRequest{Query: "ledger code", TopK: 5})
	if err != nil || resp.GetScoped() || len(resp.GetCollections()) == 0 {
		t.Fatalf("with no policy, collections %v scoped %v, %v", resp.GetCollections(), resp.GetScoped(), err)
	}
}

// TestConflictCountFailureIsNotAnAnswer: when the pins cannot be read the
// count fails as a whole, rather than reporting no conflicts.
func TestConflictCountFailureIsNotAnAnswer(t *testing.T) {
	e := newReadEnv(t)
	if err := e.s.pins.db.Close(); err != nil {
		t.Fatal(err)
	}
	if c := statusCounts(t, e.s, appOperator); c.GetError() != errCountsUnavailable {
		t.Fatalf("counts with unreadable pins = %+v", c)
	}
}

package spaces

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

func openStore(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func openEngine(t *testing.T, conn *sql.DB) *Engine {
	t.Helper()
	e, err := Open(context.Background(), conn, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var (
	validPolicy = Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"acme"}, Roots: []string{"~/code/acme"}},
		{Name: "personal", Kind: Private, Roots: []string{"~/code/willesq"}},
	}}
	otherValidPolicy = Policy{Spaces: []Space{
		{Name: "globex", Kind: Isolated, GitHubOrgs: []string{"globex"}},
	}}
	invalidPolicy = Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, Reads: []string{"globex"}},
		{Name: "globex", Kind: Isolated},
	}}
)

// current returns the enforced policy, failing when the engine is broken.
func current(t *testing.T, e *Engine) *Resolved {
	t.Helper()
	r, err := e.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	return r
}

func saveAndApply(t *testing.T, e *Engine, p Policy) Revision {
	t.Helper()
	ctx := context.Background()
	r, ps, err := e.SaveDraft(ctx, p, "test", true)
	if err != nil || ps != nil {
		t.Fatalf("SaveDraft: %v %v", err, ps)
	}
	if _, err := e.Apply(ctx, r.Number, "test", false, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return r
}

func status(t *testing.T, e *Engine) Status {
	t.Helper()
	st, err := e.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestEngineEmptyStoreIsNoPolicy(t *testing.T) {
	e := openEngine(t, openStore(t))
	if current(t, e) != nil {
		t.Fatal("empty store has a policy")
	}
	st := status(t, e)
	if st.Configured || st.AppliedRevision != 0 || st.DraftRevision != 0 || len(st.Drift) != 0 || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	d, err := e.Diff(context.Background())
	if err != nil || d.Applied != nil || d.Draft != nil {
		t.Fatalf("Diff = %+v, %v", d, err)
	}
}

func TestEngineApplyEnforces(t *testing.T) {
	conn := openStore(t)
	e := openEngine(t, conn)
	r := saveAndApply(t, e, validPolicy)
	if !current(t, e).Configured() || !current(t, e).CanRead("acme", "acme") {
		t.Fatal("applied policy not enforced")
	}
	st := status(t, e)
	if !st.Configured || st.AppliedRevision != r.Number || st.DraftRevision != 0 || len(st.Drift) != 0 || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	// A restart loads the same revision.
	if got := current(t, openEngine(t, conn)); got == nil || !got.CanRead("acme", "acme") {
		t.Fatal("restart lost the applied policy")
	}
}

// TestApplyInvalidKeepsApplied is SP-020.1.a2: an invalid draft never
// replaces the applied revision, and status reports the error.
func TestApplyInvalidKeepsApplied(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	good := saveAndApply(t, e, validPolicy)
	bad, ps, err := e.SaveDraft(ctx, invalidPolicy, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Has(RuleIsolatedReadsIsolated) {
		t.Fatalf("SaveDraft problems = %v", ps)
	}
	_, err = e.Apply(ctx, bad.Number, "test", false, nil)
	var got Problems
	if !errors.As(err, &got) || !got.Has(RuleIsolatedReadsIsolated) {
		t.Fatalf("Apply(invalid) = %v, want Problems", err)
	}
	if !current(t, e).Configured() || !current(t, e).CanRead("acme", "acme") {
		t.Fatal("invalid apply replaced the enforced policy")
	}
	st := status(t, e)
	if st.AppliedRevision != good.Number || st.DraftRevision != bad.Number {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(st.Error, "does not validate") {
		t.Fatalf("status error = %q, want the draft's validation error", st.Error)
	}
	if strings.Contains(st.Error, "acme") || strings.Contains(st.Error, "globex") {
		t.Fatalf("status error names a space: %q", st.Error)
	}
	// The answer is cached per revision, and asked again gives the same.
	if again := status(t, e); again.Error != st.Error {
		t.Fatalf("cached status = %+v", again)
	}
	d, err := e.Diff(ctx)
	if err != nil || !d.Problems.Has(RuleIsolatedReadsIsolated) || d.Draft == nil || d.Draft.Number != bad.Number {
		t.Fatalf("Diff = %+v, %v", d, err)
	}
}

// TestApplyRequiresReviewedRevision is SP-020.2.a2.
func TestApplyRequiresReviewedRevision(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	reviewed, _, err := e.SaveDraft(ctx, validPolicy, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	newer, _, err := e.SaveDraft(ctx, otherValidPolicy, "operator", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, reviewed.Number, "operator", false, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("Apply(older) = %v, want ErrStale", err)
	}
	if current(t, e) != nil {
		t.Fatal("stale apply changed the policy")
	}
	if _, err := e.Apply(ctx, newer.Number+1, "operator", false, nil); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("Apply(unsaved) = %v, want ErrNoRevision", err)
	}
	if _, err := e.Apply(ctx, newer.Number, "operator", false, nil); err != nil {
		t.Fatalf("Apply(newest) = %v", err)
	}
	if !current(t, e).CanRead("globex", "globex") {
		t.Fatal("newest revision not enforced")
	}
}

// TestAgentDraftNeedsConfirmation: a draft an agent session saved is not
// applied unless the operator says they reviewed it as such.
func TestAgentDraftNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	mine, _, _ := e.SaveDraft(ctx, validPolicy, "operator", true)
	theirs, _, err := e.SaveDraft(ctx, validPolicy, "process 9 in claude", false)
	if err != nil {
		t.Fatal(err)
	}
	if theirs.Number == mine.Number || theirs.ByOperator {
		t.Fatalf("an agent's copy of the operator's draft reused it: %+v", theirs)
	}
	if _, err := e.Apply(ctx, theirs.Number, "operator", false, nil); !errors.Is(err, ErrAgentDraft) {
		t.Fatalf("Apply(agent draft) = %v, want ErrAgentDraft", err)
	}
	if current(t, e) != nil {
		t.Fatal("an unconfirmed agent draft was applied")
	}
	if _, err := e.Apply(ctx, theirs.Number, "operator", true, nil); err != nil {
		t.Fatalf("Apply(agent draft, confirmed) = %v", err)
	}
	d, err := e.Diff(ctx)
	if err != nil || d.Draft != nil {
		t.Fatalf("Diff after apply = %+v, %v", d, err)
	}
}

func TestApplyWithNoRevision(t *testing.T) {
	e := openEngine(t, openStore(t))
	if _, err := e.Apply(context.Background(), 1, "operator", false, nil); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("Apply = %v, want ErrNoRevision", err)
	}
}

func TestSaveDraftSameAsNewestReusesIt(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	a, _, _ := e.SaveDraft(ctx, validPolicy, "test", true)
	b, _, _ := e.SaveDraft(ctx, validPolicy, "test", true)
	if a.Number != b.Number {
		t.Fatalf("identical drafts got revisions %d and %d", a.Number, b.Number)
	}
	big := Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, Roots: []string{"/" + strings.Repeat("x", maxDocBytes)}}}}
	if _, _, err := e.SaveDraft(ctx, big, "test", true); err == nil {
		t.Fatal("oversized draft saved")
	}
	many := Policy{Spaces: make([]Space, maxSpaces+1)}
	if _, _, err := e.SaveDraft(ctx, many, "test", true); err == nil {
		t.Fatal("draft with too many spaces saved")
	}
}

// TestDraftWithoutApplyIsDrift is SP-020.2.a3, first half: saving a draft
// changes nothing that is enforced and shows as drift.
func TestDraftWithoutApplyIsDrift(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	applied := saveAndApply(t, e, validPolicy)
	draft, _, err := e.SaveDraft(ctx, otherValidPolicy, "agent", false)
	if err != nil {
		t.Fatal(err)
	}
	if current(t, e).CanRead("globex", "globex") || !current(t, e).CanRead("acme", "acme") {
		t.Fatal("a saved draft changed the enforced policy")
	}
	st := status(t, e)
	if !slices.Equal(st.Drift, []string{DriftDraft}) || st.DraftRevision != draft.Number || st.AppliedRevision != applied.Number || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	d, err := e.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Changes) != 3 || d.Problems != nil || d.Draft.ByOperator || d.Draft.CreatedBy != "agent" {
		t.Fatalf("Diff = %+v", d)
	}
}

const widened = `{"spaces":[{"name":"acme","kind":"isolated","reads":["personal"]},{"name":"personal","kind":"private"}]}`

// TestEditedOutsideOrchdIsDrift is SP-020.2.a3, second half: editing the
// applied revision in orch.db directly changes nothing that is enforced, and
// shows as drift.
func TestEditedOutsideOrchdIsDrift(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, conn *sql.DB, applied Revision)
	}{
		{"document edited in place", func(t *testing.T, conn *sql.DB, r Revision) {
			exec(t, conn, `UPDATE spaces_revisions SET doc = ? WHERE revision = ?`, widened, r.Number)
		}},
		{"document and its hash edited", func(t *testing.T, conn *sql.DB, r Revision) {
			exec(t, conn, `UPDATE spaces_revisions SET doc = ?, sha256 = ? WHERE revision = ?`, widened, Hash([]byte(widened)), r.Number)
		}},
		{"apply row inserted", func(t *testing.T, conn *sql.DB, _ Revision) {
			insertApplied(t, conn, widened)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := openStore(t)
			e := openEngine(t, conn)
			r := saveAndApply(t, e, validPolicy)
			tc.edit(t, conn, r)
			if current(t, e).CanRead("acme", "personal") {
				t.Fatal("an edit outside orchd widened the enforced policy")
			}
			if st := status(t, e); !slices.Contains(st.Drift, DriftChanged) {
				t.Fatalf("status = %+v, want drift %s", st, DriftChanged)
			}
		})
	}
}

// TestRestartFailsClosed: at startup only the newest apply counts. When it no
// longer verifies, orchd enforces nothing rather than an older apply, which
// whoever broke the newest one could otherwise choose.
func TestRestartFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, conn *sql.DB, newest Revision)
	}{
		{"newest document edited", func(t *testing.T, conn *sql.DB, r Revision) {
			exec(t, conn, `UPDATE spaces_revisions SET doc = ? WHERE revision = ?`, widened, r.Number)
		}},
		{"newest revision deleted", func(t *testing.T, conn *sql.DB, r Revision) {
			// The pragma holds per connection, so both run on one.
			c, err := conn.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			for _, q := range []string{`PRAGMA foreign_keys = OFF`, `DELETE FROM spaces_revisions WHERE revision = ?`} {
				args := []any{}
				if strings.Contains(q, "?") {
					args = append(args, r.Number)
				}
				if _, err := c.ExecContext(context.Background(), q, args...); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"newest no longer validates", func(t *testing.T, conn *sql.DB, r Revision) {
			bad := `{"spaces":[{"name":"a","kind":"isolated","reads":["b"]},{"name":"b","kind":"isolated"}]}`
			exec(t, conn, `UPDATE spaces_revisions SET doc = ?, sha256 = ? WHERE revision = ?`, bad, Hash([]byte(bad)), r.Number)
			exec(t, conn, `UPDATE spaces_applied SET sha256 = ? WHERE revision = ?`, Hash([]byte(bad)), r.Number)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := openStore(t)
			e := openEngine(t, conn)
			saveAndApply(t, e, validPolicy)
			newest := saveAndApply(t, e, otherValidPolicy)
			tc.edit(t, conn, newest)
			restarted := openEngine(t, conn)
			if r, err := restarted.Current(); !errors.Is(err, ErrBroken) || r != nil {
				t.Fatalf("Current = %v, %v; want ErrBroken", r, err)
			}
			st := status(t, restarted)
			if !strings.Contains(st.Error, "does not verify") || slices.Contains(st.Drift, DriftChanged) {
				t.Fatalf("status = %+v", st)
			}
			// Applying a fresh revision mends it.
			saveAndApply(t, restarted, Policy{Spaces: []Space{{Name: "fresh", Kind: Private}}})
			if current(t, restarted) == nil || status(t, restarted).Error != "" {
				t.Fatal("apply did not clear the broken state")
			}
		})
	}
}

// TestRestartIgnoresFilesystemRules: a later change on disk (a .git above a
// terms file) does not undo an apply at the next start.
func TestRestartIgnoresFilesystemRules(t *testing.T) {
	conn := openStore(t)
	e := openEngine(t, conn)
	saveAndApply(t, e, Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, TermsFile: "~/terms/acme.txt"}}})
	gitEverywhere := testEnv()
	gitEverywhere.InGitTree = func(string) bool { return true }
	restarted, err := Open(context.Background(), conn, gitEverywhere)
	if err != nil {
		t.Fatal(err)
	}
	if r := current(t, restarted); !r.CanRead("acme", "acme") {
		t.Fatal("restart dropped the applied policy")
	}
}

func TestEngineRowWithoutApplyIsDrift(t *testing.T) {
	conn := openStore(t)
	e := openEngine(t, conn)
	insertApplied(t, conn, `{"spaces":[]}`)
	if st := status(t, e); !slices.Contains(st.Drift, DriftChanged) {
		t.Fatalf("status = %+v", st)
	}
}

func TestStoreErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := openStore(t)
	cancel()
	if _, err := SaveDraft(ctx, conn, validPolicy, "x", true, time.Now()); err == nil {
		t.Error("SaveDraft with a cancelled context succeeded")
	}
	if _, err := Apply(ctx, conn, 1, "x", false, time.Now(), testEnv(), nil, nil); err == nil {
		t.Error("Apply with a cancelled context succeeded")
	}
	_ = conn.Close()
	bg := context.Background()
	if _, err := Open(bg, conn, testEnv()); err == nil {
		t.Error("Open on a closed store succeeded")
	}
	e := &Engine{db: conn, env: testEnv(), now: time.Now}
	if _, err := e.Status(bg); err == nil {
		t.Error("Status on a closed store succeeded")
	}
	if _, err := e.Diff(bg); err == nil {
		t.Error("Diff on a closed store succeeded")
	}
	if _, _, err := e.SaveDraft(bg, validPolicy, "x", true); err == nil {
		t.Error("SaveDraft on a closed store succeeded")
	}
	if _, err := Get(bg, conn, 1); err == nil {
		t.Error("Get on a closed store succeeded")
	}
	if _, err := LoadApplied(bg, conn, testEnv()); err == nil {
		t.Error("LoadApplied on a closed store succeeded")
	}
}

func TestDiffUnparseableDraft(t *testing.T) {
	conn := openStore(t)
	e := openEngine(t, conn)
	exec(t, conn, `INSERT INTO spaces_revisions (doc, sha256, created_at, created_by, by_operator) VALUES ('nope', ?, 'x', 'agent', 1)`, Hash([]byte("nope")))
	d, err := e.Diff(context.Background())
	if err != nil || !d.Problems.Has(RuleUnparseable) || d.Draft == nil {
		t.Fatalf("Diff = %+v, %v", d, err)
	}
	if st := status(t, e); !strings.Contains(st.Error, "does not validate") {
		t.Fatalf("status = %+v", st)
	}
	if _, err := e.Apply(context.Background(), d.Draft.Number, "operator", false, nil); err == nil {
		t.Fatal("unparseable draft applied")
	}
}

func insertApplied(t *testing.T, conn *sql.DB, doc string) {
	t.Helper()
	exec(t, conn, `INSERT INTO spaces_revisions (doc, sha256, created_at, created_by) VALUES (?, ?, 'x', 'agent')`, doc, Hash([]byte(doc)))
	exec(t, conn, `INSERT INTO spaces_applied (revision, sha256, applied_at, applied_by)
		SELECT MAX(revision), ?, 'x', 'agent' FROM spaces_revisions`, Hash([]byte(doc)))
}

func exec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

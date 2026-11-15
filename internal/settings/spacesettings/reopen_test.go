package spacesettings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// TestReopened (D-67): orchd's record keeps the folders each checkout's
// file denies while it holds orch's sandbox rules, and Reopened finds,
// from a folder in the checkout (as written or through a link), an entry
// of the checkout's own settings or of a subfolder's that re-opens one.
// A checkout without orch's denies, a folder outside every checkout and a
// relative folder find nothing; a switch to off clears the record's
// denies.
func TestReopened(t *testing.T) {
	w := newWorld(t)
	s := w.syncer()
	ctx := context.Background()
	co := gitRepo(t, filepath.Join(w.code, "a", "app"))
	sub := filepath.Join(co, "sub", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(w.code, "b")
	if rep := s.Sync(ctx, w.res, ChoiceOn); len(rep.Problems) != 0 {
		t.Fatal(rep.Problems)
	}
	if got, err := Reopened(s.StateFile, []string{sub}); err != nil || len(got) != 0 {
		t.Fatalf("no loosening entry: %v, %v", got, err)
	}
	writeFile(t, filepath.Join(co, "sub", ".claude", "settings.json"), `{"sandbox":{"filesystem":{"allowRead":["`+b+`"]}}}`)
	got, err := Reopened(s.StateFile, []string{sub})
	if err != nil || len(got) != 1 || !strings.HasPrefix(got[0], filepath.Join(co, "sub")+": .claude/settings.json") || !strings.Contains(got[0], "re-opens "+b) {
		t.Fatalf("subfolder entry: %v, %v", got, err)
	}
	// orch's own allowRead (the default gh folder) is not counted, a user's
	// entry in the same file is.
	writeFile(t, filepath.Join(co, "sub", ".claude", "settings.json"), `{}`)
	writeFile(t, localOf(co), strings.Replace(readFile(t, localOf(co)), `"allowRead": [`, `"allowRead": ["`+b+`",`, 1))
	if got, err := Reopened(s.StateFile, []string{co}); err != nil || len(got) != 1 || !strings.Contains(got[0], "settings.local.json") {
		t.Fatalf("user entry in settings.local.json: %v, %v\n%s", got, err, readFile(t, localOf(co)))
	}
	// A folder deeper than the walk goes still has its checkout's files
	// checked.
	far := filepath.Join(append([]string{co}, strings.Split(strings.Repeat("d/", maxReopenDepth+6), "/")...)...)
	if err := os.MkdirAll(far, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := Reopened(s.StateFile, []string{far}); len(got) != 1 {
		t.Errorf("deep folder: %v", got)
	}
	link := filepath.Join(w.home, "link")
	if err := os.Symlink(filepath.Join(w.code, "a"), link); err != nil {
		t.Fatal(err)
	}
	if got, _ := Reopened(s.StateFile, []string{filepath.Join(link, "app", "sub")}); len(got) != 1 {
		t.Errorf("through a link: %v", got)
	}
	// A subfolder that is gone, or a link, is skipped rather than refused.
	writeFile(t, localOf(co), strings.Replace(readFile(t, localOf(co)), `"`+b+`",`, "", 1))
	shared := filepath.Join(w.home, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(co, "docs")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(co, "gone", "deeper"), filepath.Join(co, "docs")} {
		if got, err := Reopened(s.StateFile, []string{d}); err != nil || len(got) != 0 {
			t.Errorf("%s: %v %v", d, got, err)
		}
	}
	for _, d := range []string{"code/a/app", filepath.Join(w.code, "a"), w.home} {
		if got, _ := Reopened(s.StateFile, []string{d}); len(got) != 0 {
			t.Errorf("%s: %v", d, got)
		}
	}
	// With the sandbox off orch's denies go, and so does the record's.
	if rep := s.Sync(ctx, w.res, Choice{Setting: spaces.SandboxOff}); len(rep.Problems) != 0 {
		t.Fatal(rep.Problems)
	}
	if got, err := Reopened(s.StateFile, []string{co}); err != nil || len(got) != 0 {
		t.Errorf("sandbox off: %v, %v", got, err)
	}
}

// TestReopenedRecord: a missing record is nothing to check; one that is a
// link, not a regular file or not JSON is an error, so the guard fails
// closed.
func TestReopenedRecord(t *testing.T) {
	dir := realDir(t)
	rec := filepath.Join(dir, "checkouts.json")
	if got, err := Reopened(rec, []string{dir}); err != nil || got != nil {
		t.Errorf("missing: %v, %v", got, err)
	}
	writeFile(t, filepath.Join(dir, "real.json"), `{"checkouts":{}}`)
	if err := os.Symlink(filepath.Join(dir, "real.json"), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := Reopened(rec, []string{dir}); err == nil {
		t.Error("a linked record was read")
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Reopened(rec, []string{dir}); err == nil {
		t.Error("a folder was read as the record")
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	writeFile(t, rec, `{"checkouts":`)
	if _, err := Reopened(rec, []string{dir}); err == nil {
		t.Error("a damaged record was read")
	}
}

// TestReopenedRealPath: a checkout recorded by a path through a link is
// matched by its recorded real path, without resolving it again.
func TestReopenedRealPath(t *testing.T) {
	dir := realDir(t)
	real := filepath.Join(dir, "real", "app")
	writeFile(t, filepath.Join(real, ".claude", "settings.json"), `{"sandbox":{"filesystem":{"allowRead":["/denied"]}}}`)
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(dir, "checkouts.json")
	writeFile(t, rec, `{"checkouts":{"`+filepath.Join(dir, "link", "app")+`":{"owned":{},"deny":["/denied"],"real":"`+real+`"}}}`)
	if got, err := Reopened(rec, []string{filepath.Join(real, "sub")}); err != nil || len(got) != 1 || !strings.Contains(got[0], "re-opens /denied") {
		t.Errorf("by its real path: %v %v", got, err)
	}
}

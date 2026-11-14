package spacesettings

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// TestLoosening (RC-215): an allowRead or allowWrite in a checkout's own
// settings.json, or a user entry in settings.local.json, that holds or lies
// in a folder orch denies is reported; orch's own entries, unrelated
// entries and files orch cannot read are not.
func TestLoosening(t *testing.T) {
	w := newWorld(t)
	co := gitRepo(t, filepath.Join(w.code, "a", "app"))
	b := filepath.Join(w.code, "b")
	deny := []string{b, filepath.Join(w.home, "gh", "b")}
	if got := Loosening(co, deny, Owned{}); len(got) != 0 {
		t.Fatalf("no files: %v", got)
	}
	writeFile(t, filepath.Join(co, ".claude", "settings.json"),
		`{"sandbox":{"filesystem":{"allowRead":["/Users", "~/gh/*", "rel/dir", 3],"allowWrite":["//`+b+`/x"]}}}`)
	writeFile(t, localOf(co), `{"sandbox":{"filesystem":{"allowRead":["`+w.home+`/gh/b", "/opt/tools", "/tmp/orch-own"]}}}`)
	got := Loosening(co, deny, Owned{Lists: map[string][]string{keyAllowRead: {"/tmp/orch-own"}}})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		`.claude/settings.json: sandbox.filesystem.allowRead entry "~/gh/*" re-opens ` + filepath.Join(w.home, "gh", "b"),
		`.claude/settings.json: sandbox.filesystem.allowWrite entry "//` + b + `/x" re-opens ` + b,
		`.claude/settings.local.json: sandbox.filesystem.allowRead entry "` + w.home + `/gh/b" re-opens`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("lacks %q:\n%s", want, joined)
		}
	}
	for _, not := range []string{"/opt/tools", "rel/dir", "/tmp/orch-own"} {
		if strings.Contains(joined, `"`+not+`"`) {
			t.Errorf("reported %s:\n%s", not, joined)
		}
	}
	// "/Users" holds the temp home only on a Mac whose temp folder is there.
	if spaces.RootHolds("/Users", b) != strings.Contains(joined, `"/Users"`) {
		t.Errorf("/Users:\n%s", joined)
	}

	// A relative entry is read from the checkout; a linked .claude, a
	// file that is not JSON or not a regular file are skipped.
	other := gitRepo(t, filepath.Join(w.code, "a", "lib"))
	writeFile(t, filepath.Join(other, ".claude", "settings.json"), `{"sandbox":{"filesystem":{"allowRead":["../../b"]}}}`)
	if got := Loosening(other, deny, Owned{}); len(got) != 1 {
		t.Errorf("relative entry: %v", got)
	}
	writeFile(t, filepath.Join(other, ".claude", "settings.json"), `not json`)
	if got := Loosening(other, deny, Owned{}); len(got) != 0 {
		t.Errorf("not JSON: %v", got)
	}
	linked := gitRepo(t, filepath.Join(w.code, "a", "linked"))
	if err := os.Symlink(filepath.Join(co, ".claude"), filepath.Join(linked, ".claude")); err != nil {
		t.Fatal(err)
	}
	if got := Loosening(linked, deny, Owned{}); len(got) != 0 {
		t.Errorf("linked .claude read: %v", got)
	}

	// The sync reports them for a checkout it writes rules for.
	res, err := spaces.Validate(spaces.Policy{Spaces: []spaces.Space{
		{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}},
		{Name: "jobb", Kind: spaces.Isolated, Roots: []string{"~/code/b"}},
	}}, spaces.Env{Home: w.home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	rep := w.syncer().Sync(context.Background(), res, ChoiceOn)
	if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.HasPrefix(p, co+": .claude/settings.json") }) {
		t.Errorf("sync problems: %v", rep.Problems)
	}
	if rep := w.syncer().Sync(context.Background(), res, Choice{Setting: spaces.SandboxOff}); slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "re-opens") }) {
		t.Errorf("reported without rules: %v", rep.Problems)
	}
}

// TestAllowedReadsDropsEveryForm (security audit lows): an allowRead whose
// link form or real form holds a denied folder is dropped in both forms,
// and one holding a pattern character is never written.
func TestAllowedReadsDropsEveryForm(t *testing.T) {
	home := realDir(t)
	real := filepath.Join(home, "dotfiles", "config")
	if err := os.MkdirAll(filepath.Join(real, "gh", "jobb"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Job b's profile is named by its real path; the default folder by the link.
	if got := allowedReads([]string{filepath.Join(link, "gh")}, []string{filepath.Join(real, "gh", "jobb")}); len(got) != 0 {
		t.Errorf("link form kept: %v", got)
	}
	if got := allowedReads([]string{filepath.Join(home, "gh-*")}, nil); len(got) != 0 {
		t.Errorf("pattern kept: %v", got)
	}
	if got := allowedReads([]string{filepath.Join(link, "gh")}, []string{filepath.Join(home, "other")}); !slices.Equal(got, []string{filepath.Join(link, "gh"), filepath.Join(real, "gh")}) {
		t.Errorf("both forms: %v", got)
	}
}

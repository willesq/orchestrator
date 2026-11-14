package spaces

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestHarnessLookups: the lookups per-space harness settings use list every
// expanded root with its space, every space name and each gh profile.
func TestHarnessLookups(t *testing.T) {
	p := Policy{Spaces: []Space{
		{Name: "oss", Kind: Public, Roots: []string{"~/code"}},
		{Name: "joba", Kind: Isolated, Roots: []string{"~/code/a", "/srv/a"}, GHConfigDir: "~/gh/a"},
	}}
	res, err := Validate(p, Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	want := []RootMatch{
		{Root: "/home/u/code", Space: "oss", Kind: Public},
		{Root: "/home/u/code/a", Space: "joba", Kind: Isolated},
		{Root: "/srv/a", Space: "joba", Kind: Isolated},
	}
	if got := res.BoundRoots(); !slices.Equal(got, want) {
		t.Errorf("BoundRoots = %v", got)
	}
	if got := res.Names(); !slices.Equal(got, []string{"joba", "oss"}) {
		t.Errorf("Names = %v", got)
	}
	if res.GHConfigDir("joba") != "/home/u/gh/a" || res.GHConfigDir("oss") != "" {
		t.Errorf("GHConfigDir = %q, %q", res.GHConfigDir("joba"), res.GHConfigDir("oss"))
	}
	var none *Resolved
	if none.BoundRoots() != nil || none.Names() != nil || none.GHConfigDir("joba") != "" {
		t.Error("no policy has lookups")
	}
	for _, c := range []struct {
		root, path string
		want       bool
	}{
		{"/home/u/code", "/home/u/code", true},
		{"/home/u/code", "/home/u/Code/a", true},
		{"/home/u/code/", "/home/u/code/a", true},
		{"/home/u/code", "/home/u/codex", false},
		{"/home/u/code/a", "/home/u/code", false},
	} {
		if got := RootHolds(c.root, c.path); got != c.want {
			t.Errorf("RootHolds(%q, %q) = %v", c.root, c.path, got)
		}
	}
}

// TestClaudeConfigDir (D-59): a space may name its own Claude Code folder
// for 'orch run'; it is expanded and looked up like gh_config_dir, must be
// absolute, outside every git tree and the space's own, and a change shows
// in Compare. A space naming none, or no policy, has "".
func TestClaudeConfigDir(t *testing.T) {
	p := Policy{Spaces: []Space{
		{Name: "joba", Kind: Isolated, Roots: []string{"~/code/a"}, ClaudeConfigDir: "~/claude-a"},
		{Name: "jobb", Kind: Isolated, Roots: []string{"~/code/b"}},
	}}
	res := mustValid(t, p, testEnv())
	if res.ClaudeConfigDir("joba") != "/home/u/claude-a" || res.ClaudeConfigDir("jobb") != "" {
		t.Errorf("ClaudeConfigDir = %q, %q", res.ClaudeConfigDir("joba"), res.ClaudeConfigDir("jobb"))
	}
	var none *Resolved
	if none.ClaudeConfigDir("joba") != "" {
		t.Error("no policy names a Claude folder")
	}
	inRepo := testEnv()
	inRepo.InGitTree = func(path string) bool { return strings.HasPrefix(path, "/home/u/code/") }
	for _, tc := range []struct {
		name string
		p    Policy
		env  Env
		rule Rule
	}{
		{"relative", Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, ClaudeConfigDir: "claude-acme"}}}, testEnv(), RuleBadBinding},
		{"in a git tree", Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, ClaudeConfigDir: "~/code/acme/.claude"}}}, inRepo, RuleClaudeDirInGitTree},
		{"shared", Policy{Spaces: []Space{
			{Name: "acme", Kind: Isolated, ClaudeConfigDir: "~/claude-work"},
			{Name: "globex", Kind: Isolated, ClaudeConfigDir: "/HOME/u/claude-work/"},
		}}, testEnv(), RuleClaudeDirShared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := mustProblems(t, tc.p, tc.env)
			if len(ps) != 1 || ps[0].Rule != tc.rule {
				t.Fatalf("problems = %+v, want one %s", ps, tc.rule)
			}
		})
	}
	to := Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, ClaudeConfigDir: "/c/other"}}}
	got := Compare(Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, ClaudeConfigDir: "/c/acme"}}}, to)
	if len(got) != 1 || !slices.Equal(got[0].Fields, []string{"claude_config_dir"}) ||
		!slices.Equal(got[0].Details, []string{`claude_config_dir: "/c/acme" -> "/c/other"`}) {
		t.Errorf("Compare = %+v", got)
	}
	big := Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, ClaudeConfigDir: "/" + strings.Repeat("x/", maxDepth+1)}}}
	doc, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(doc); err == nil {
		t.Error("Parse took a claude_config_dir deeper than the limit")
	}
}

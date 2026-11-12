package spaces

import (
	"slices"
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

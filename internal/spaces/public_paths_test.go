package spaces

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// publicPathsPolicy is placementPolicy with public_paths on oss.
func publicPathsPolicy(paths ...string) Policy {
	p := placementPolicy
	p.Spaces = slices.Clone(p.Spaces)
	p.Spaces[0].PublicPaths = paths
	return p
}

// TestPublicPathPatterns is SP-076.1.a1: an exact folder matches only the
// files directly in it, '*' one folder name, '**' any depth (zero or more
// folders); matching ignores case, as roots do.
func TestPublicPathPatterns(t *testing.T) {
	const h = "/home/u/code"
	for _, c := range []struct {
		pattern, file string
		want          bool
	}{
		// An exact folder: its direct files only.
		{"~/code/x", h + "/x/README.md", true},
		{"~/code/x", h + "/x/docs/a.md", false},
		{"~/code/x", h + "/README.md", false},
		{"~/code/x", h + "/xy/a.md", false},
		{"/home/u/code/x", h + "/x/a.md", true},
		{"~/code/X", h + "/x/a.md", true},
		{"~/code", h + "/a.md", true},
		{"~/code", h + "/x/a.md", false},
		// '*': exactly one folder name, whole or within a name.
		{"~/code/*", h + "/x/a.md", true},
		{"~/code/*", h + "/a.md", false},
		{"~/code/*", h + "/x/y/a.md", false},
		{"~/code/*/docs", h + "/x/docs/a.md", true},
		{"~/code/*/docs", h + "/x/y/docs/a.md", false},
		{"~/code/oss-*", h + "/oss-lib/a.md", true},
		{"~/code/oss-*", h + "/lib/a.md", false},
		{"~/code/oss-*", h + "/oss-lib/sub/a.md", false},
		// '**': any depth, including none.
		{"~/code/x/**", h + "/x/a.md", true},
		{"~/code/x/**", h + "/x/docs/a.md", true},
		{"~/code/x/**", h + "/x/a/b/c/d.md", true},
		{"~/code/x/**", h + "/y/a.md", false},
		{"~/code/x/**", h + "/xy/a.md", false},
		{"~/code/**/docs", h + "/docs/a.md", true},
		{"~/code/**/docs", h + "/a/b/docs/a.md", true},
		{"~/code/**/docs", h + "/a/b/docs/c/a.md", false},
		{"~/code/**/docs/**", h + "/a/docs/c/a.md", true},
		{"~/code/**/*/**", h + "/a.md", false},
		{"~/code/**/*/**", h + "/a/a.md", true},
	} {
		res := resolvePlacement(t, publicPathsPolicy(c.pattern))
		got, ok := res.PublicPathFor("oss", c.file, false)
		if ok != c.want || (ok && got != c.pattern) {
			t.Errorf("%q against %s = %q %v, want %v", c.pattern, c.file, got, ok, c.want)
		}
	}
}

// TestPublicPathForFolders: a folder is matched as the folder the files
// directly in it share; only the named space's patterns count; the first
// matching pattern is named.
func TestPublicPathForFolders(t *testing.T) {
	res := resolvePlacement(t, publicPathsPolicy("~/code/x", "~/code/x/**"))
	if got, ok := res.PublicPathFor("oss", "/home/u/code/x", true); !ok || got != "~/code/x" {
		t.Errorf("folder x = %q %v", got, ok)
	}
	if got, ok := res.PublicPathFor("oss", "/home/u/code/x/docs", true); !ok || got != "~/code/x/**" {
		t.Errorf("folder x/docs = %q %v", got, ok)
	}
	if _, ok := res.PublicPathFor("oss", "/home/u/code", true); ok {
		t.Error("the root folder matched")
	}
	for _, c := range []struct{ space, path string }{
		{"personal", "/home/u/code/x/a.md"},
		{"oss", "relative/x/a.md"},
		{"oss", ""},
	} {
		if _, ok := res.PublicPathFor(c.space, c.path, false); ok {
			t.Errorf("%s %q matched", c.space, c.path)
		}
	}
	if _, ok := (*Resolved)(nil).PublicPathFor("oss", "/home/u/code/x/a.md", false); ok {
		t.Error("a nil policy matched")
	}
}

// TestPublicPathRejectedSyntax is SP-076.1.a1's other half: '!' and every
// other gitignore or glob syntax is refused, with a reason that never
// repeats the pattern.
func TestPublicPathRejectedSyntax(t *testing.T) {
	for _, pattern := range []string{
		"",
		"!~/code/x",
		"~/code/!x",
		"~/code/x?",
		"~/code/[ab]",
		"~/code/{a,b}",
		`~/code/a\*`,
		"~/code/x**",
		"~/code/***",
		"~/code/x/",
		"~/code//x",
		"~/code/./x",
		"~/code/../code/x",
		"code/x",
		"*/code",
		"~user/code",
	} {
		if _, why := parsePublicPath(pattern, "/home/u"); why == "" {
			t.Errorf("%q was accepted", pattern)
		} else if pattern != "" && strings.Contains(why, pattern) {
			t.Errorf("%q: reason %q repeats it", pattern, why)
		}
	}
	if _, why := parsePublicPath("~/code", ""); why == "" {
		t.Error("~ with no home was accepted")
	}
	for _, pattern := range []string{"~", "~/", "/", "~/code/x", "~/code/*/x", "~/code/**", "/srv/a-*-b/**/c"} {
		if _, why := parsePublicPath(pattern, "/home/u"); why != "" {
			t.Errorf("%q refused: %s", pattern, why)
		}
	}
	// The home folder's own name is literal, even holding a '*'.
	pp, _ := parsePublicPath("~/x", "/home/a*")
	if pp.matchesDir([]string{"home", "ab", "x"}) || !pp.matchesDir([]string{"home", "a*", "x"}) {
		t.Error("a '*' in the home folder's name was a wildcard")
	}
}

// TestValidatePublicPaths is SP-076.1.a2 in Validate: a pattern outside
// every root of its public space, shadowed by a deeper root of another
// space, malformed, or using '!' is refused, as is public_paths on a space
// that is not public; no message repeats a pattern, so none can echo a term
// a folder name holds.
func TestValidatePublicPaths(t *testing.T) {
	const term = "acmecorp-merger"
	for _, c := range []struct {
		name  string
		paths []string
		rule  Rule
	}{
		{"outside every root", []string{"~/work/" + term}, RulePublicPathOutsideRoot},
		{"above its root", []string{"~/*/" + term}, RulePublicPathOutsideRoot},
		{"inside a private root", []string{"~/code/willesq/" + term}, RulePublicPathOutsideRoot},
		{"inside an isolated root", []string{"~/code/company1/" + term + "/**"}, RulePublicPathOutsideRoot},
		{"malformed", []string{"~/code/" + term + "/"}, RuleBadPublicPath},
		{"negation", []string{"!~/code/" + term}, RuleBadPublicPath},
		{"glob class", []string{"~/code/[a]" + term}, RuleBadPublicPath},
		{"relative", []string{term + "/**"}, RuleBadPublicPath},
	} {
		ps := mustProblems(t, publicPathsPolicy(append([]string{"~/code/ok"}, c.paths...)...), testEnv())
		if !ps.Has(c.rule) || len(ps) != 1 {
			t.Errorf("%s: problems %v, want only %s", c.name, ps, c.rule)
		}
		if strings.Contains(ps.Error(), term) {
			t.Errorf("%s: %q echoes the pattern", c.name, ps.Error())
		}
		if !strings.Contains(ps.Error(), "entry 2") {
			t.Errorf("%s: %q does not name the entry", c.name, ps.Error())
		}
	}
	p := publicPathsPolicy()
	p.Spaces[1].PublicPaths = []string{"~/code/willesq/x"}
	if ps := mustProblems(t, p, testEnv()); !ps.Has(RuleBadPublicPath) {
		t.Errorf("public_paths on a private space: %v", ps)
	}
	// A pattern at its root, a '**' reaching over a stricter root, and one
	// in a second public space's own root are fine.
	p = publicPathsPolicy("~/code", "~/code/**")
	p.Spaces = append(p.Spaces, Space{Name: "docs", Kind: Public, Roots: []string{"~/code/site"}, PublicPaths: []string{"~/code/site/**"}})
	mustValid(t, p, testEnv())
	// But not one of oss's in the other public space's root.
	p.Spaces[0].PublicPaths = []string{"~/code/site/x"}
	if ps := mustProblems(t, p, testEnv()); !ps.Has(RulePublicPathOutsideRoot) {
		t.Errorf("a pattern in another public space's root: %v", ps)
	}
}

// TestPublicPathsLimits: public_paths count toward the policy's size limits.
func TestPublicPathsLimits(t *testing.T) {
	many := make([]string, maxPerList+1)
	for i := range many {
		many[i] = "~/code/x"
	}
	deep := "/" + strings.Repeat("a/", maxDepth) + "t"
	for name, paths := range map[string][]string{"too many": many, "too deep": {deep}, "too long": {"/" + strings.Repeat("a", maxString)}} {
		if ps := mustProblems(t, publicPathsPolicy(paths...), testEnv()); !ps.Has(RuleTooLarge) {
			t.Errorf("%s: %v", name, ps)
		}
	}
}

// TestApplyRefusesBadPublicPaths is SP-076.1.a2 at apply: a revision with a
// bad pattern is refused, the last applied policy stays enforced, and the
// error does not echo the pattern.
func TestApplyRefusesBadPublicPaths(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	good := publicPathsPolicy("~/code/lib/**")
	r, _, err := e.SaveDraft(ctx, good, "op", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, r.Number, "op", false, nil); err != nil {
		t.Fatalf("apply good: %v", err)
	}
	const term = "acmecorp-merger"
	for _, bad := range [][]string{{"!~/code/" + term}, {"~/elsewhere/" + term}, {"~/code/" + term + "?"}} {
		r, ps, err := e.SaveDraft(ctx, publicPathsPolicy(bad...), "op", true)
		if err != nil || len(ps) == 0 {
			t.Fatalf("%v: draft problems %v, %v", bad, ps, err)
		}
		_, err = e.Apply(ctx, r.Number, "op", false, nil)
		if err == nil {
			t.Fatalf("%v: applied", bad)
		}
		if strings.Contains(err.Error(), term) {
			t.Errorf("%v: apply error %q echoes the pattern", bad, err)
		}
		res, err := e.Current()
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := res.PublicPathFor("oss", "/home/u/code/lib/docs/a.md", false); !ok || got != "~/code/lib/**" {
			t.Errorf("%v: enforced policy changed: %q %v", bad, got, ok)
		}
	}
}

// TestClassifyPublicPaths is SP-076.1.a3 in the classifier: under the public
// root, local content is public (rule public_path) only when a pattern of
// its deepest root's public space matches; otherwise it is quarantined
// (no_public_proof). A matched checkout still needs its repo verified
// public, and a remote naming a stricter space's org does not move it. A
// stricter root nested inside a '**' pattern keeps its own space, and so
// does a second public space nested inside the first.
func TestClassifyPublicPaths(t *testing.T) {
	p := publicPathsPolicy("~/code/**")
	p.Spaces = append(p.Spaces, Space{Name: "site", Kind: Public, Roots: []string{"~/code/site"}})
	res := resolvePlacement(t, p)
	listed := resolvePlacement(t, publicPathsPolicy("~/code/lib", "~/code/oss-*/**"))
	const h = "/home/u/code"
	for _, c := range []struct {
		name    string
		res     *Resolved
		o       Origin
		space   string
		rule    PlaceRule
		pattern string
	}{
		{"plain folder, matched", res, Origin{System: SystemFS, Path: h + "/notes/a.md"}, "oss", PlacePublicPath, "~/code/**"},
		{"verified public checkout, matched", res, Origin{System: SystemFS, Path: h + "/lib/a.md", Remote: repo("oss-org", "lib", VisibilityPublic)}, "oss", PlacePublicPath, "~/code/**"},
		{"public repo of an unbound owner, matched", res, Origin{System: SystemFS, Path: h + "/x/a.md", Remote: repo("stranger", "x", VisibilityPublic)}, "oss", PlacePublicPath, "~/code/**"},
		{"private repo, matched", res, Origin{System: SystemFS, Path: h + "/s/a.md", Remote: repo("oss-org", "s", VisibilityPrivate)}, Quarantine, PlaceNotVerifiedPublic, "~/code/**"},
		{"unchecked repo, matched", res, Origin{System: SystemFS, Path: h + "/s/a.md", Remote: &Repo{Owner: "oss-org", Name: "s", Unchecked: true}}, Quarantine, PlaceNotVerifiedPublic, "~/code/**"},
		{"stricter remote, matched", res, Origin{System: SystemFS, Path: h + "/x/a.md", Remote: repo("acme", "x", VisibilityPrivate)}, Quarantine, PlaceNoPublicProof, "~/code/**"},
		{"private root inside **", res, Origin{System: SystemFS, Path: h + "/willesq/a.md"}, "personal", PlaceRoot, ""},
		{"isolated root inside **", res, Origin{System: SystemFS, Path: h + "/company1/x/a.md"}, "acme", PlaceRoot, ""},
		{"another public space's root inside **", res, Origin{System: SystemFS, Path: h + "/site/a.md"}, Quarantine, PlaceNoPublicProof, ""},
		{"session under a matched folder", res, Origin{System: SystemSession, Path: h + "/notes"}, "personal", PlacePublicRootSession, ""},
		{"exact folder, direct file", listed, Origin{System: SystemFS, Path: h + "/lib/a.md"}, "oss", PlacePublicPath, "~/code/lib"},
		{"exact folder, deeper file", listed, Origin{System: SystemFS, Path: h + "/lib/docs/a.md"}, Quarantine, PlaceNoPublicProof, ""},
		{"glob folder", listed, Origin{System: SystemFS, Path: h + "/oss-tool/docs/a.md"}, "oss", PlacePublicPath, "~/code/oss-*/**"},
		{"unlisted folder", listed, Origin{System: SystemFS, Path: h + "/notes/a.md"}, Quarantine, PlaceNoPublicProof, ""},
		{"folder named, as an explanation does", listed, Origin{System: SystemFS, Path: h + "/lib", IsDir: true}, "oss", PlacePublicPath, "~/code/lib"},
	} {
		got := Classify(c.o, c.res, Options{Confirmed: allConfirmed})
		if got.Space != c.space || got.Rule != c.rule || got.PublicPath != c.pattern {
			t.Errorf("%s = %s (%s, %q), want %s (%s, %q)", c.name, got.Space, got.Rule, got.PublicPath, c.space, c.rule, c.pattern)
		}
		if strings.Contains(got.Reason, "~/code") {
			t.Errorf("%s: reason %q names the pattern", c.name, got.Reason)
		}
	}
	// A retag still wins over a matched pattern.
	if got := Classify(Origin{System: SystemFS, Path: h + "/notes/a.md"}, res, Options{Override: "personal"}); got.Space != "personal" || got.Rule != PlaceOverride {
		t.Errorf("retag = %s (%s)", got.Space, got.Rule)
	}
}

// TestComparePublicPaths: a reviewer sees public_paths changes in the diff,
// since they decide what every session reads.
func TestComparePublicPaths(t *testing.T) {
	got := Compare(publicPathsPolicy("~/code/a"), publicPathsPolicy("~/code/b/**"))
	if len(got) != 1 || got[0].Space != "oss" || !slices.Equal(got[0].Fields, []string{"public_paths"}) ||
		got[0].Details[0] != `public_paths: -"~/code/a" +"~/code/b/**"` {
		t.Errorf("Compare = %+v", got)
	}
}

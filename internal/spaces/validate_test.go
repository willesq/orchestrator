package spaces

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv expands "~" to /home/u and treats nothing as a git tree.
func testEnv() Env {
	return Env{Home: "/home/u", InGitTree: func(string) bool { return false }}
}

// mustProblems validates p and returns its problems, failing if it passed.
func mustProblems(t *testing.T, p Policy, env Env) Problems {
	t.Helper()
	_, err := Validate(p, env)
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("Validate = %v, want Problems", err)
	}
	return ps
}

func mustValid(t *testing.T, p Policy, env Env) *Resolved {
	t.Helper()
	r, err := Validate(p, env)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return r
}

// TestValidateRules covers each rule of SP-020.1.a1 with a policy that breaks
// only that rule.
func TestValidateRules(t *testing.T) {
	cases := []struct {
		name string
		p    Policy
		env  Env
		rule Rule
	}{
		{
			name: "org bound twice",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"acme"}},
				{Name: "globex", Kind: Isolated, GitHubOrgs: []string{"ACME"}},
			}},
			rule: RuleOrgBoundTwice,
		},
		{
			name: "web prefix bound twice",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, WebPrefixes: []string{"https://wiki.acme.example/"}},
				{Name: "personal", Kind: Private, WebPrefixes: []string{"https://WIKI.acme.example/"}},
			}},
			rule: RulePrefixBoundTwice,
		},
		{
			name: "chat tenant bound twice",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, Chat: []string{"slack:T01"}},
				{Name: "globex", Kind: Isolated, Chat: []string{"slack:t01"}},
			}},
			rule: RuleTenantBoundTwice,
		},
		{
			name: "same root bound twice",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, Roots: []string{"~/code/acme"}},
				{Name: "globex", Kind: Isolated, Roots: []string{"/home/u/code/acme/"}},
			}},
			rule: RuleRootBoundTwice,
		},
		{
			name: "nested root less strict",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, Roots: []string{"~/code/acme"}},
				{Name: "oss", Kind: Public, Roots: []string{"~/code/acme/vendor-docs"}},
			}},
			rule: RuleNestedLessStrict,
		},
		{
			name: "isolated root inside another isolated space",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, Roots: []string{"~/code/acme"}},
				{Name: "globex", Kind: Isolated, Roots: []string{"~/code/acme/globex"}},
			}},
			rule: RuleIsolatedInIsolated,
		},
		{
			name: "isolated reads isolated",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, Reads: []string{"globex"}},
				{Name: "globex", Kind: Isolated},
			}},
			rule: RuleIsolatedReadsIsolated,
		},
		{
			name: "private reads isolated",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated},
				{Name: "personal", Kind: Private, Reads: []string{"acme"}},
			}},
			rule: RulePrivateReadsIsolated,
		},
		{
			name: "public reads another space",
			p: Policy{Spaces: []Space{
				{Name: "oss", Kind: Public, Reads: []string{"personal"}},
				{Name: "personal", Kind: Private},
			}},
			rule: RulePublicReads,
		},
		{
			name: "terms file inside a git tree",
			p: Policy{Spaces: []Space{
				{Name: "acme", Kind: Isolated, TermsFile: "~/code/acme/terms.txt"},
			}},
			env: Env{Home: "/home/u", InGitTree: func(p string) bool {
				return strings.HasPrefix(p, "/home/u/code/acme/")
			}},
			rule: RuleTermsInGitTree,
		},
		{
			name: "unknown kind",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: "secret"}}},
			rule: RuleUnknownKind,
		},
		{
			name: "unknown read",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, Reads: []string{"nope"}}}},
			rule: RuleUnknownRead,
		},
		{
			name: "duplicate name",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated}, {Name: "acme", Kind: Private}}},
			rule: RuleDuplicateName,
		},
		{
			name: "reserved name",
			p:    Policy{Spaces: []Space{{Name: Quarantine, Kind: Private}}},
			rule: RuleBadName,
		},
		{
			name: "bad name",
			p:    Policy{Spaces: []Space{{Name: "Acme Corp", Kind: Private}}},
			rule: RuleBadName,
		},
		{
			name: "relative root",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, Roots: []string{"code/acme"}}}},
			rule: RuleBadBinding,
		},
		{
			name: "relative terms file",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, TermsFile: "terms.txt"}}},
			rule: RuleBadBinding,
		},
		{
			name: "bad web prefix",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, WebPrefixes: []string{"ftp://x"}}}},
			rule: RuleBadBinding,
		},
		{
			name: "bad chat tenant",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, Chat: []string{"irc:x"}}}},
			rule: RuleBadBinding,
		},
		{
			name: "empty org",
			p:    Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GitHubOrgs: []string{" "}}}},
			rule: RuleBadBinding,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			if env.InGitTree == nil {
				env = testEnv()
			}
			ps := mustProblems(t, tc.p, env)
			if !ps.Has(tc.rule) {
				t.Fatalf("problems %v, want rule %s", ps, tc.rule)
			}
			for _, p := range ps {
				if p.Rule != tc.rule {
					t.Errorf("unexpected extra problem %s: %s", p.Rule, p.Message)
				}
			}
			if !strings.Contains(ps.Error(), "invalid spaces policy") {
				t.Errorf("error %q lacks its prefix", ps.Error())
			}
		})
	}
}

// TestValidateNestedRootsFixture is SP-020.1.a3: ~/code public,
// ~/code/willesq private, ~/code/company1 isolated.
func TestValidateNestedRootsFixture(t *testing.T) {
	mustValid(t, Policy{Spaces: []Space{
		{Name: "oss", Kind: Public, Roots: []string{"~/code"}},
		{Name: "personal", Kind: Private, Roots: []string{"~/code/willesq"}},
		{Name: "company1", Kind: Isolated, Roots: []string{"~/code/company1", "~/code/company1/sub"}},
	}}, testEnv())
}

// TestValidateSiblingPrefixIsNotNested guards the path-boundary check:
// ~/code/acme-tools is beside ~/code/acme, not inside it.
func TestValidateSiblingPrefixIsNotNested(t *testing.T) {
	mustValid(t, Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, Roots: []string{"~/code/acme"}},
		{Name: "globex", Kind: Isolated, Roots: []string{"~/code/acme-tools"}},
	}}, testEnv())
}

// TestIsolatedReadsPrivateWhenListed is SP-020.1.a4, both halves.
func TestIsolatedReadsPrivateWhenListed(t *testing.T) {
	t.Run("listed", func(t *testing.T) {
		r := mustValid(t, Policy{Spaces: []Space{
			{Name: "acme", Kind: Isolated, Reads: []string{"personal"}},
			{Name: "personal", Kind: Private},
		}}, testEnv())
		if !r.CanRead("acme", "personal") {
			t.Fatal("acme lists personal in reads but cannot read it")
		}
	})
	t.Run("not listed", func(t *testing.T) {
		r := mustValid(t, Policy{Spaces: []Space{
			{Name: "acme", Kind: Isolated},
			{Name: "personal", Kind: Private},
		}}, testEnv())
		if r.CanRead("acme", "personal") {
			t.Fatal("acme reads personal without listing it")
		}
	})
}

// TestPublicRootSessions is SP-020.1.a5.
func TestPublicRootSessions(t *testing.T) {
	t.Run("named private or isolated space", func(t *testing.T) {
		for _, target := range []string{"personal", "acme"} {
			r := mustValid(t, Policy{PublicRootSessions: target, Spaces: []Space{
				{Name: "acme", Kind: Isolated},
				{Name: "personal", Kind: Private},
				{Name: "oss", Kind: Public},
			}}, testEnv())
			if r.PublicRootSessions != target {
				t.Errorf("PublicRootSessions = %q, want %q", r.PublicRootSessions, target)
			}
		}
	})
	t.Run("public or unknown space refused", func(t *testing.T) {
		for _, target := range []string{"oss", "nope"} {
			ps := mustProblems(t, Policy{PublicRootSessions: target, Spaces: []Space{
				{Name: "oss", Kind: Public},
			}}, testEnv())
			if !ps.Has(RulePublicRootSessions) {
				t.Errorf("%s: problems %v, want %s", target, ps, RulePublicRootSessions)
			}
		}
	})
	t.Run("default", func(t *testing.T) {
		cases := []struct {
			name   string
			spaces []Space
			want   string
		}{
			{"one private", []Space{{Name: "acme", Kind: Isolated}, {Name: "personal", Kind: Private}}, "personal"},
			{"no private", []Space{{Name: "acme", Kind: Isolated}}, Quarantine},
			{"two private", []Space{{Name: "home", Kind: Private}, {Name: "personal", Kind: Private}}, Quarantine},
		}
		for _, tc := range cases {
			r := mustValid(t, Policy{Spaces: tc.spaces}, testEnv())
			if r.PublicRootSessions != tc.want {
				t.Errorf("%s: PublicRootSessions = %q, want %q", tc.name, r.PublicRootSessions, tc.want)
			}
		}
	})
}

func TestCanRead(t *testing.T) {
	r := mustValid(t, Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, Reads: []string{"personal"}},
		{Name: "globex", Kind: Isolated},
		{Name: "personal", Kind: Private, Reads: []string{"home"}},
		{Name: "home", Kind: Private},
		{Name: "oss", Kind: Public},
	}}, testEnv())
	cases := []struct {
		reader, target string
		want           bool
	}{
		{"acme", "acme", true},
		{"acme", "oss", true},
		{"acme", "personal", true},
		{"acme", "globex", false},
		{"globex", "acme", false},
		{"acme", "home", false}, // reads do not chain
		{"personal", "home", true},
		{"home", "personal", false},
		{"personal", "acme", false},
		{"oss", "personal", false},
		{"acme", Quarantine, false},
		{"nope", "oss", false},
		{"acme", "nope", false},
		{"acme", Shared, false},
	}
	for _, tc := range cases {
		if got := r.CanRead(tc.reader, tc.target); got != tc.want {
			t.Errorf("CanRead(%s, %s) = %v, want %v", tc.reader, tc.target, got, tc.want)
		}
	}
	if s, ok := r.Space("acme"); !ok || s.Kind != Isolated {
		t.Errorf("Space(acme) = %+v, %v", s, ok)
	}
	if _, ok := r.Space("nope"); ok {
		t.Error("Space(nope) found")
	}
}

// TestNoPolicyIsOneSharedSpace is SP-020.3.a1's unit half: with nothing
// applied, every caller is in Shared and reads all of it.
func TestNoPolicyIsOneSharedSpace(t *testing.T) {
	var none *Resolved
	if none.Configured() {
		t.Fatal("nil policy reports configured")
	}
	if !none.CanRead(Shared, Shared) {
		t.Fatal("nil policy: Shared cannot read Shared")
	}
	if _, ok := none.Space("x"); ok {
		t.Fatal("nil policy has a space")
	}
	empty := mustValid(t, Policy{}, testEnv())
	if empty.Configured() || !empty.CanRead(Shared, Shared) {
		t.Fatal("an empty policy must behave as no policy")
	}
	if empty.PublicRootSessions != Quarantine {
		t.Errorf("PublicRootSessions = %q", empty.PublicRootSessions)
	}
}

func TestDefaultEnvFindsGitTree(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := DefaultEnv()
	env.Home = dir
	if !env.inGitTree(filepath.Join(repo, "sub", "terms.txt")) {
		t.Error("file under a repo not seen as in a git tree")
	}
	if env.inGitTree(filepath.Join(dir, "outside", "terms.txt")) {
		t.Error("file outside any repo seen as in a git tree")
	}
	if got := env.expand("~"); got != dir {
		t.Errorf("expand(~) = %q", got)
	}
	if got := (Env{}).expand("~/x"); got != "" {
		t.Errorf("expand without home = %q", got)
	}
}

func TestParseAndCanonical(t *testing.T) {
	p, err := Parse([]byte(`{"spaces":[{"name":"acme","kind":"isolated","roots":["~/code/acme"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Canonical(mustParse(t, doc))
	if Hash(doc) != Hash(again) {
		t.Error("canonical form is not stable")
	}
	empty, _ := Canonical(Policy{})
	if string(empty) != `{"spaces":[]}` {
		t.Errorf("empty canonical = %s", empty)
	}
	for _, bad := range []string{
		`{"spaces":[{"name":"acme","kidn":"isolated"}]}`,
		`{"spaces":[]} {}`,
		`not json`,
		`{"spaces":[]}` + strings.Repeat(" ", maxDocBytes),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%.40q) succeeded", bad)
		}
	}
}

func mustParse(t *testing.T, doc []byte) Policy {
	t.Helper()
	p, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompare(t *testing.T) {
	from := Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, Roots: []string{"~/a"}},
		{Name: "gone", Kind: Private},
		{Name: "same", Kind: Public},
	}}
	to := Policy{PublicRootSessions: "acme", Spaces: []Space{
		{Name: "acme", Kind: Private, Roots: []string{"~/b"}, GitHubOrgs: []string{"acme"},
			WebPrefixes: []string{"https://x"}, Chat: []string{"slack:T1"}, Reads: []string{"same"}, TermsFile: "/t"},
		{Name: "new", Kind: Private},
		{Name: "same", Kind: Public},
	}}
	got := Compare(from, to)
	want := []string{
		`:changed:public_root_sessions:public_root_sessions: "" -> "acme"`,
		`acme:changed:kind,github_orgs,roots,web_prefixes,chat,reads,terms_file:` +
			`kind: "isolated" -> "private"|github_orgs: +"acme"|roots: -"~/a" +"~/b"|web_prefixes: +"https://x"|` +
			`chat: +"slack:T1"|reads: +"same"|terms_file: "" -> "/t"`,
		`gone:removed::kind: "private" -> ""`,
		`new:added::kind: "" -> "private"`,
	}
	if len(got) != len(want) {
		t.Fatalf("Compare = %+v", got)
	}
	for i, c := range got {
		if s := c.Space + ":" + c.Kind + ":" + strings.Join(c.Fields, ",") + ":" + strings.Join(c.Details, "|"); s != want[i] {
			t.Errorf("change %d = %s\nwant %s", i, s, want[i])
		}
	}
	reordered := Compare(Policy{Spaces: []Space{{Name: "a", Kind: Private, Roots: []string{"/x", "/y"}}}},
		Policy{Spaces: []Space{{Name: "a", Kind: Private, Roots: []string{"/y", "/x"}}}})
	if len(reordered) != 1 || reordered[0].Details[0] != "roots: reordered" {
		t.Errorf("reordered = %+v", reordered)
	}
	if len(Compare(from, from)) != 0 {
		t.Error("a policy differs from itself")
	}
}

func TestValidateLimits(t *testing.T) {
	tooMany := Policy{Spaces: make([]Space, maxSpaces+1)}
	if ps := mustProblems(t, tooMany, testEnv()); !ps.Has(RuleTooLarge) {
		t.Errorf("too many spaces: %v", ps)
	}
	longList := Policy{Spaces: []Space{{Name: "a", Kind: Private, GitHubOrgs: make([]string, maxPerList+1)}}}
	if ps := mustProblems(t, longList, testEnv()); !ps.Has(RuleTooLarge) {
		t.Errorf("long list: %v", ps)
	}
	var spaces []Space
	for i := 0; i < 3; i++ {
		roots := make([]string, maxPerList)
		for j := range roots {
			roots[j] = fmt.Sprintf("/r/%d/%d", i, j)
		}
		spaces = append(spaces, Space{Name: fmt.Sprintf("s%d", i), Kind: Private, Roots: roots})
	}
	if ps := mustProblems(t, Policy{Spaces: spaces}, testEnv()); !ps.Has(RuleTooLarge) {
		t.Errorf("too many roots: %v", ps)
	}
	for _, doc := range []string{
		fmt.Sprintf(`{"spaces":[%s{"name":"x","kind":"private"}]}`, strings.Repeat(`{"name":"x","kind":"private"},`, maxSpaces)),
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Error("Parse accepted an oversized policy")
		}
	}
}

// TestValidateManyNestedRootsIsBounded: duplicate and nested roots that once
// produced millions of problems stay within maxProblems.
func TestValidateManyNestedRootsIsBounded(t *testing.T) {
	outer := make([]string, maxPerList)
	inner := make([]string, maxPerList)
	for i := range outer {
		outer[i], inner[i] = "/a", "/a/b"
	}
	ps := mustProblems(t, Policy{Spaces: []Space{
		{Name: "p", Kind: Private, Roots: outer},
		{Name: "q", Kind: Public, Roots: inner},
	}}, testEnv())
	if len(ps) > maxProblems || !ps.Has(RuleRootBoundTwice) {
		t.Fatalf("%d problems", len(ps))
	}
}

func TestInGitTreeFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "terms"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "elsewhere")
	if err := os.Symlink(filepath.Join(repo, "terms"), link); err != nil {
		t.Fatal(err)
	}
	if !(Env{}).inGitTree(filepath.Join(link, "acme.txt")) {
		t.Error("a terms file reached through a symlink into a repo was not seen")
	}
}

func TestProblemMessagesQuotePaths(t *testing.T) {
	ps := mustProblems(t, Policy{Spaces: []Space{
		{Name: "a", Kind: Isolated, Roots: []string{"/x\x1b[31m"}},
		{Name: "b", Kind: Isolated, Roots: []string{"/x\x1b[31m/y"}},
	}}, testEnv())
	if strings.Contains(ps.Error(), "\x1b") {
		t.Errorf("control character in %q", ps.Error())
	}
}

// TestDeepOrLongPathsRefused: path checks walk parents, so a path's depth and
// every string's length are bounded before anything is validated or saved.
func TestDeepOrLongPathsRefused(t *testing.T) {
	deep := "/" + strings.Repeat("a/", maxDepth) + "t"
	long := "/" + strings.Repeat("a", maxString)
	for name, p := range map[string]Policy{
		"deep terms file":      {Spaces: []Space{{Name: "a", Kind: Isolated, TermsFile: deep}}},
		"deep root":            {Spaces: []Space{{Name: "a", Kind: Isolated, Roots: []string{deep}}}},
		"long org":             {Spaces: []Space{{Name: "a", Kind: Isolated, GitHubOrgs: []string{long}}}},
		"long terms file":      {Spaces: []Space{{Name: "a", Kind: Isolated, TermsFile: long}}},
		"long name":            {Spaces: []Space{{Name: long, Kind: Isolated}}},
		"long public sessions": {PublicRootSessions: long},
	} {
		doc, _ := Canonical(p)
		if _, err := Parse(doc); err == nil {
			t.Errorf("%s: Parse accepted it", name)
		}
		if ps := mustProblems(t, p, testEnv()); !ps.Has(RuleTooLarge) {
			t.Errorf("%s: problems %v", name, ps)
		}
	}
	ok := Policy{Spaces: []Space{{Name: "a", Kind: Isolated, TermsFile: "/" + strings.Repeat("a/", maxDepth-1) + "t"}}}
	mustValid(t, ok, testEnv())
}

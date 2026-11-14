package spaces

import (
	"context"
	"math/rand"
	"slices"
	"testing"
)

// placementPolicy is the layout the plan's examples use: ~/code public,
// ~/code/willesq private, ~/code/company1 and ~/code/globex isolated.
var placementPolicy = Policy{
	Spaces: []Space{
		{Name: "oss", Kind: Public, GitHubOrgs: []string{"oss-org"}, Roots: []string{"~/code"}, WebPrefixes: []string{"https://docs.oss.dev/"}},
		{Name: "personal", Kind: Private, GitHubOrgs: []string{"willesq"}, Roots: []string{"~/code/willesq"}},
		{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"acme"}, Roots: []string{"~/code/company1"}, Chat: []string{"slack:TACME"}, Reads: []string{"personal"}},
		{Name: "globex", Kind: Isolated, GitHubOrgs: []string{"globex"}, Roots: []string{"~/code/globex"}, Chat: []string{"teams:globex-tenant"}, WebPrefixes: []string{"https://wiki.globex.example/"}},
	},
	PublicRootSessions: "personal",
}

func resolvePlacement(t *testing.T, p Policy) *Resolved {
	t.Helper()
	res, err := Validate(p, testEnv())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return res
}

func allConfirmed(string) bool { return true }

func repo(owner, name string, v Visibility) *Repo {
	return &Repo{Owner: owner, Name: name, Visibility: v}
}

// Local content under the broad public root ~/code is quarantined
// (no_public_proof, SP-030, D-64); the matching roots still show.
func TestClassifyCases(t *testing.T) {
	res := resolvePlacement(t, placementPolicy)
	const h = "/home/u/code"
	cases := []struct {
		name   string
		o      Origin
		opt    Options
		space  string
		rule   PlaceRule
		roots  []string // spaces of the matching roots, shallowest first
		readAs string
	}{
		{name: "repo in isolated org", o: Origin{System: SystemFS, Remote: repo("acme", "app", VisibilityPrivate)}, space: "acme", rule: PlaceRemote},
		{name: "fork owned by job org with public parent", o: Origin{System: SystemFS, Path: h + "/company1/fork", Remote: repo("acme", "oss-lib-fork", VisibilityPublic)}, space: "acme", rule: PlaceAgree, roots: []string{"oss", "acme"}},
		{name: "public repo in public org", o: Origin{System: SystemFS, Path: h + "/lib", Remote: repo("oss-org", "lib", VisibilityPublic)}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "private repo in public org", o: Origin{System: SystemFS, Path: h + "/secret", Remote: repo("oss-org", "secret", VisibilityPrivate)}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "public org repo with unchecked visibility", o: Origin{System: SystemGitHub, Remote: repo("oss-org", "lib", VisibilityUnknown)}, space: Quarantine, rule: PlaceNotVerifiedPublic},
		{name: "private repo of public org under private root", o: Origin{System: SystemFS, Path: h + "/willesq/secret", Remote: repo("oss-org", "secret", VisibilityPrivate)}, space: "personal", rule: PlaceRootStricter, roots: []string{"oss", "personal"}},
		{name: "repo transferred from public org to job org", o: Origin{System: SystemFS, Path: h + "/lib", Remote: repo("acme", "lib", VisibilityPrivate)}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "repo transferred between job orgs", o: Origin{System: SystemFS, Path: h + "/company1/app", Remote: repo("globex", "app", VisibilityPrivate)}, space: Quarantine, rule: PlaceConflict, roots: []string{"oss", "acme"}},
		{name: "root and remote agree", o: Origin{System: SystemFS, Path: h + "/company1/app", Remote: repo("acme", "app", VisibilityPrivate)}, space: "acme", rule: PlaceAgree, roots: []string{"oss", "acme"}},
		{name: "remote stricter than root", o: Origin{System: SystemFS, Path: h + "/willesq/app", Remote: repo("acme", "app", VisibilityPrivate)}, space: "acme", rule: PlaceRemoteStricter, roots: []string{"oss", "personal"}},
		{name: "root stricter than remote", o: Origin{System: SystemFS, Path: h + "/company1/lib", Remote: repo("oss-org", "lib", VisibilityPublic)}, space: "acme", rule: PlaceRootStricter, roots: []string{"oss", "acme"}},
		{name: "root and remote in two isolated spaces", o: Origin{System: SystemFS, Path: h + "/company1/x", Remote: repo("globex", "x", VisibilityPrivate)}, space: Quarantine, rule: PlaceConflict, roots: []string{"oss", "acme"}},
		{name: "nested: public root", o: Origin{System: SystemFS, Path: h}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "nested: under public root", o: Origin{System: SystemFS, Path: h + "/tools/a.go"}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "nested: private root", o: Origin{System: SystemFS, Path: h + "/willesq"}, space: "personal", rule: PlaceRoot, roots: []string{"oss", "personal"}},
		{name: "nested: under private root", o: Origin{System: SystemFS, Path: h + "/willesq/orch/main.go"}, space: "personal", rule: PlaceRoot, roots: []string{"oss", "personal"}},
		{name: "nested: isolated root", o: Origin{System: SystemFS, Path: h + "/company1"}, space: "acme", rule: PlaceRoot, roots: []string{"oss", "acme"}},
		{name: "nested: under isolated root", o: Origin{System: SystemFS, Path: h + "/company1/svc/deep/x.go"}, space: "acme", rule: PlaceRoot, roots: []string{"oss", "acme"}},
		{name: "nested: case differs", o: Origin{System: SystemFS, Path: "/home/u/Code/Company1/x"}, space: "acme", rule: PlaceRoot, roots: []string{"oss", "acme"}},
		{name: "nested: sibling sharing a prefix is not under the root", o: Origin{System: SystemFS, Path: h + "/company10/x"}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "remote only, no root", o: Origin{System: SystemFS, Path: "/srv/notes", Remote: repo("willesq", "notes", VisibilityPrivate)}, space: "personal", rule: PlaceRemote},
		{name: "docs sync ignores any path", o: Origin{System: SystemGitHub, Path: h + "/willesq/x", Remote: repo("acme", "docs", VisibilityPrivate)}, space: "acme", rule: PlaceRemote},
		{name: "no root and no remote", o: Origin{System: SystemFS, Path: "/tmp/scratch"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "relative path matches no root", o: Origin{System: SystemFS, Path: "code/company1"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "unbound org outside roots", o: Origin{System: SystemGitHub, Remote: repo("stranger", "x", VisibilityPublic)}, space: Quarantine, rule: PlaceUnmatched},
		{name: "unbound private repo under public root", o: Origin{System: SystemFS, Path: h + "/x", Remote: repo("stranger", "x", VisibilityPrivate)}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "unbound repo of unchecked visibility under public root", o: Origin{System: SystemFS, Path: h + "/x", Remote: repo("stranger", "x", VisibilityUnknown)}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "unbound public repo under public root", o: Origin{System: SystemFS, Path: h + "/x", Remote: repo("stranger", "x", VisibilityPublic)}, space: Quarantine, rule: PlaceNoPublicProof, roots: []string{"oss"}},
		{name: "dot segments cannot climb above a site's root", o: Origin{System: SystemWeb, URL: "https://docs.oss.dev/x/../../../wiki/a", WriterSpace: "acme"}, space: "acme", rule: PlaceWriterStricter},
		{name: "dot segments resolve into a prefix", o: Origin{System: SystemWeb, URL: "https://wiki.globex.example/a/../b"}, space: "globex", rule: PlaceWebPrefix},
		{name: "encoded path resolves into a prefix", o: Origin{System: SystemWeb, URL: "https://wiki.globex.example/%61"}, space: "globex", rule: PlaceWebPrefix},
		{name: "default port and trailing dot match a prefix", o: Origin{System: SystemWeb, URL: "https://Wiki.Globex.Example.:443/a"}, space: "globex", rule: PlaceWebPrefix},
		{name: "other port is another site", o: Origin{System: SystemWeb, URL: "https://wiki.globex.example:8443/a"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "unconfirmed org binding", o: Origin{System: SystemFS, Path: h + "/company1/x", Remote: repo("acme", "x", VisibilityPrivate)}, opt: Options{Confirmed: func(string) bool { return false }}, space: Quarantine, rule: PlaceOrgUnconfirmed, roots: []string{"oss", "acme"}},
		{name: "allowlisted web prefix", o: Origin{System: SystemWeb, URL: "https://docs.oss.dev/guide/start"}, space: "oss", rule: PlaceWebPrefix},
		{name: "host sharing a prefix's text is not under it", o: Origin{System: SystemWeb, URL: "https://docs.oss.dev.evil.example/guide"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "unbound web page", o: Origin{System: SystemWeb, URL: "https://example.com/x"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "web page ingested by a session", o: Origin{System: SystemWeb, URL: "https://example.com/x", WriterSpace: "acme"}, space: "acme", rule: PlaceWriter},
		{name: "public page ingested by a private session", o: Origin{System: SystemWeb, URL: "https://docs.oss.dev/a", WriterSpace: "personal"}, space: "personal", rule: PlaceWriterStricter},
		{name: "job page ingested by a private session", o: Origin{System: SystemWeb, URL: "https://wiki.globex.example/a", WriterSpace: "personal"}, space: "globex", rule: PlacePrefixStricter},
		{name: "job page ingested by another job's session", o: Origin{System: SystemWeb, URL: "https://wiki.globex.example/a", WriterSpace: "acme"}, space: Quarantine, rule: PlaceConflict},
		{name: "page from a pin naming a public space", o: Origin{System: SystemWeb, URL: "https://example.com/x", WriterSpace: "oss"}, space: Quarantine, rule: PlaceWriterUnknown},
		{name: "Slack tenant", o: Origin{System: SystemSlack, Tenant: "tacme"}, space: "acme", rule: PlaceChatTenant},
		{name: "Teams tenant", o: Origin{System: SystemTeams, Tenant: "globex-tenant"}, space: "globex", rule: PlaceChatTenant},
		{name: "unbound Slack tenant", o: Origin{System: SystemSlack, Tenant: "TOTHER"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "Teams ID bound only for Slack", o: Origin{System: SystemTeams, Tenant: "TACME"}, space: Quarantine, rule: PlaceUnmatched},
		{name: "client ingest from a pinned session", o: Origin{System: SystemClient, WriterSpace: "acme"}, space: "acme", rule: PlaceWriter},
		{name: "client ingest with no session", o: Origin{System: SystemClient}, space: Quarantine, rule: PlaceUnmatched},
		{name: "client ingest from a stale pin", o: Origin{System: SystemClient, WriterSpace: "gone"}, space: Quarantine, rule: PlaceWriterUnknown},
		{name: "sticky retag override", o: Origin{System: SystemFS, Path: h + "/company1/x"}, opt: Options{Override: "personal"}, space: "personal", rule: PlaceOverride, roots: []string{"oss", "acme"}},
		{name: "override wins over a conflict", o: Origin{System: SystemFS, Path: h + "/company1/x", Remote: repo("globex", "x", VisibilityPrivate)}, opt: Options{Override: "globex"}, space: "globex", rule: PlaceOverride, roots: []string{"oss", "acme"}},
		{name: "override to quarantine", o: Origin{System: SystemFS, Path: h + "/lib"}, opt: Options{Override: Quarantine}, space: Quarantine, rule: PlaceOverride, roots: []string{"oss"}},
		{name: "override to a removed space", o: Origin{System: SystemFS, Path: h + "/lib"}, opt: Options{Override: "gone"}, space: Quarantine, rule: PlaceOverrideUnknown, roots: []string{"oss"}},
		{name: "session under an isolated root", o: Origin{System: SystemSession, Path: h + "/company1/app"}, space: "acme", rule: PlaceRoot, roots: []string{"oss", "acme"}, readAs: "acme"},
		{name: "session under a public root", o: Origin{System: SystemSession, Path: h + "/lib"}, space: "personal", rule: PlacePublicRootSession, roots: []string{"oss"}, readAs: "oss"},
		{name: "session in a conflicting checkout", o: Origin{System: SystemSession, Path: h + "/company1/x", Remote: repo("globex", "x", VisibilityPrivate)}, space: Quarantine, rule: PlaceConflict, roots: []string{"oss", "acme"}},
		{name: "unknown system", o: Origin{System: "email"}, space: Quarantine, rule: PlaceUnmatched},
	}
	if len(cases) < 22 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.opt.Confirmed == nil {
				c.opt.Confirmed = allConfirmed
			}
			p := Classify(c.o, res, c.opt)
			if p.Space != c.space || p.Rule != c.rule {
				t.Fatalf("got %s by %s (%s), want %s by %s", p.Space, p.Rule, p.Reason, c.space, c.rule)
			}
			var roots []string
			for _, r := range p.Roots {
				roots = append(roots, r.Space)
			}
			if !slices.Equal(roots, c.roots) {
				t.Errorf("roots %v, want %v", roots, c.roots)
			}
			if p.ReadAs != c.readAs {
				t.Errorf("ReadAs %q, want %q", p.ReadAs, c.readAs)
			}
			if p.Reason == "" {
				t.Error("no reason given")
			}
			wantOutcome := Placed
			switch {
			case c.rule == PlaceConflict:
				wantOutcome = Conflict
			case c.space == Quarantine:
				wantOutcome = Quarantined
			}
			if p.Outcome != wantOutcome {
				t.Errorf("outcome %s, want %s", p.Outcome, wantOutcome)
			}
		})
	}
}

func TestClassifyRemoteEffect(t *testing.T) {
	res := resolvePlacement(t, placementPolicy)
	p := Classify(Origin{System: SystemFS, Path: "/home/u/code/company1/x", Remote: repo("oss-org", "x", VisibilityPublic)}, res, Options{Confirmed: allConfirmed})
	if p.Remote == nil || p.Remote.Space != "oss" || p.Remote.Kind != Public || p.Remote.Effect == "" {
		t.Fatalf("remote effect %+v", p.Remote)
	}
	if got := p.Roots; len(got) != 2 || got[0].Root != "/home/u/code" || got[1].Root != "/home/u/code/company1" {
		t.Fatalf("roots %+v", got)
	}
	if !slices.Equal(p.Spaces(), []string{"acme", "oss", "acme", "oss"}) {
		t.Fatalf("Spaces %v", p.Spaces())
	}
	// A nil Confirmed confirms nothing: a bound org's content is not placed.
	p = Classify(Origin{System: SystemGitHub, Remote: repo("acme", "x", VisibilityPrivate)}, res, Options{})
	if p.Rule != PlaceOrgUnconfirmed {
		t.Fatalf("nil Confirmed: %s", p.Rule)
	}
}

func TestClassifyWithoutPolicy(t *testing.T) {
	for _, res := range []*Resolved{nil, resolvePlacement(t, Policy{})} {
		p := Classify(Origin{System: SystemSession, Path: "/home/u/code/company1"}, res, Options{})
		if p.Space != Shared || p.ReadAs != Shared || p.Rule != PlaceNoPolicy || p.Outcome != Placed {
			t.Fatalf("got %+v", p)
		}
	}
}

// TestClassifyNeverPublicWhenUnplaced is SP-022.1.a2: over random origins,
// an unmatched or conflicting origin never lands in a public space.
func TestClassifyNeverPublicWhenUnplaced(t *testing.T) {
	res := resolvePlacement(t, placementPolicy)
	rng := rand.New(rand.NewSource(22))
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	systems := []System{SystemFS, SystemGitHub, SystemWeb, SystemSession, SystemClient, SystemSlack, SystemTeams, "other"}
	paths := []string{"", "/home/u/code", "/home/u/code/x", "/home/u/code/willesq/x", "/home/u/code/company1/x", "/home/u/code/globex/y", "/tmp/x", "rel/x", "/home/u/CODE/Company1"}
	owners := []string{"", "oss-org", "willesq", "acme", "globex", "stranger", "OSS-ORG"}
	vis := []Visibility{VisibilityUnknown, VisibilityPublic, VisibilityPrivate}
	urls := []string{"", "https://docs.oss.dev/a", "https://docs.oss.dev", "https://wiki.globex.example/x", "https://example.com/", "ftp://docs.oss.dev/a", "https://docs.oss.dev.evil/x"}
	writers := []string{"", "oss", "personal", "acme", "globex", "gone", Quarantine, Shared}
	tenants := []string{"", "TACME", "tacme", "globex-tenant", "x"}
	overrides := []string{"", "", "", "oss", "acme", "gone", Quarantine}
	unplaced := 0
	for i := 0; i < 20000; i++ {
		o := Origin{
			System:      systems[rng.Intn(len(systems))],
			Path:        pick(paths),
			URL:         pick(urls),
			Tenant:      pick(tenants),
			WriterSpace: pick(writers),
		}
		if owner := pick(owners); owner != "" {
			o.Remote = repo(owner, "r", vis[rng.Intn(len(vis))])
		}
		confirmed := rng.Intn(4) > 0
		opt := Options{Confirmed: func(string) bool { return confirmed }}
		override := pick(overrides)
		opt.Override = override
		p := Classify(o, res, opt)
		if override != "" {
			continue // the operator's retag is the one deliberate exception
		}
		s, _ := res.Space(p.Space)
		if (p.Outcome != Placed || p.Rule == PlaceUnmatched || p.Rule == PlaceConflict) && (s.Kind == Public || p.Space != Quarantine) {
			t.Fatalf("origin %+v remote %+v: %s by %s", o, o.Remote, p.Space, p.Rule)
		}
		if p.Outcome != Placed {
			unplaced++
		}
		if o.System == SystemSession && s.Kind == Public {
			t.Fatalf("session content placed in public: %+v", o)
		}
	}
	if unplaced == 0 {
		t.Fatal("the generator produced no unplaced origins")
	}
}

// TestSessionUnderPublicRoot is SP-022.1.a3 (D-22): a session pinned under a
// public root reads public only, and its content goes to
// public_root_sessions.
func TestSessionUnderPublicRoot(t *testing.T) {
	res := resolvePlacement(t, placementPolicy)
	p := Classify(Origin{System: SystemSession, Path: "/home/u/code/tools"}, res, Options{Confirmed: allConfirmed})
	if p.Space != "personal" || p.Rule != PlacePublicRootSession {
		t.Fatalf("writes go to %s by %s, want personal", p.Space, p.Rule)
	}
	for _, s := range placementPolicy.Spaces {
		if got, want := res.CanRead(p.ReadAs, s.Name), s.Kind == Public; got != want {
			t.Errorf("session reads %s: %v, want %v", s.Name, got, want)
		}
	}
	if res.CanRead(p.ReadAs, Quarantine) {
		t.Error("session reads quarantine")
	}

	// With no default space configured and two private spaces, the content
	// goes to quarantine; the session still reads public only.
	two := placementPolicy
	two.PublicRootSessions = ""
	two.Spaces = append(slices.Clone(two.Spaces), Space{Name: "family", Kind: Private})
	res2 := resolvePlacement(t, two)
	p = Classify(Origin{System: SystemSession, Path: "/home/u/code/tools"}, res2, Options{})
	if p.Space != Quarantine || p.Outcome != Quarantined || p.ReadAs != "oss" {
		t.Fatalf("got %+v", p)
	}
}

func TestEnginePlace(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, openStore(t))
	o := Origin{System: SystemGitHub, Remote: repo("acme", "x", VisibilityPrivate)}
	p, err := e.Place(ctx, o)
	if err != nil || p.Space != Shared {
		t.Fatalf("no policy: %+v, %v", p, err)
	}
	saveAndApply(t, e, validPolicy)
	if p, err = e.Place(ctx, o); err != nil || p.Space != "acme" {
		t.Fatalf("confirmed: %+v, %v", p, err)
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE spaces_org_bindings SET suspended_at = 'x'`); err != nil {
		t.Fatal(err)
	}
	if p, err = e.Place(ctx, o); err != nil || p.Rule != PlaceOrgUnconfirmed {
		t.Fatalf("suspended: %+v, %v", p, err)
	}
	if p, err = e.Place(ctx, Origin{System: SystemFS, Path: "/home/u/code/willesq/x"}); err != nil || p.Space != "personal" {
		t.Fatalf("root: %+v, %v", p, err)
	}
}

func TestPrefixKeyNormalizes(t *testing.T) {
	for in, want := range map[string]string{
		"https://a.com":                   "http(s)://a.com",
		"https://a.com/":                  "http(s)://a.com",
		"https://a.com?x=1#f":             "http(s)://a.com",
		"HTTPS://A.com..:443/x/./y/../z/": "http(s)://a.com/x/z",
		"http://a.com:80/%70rivate":       "http(s)://a.com/private",
		"https://a.com:0443/s":            "http(s)://a.com/s",
		"http://a.com:8080/a%20b":         "http(s)://a.com:8080/a%20b",
		"https://[::1]:443/x":             "http(s)://[::1]/x",
		"https://[::1]:8443/x":            "http(s)://[::1]:8443/x",
		"https://a.com/../..":             "http(s)://a.com",
	} {
		if got, ok := prefixKey(in); !ok || got != want {
			t.Errorf("prefixKey(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"ftp://a.com", "https://", "a.com/x", "%zz", `https://a.com/docs/..\secret`, "https://a.com:99999/", "https://a.com:x/", "https://.../"} {
		if _, ok := prefixKey(bad); ok {
			t.Errorf("prefixKey(%q) accepted", bad)
		}
	}
}

func TestWebPrefixBoundaries(t *testing.T) {
	res := resolvePlacement(t, Policy{Spaces: []Space{
		{Name: "pub", Kind: Public, WebPrefixes: []string{"https://a.com"}},
		{Name: "job", Kind: Isolated, WebPrefixes: []string{"https://wiki.corp.com/", "https://a.com/docs/"}},
		{Name: "me", Kind: Private},
	}})
	for url, want := range map[string]string{
		"https://wiki.corp.com":       "job",
		"https://wiki.corp.com?x=1":   "job",
		"https://wiki.corp.com#f":     "job",
		"https://a.com/docs":          "job",
		"https://a.com/docs?x":        "job",
		"https://a.com/docs/x":        "job",
		"https://a.com/docsx":         "pub",
		"https://wiki.corp.com.evil/": Quarantine,
		"http://wiki.corp.com/x":      "job",
	} {
		if p := Classify(Origin{System: SystemWeb, URL: url}, res, Options{}); p.Space != want {
			t.Errorf("%s: %s by %s, want %s", url, p.Space, p.Rule, want)
		}
	}
	for _, url := range []string{`https://wiki.corp.com/a\b`, "https://wiki.corp.com/a%5Cb", "https://wiki.corp.com/%zz", "not a url"} {
		if p := Classify(Origin{System: SystemWeb, URL: url, WriterSpace: "me"}, res, Options{}); p.Space != Quarantine {
			t.Errorf("unkeyable %s: %s by %s, want quarantine", url, p.Space, p.Rule)
		}
	}
	_, err := Validate(Policy{Spaces: []Space{
		{Name: "one", Kind: Private, WebPrefixes: []string{"https://a.com"}},
		{Name: "two", Kind: Private, WebPrefixes: []string{"http://A.com./"}},
	}}, testEnv())
	if ps, _ := err.(Problems); !ps.Has(RulePrefixBoundTwice) {
		t.Errorf("a prefix with and without its slash was bound twice: %v", err)
	}
}

// TestPublicRootNeedsProofForCheckouts is SP-030.1.a5 (D-50, D-65) in the
// classifier: until public_paths exist (SP-076), no local content under a
// public root is placed public, whatever the remote and however the roots
// are nested, even with a public root bound exactly at the folder. A repo
// GitHub does not report public is named for the root's lack of proof too.
// Sessions keep D-22, and stricter roots and remotes are unaffected.
func TestPublicRootNeedsProofForCheckouts(t *testing.T) {
	p := placementPolicy
	p.Spaces = slices.Clone(p.Spaces)
	p.Spaces[0].Roots = []string{"~/code", "~/code/declared"}
	res := resolvePlacement(t, p)
	const h = "/home/u/code"
	for _, c := range []struct {
		name  string
		o     Origin
		space string
		rule  PlaceRule
	}{
		{"no remote", Origin{System: SystemFS, Path: h + "/gl/a.md"}, Quarantine, PlaceNoPublicProof},
		{"verified public on GitHub", Origin{System: SystemFS, Path: h + "/x/a.md", Remote: repo("stranger", "x", VisibilityPublic)}, Quarantine, PlaceNoPublicProof},
		{"public org repo", Origin{System: SystemFS, Path: h + "/lib/a.md", Remote: repo("oss-org", "lib", VisibilityPublic)}, Quarantine, PlaceNoPublicProof},
		{"private repo of public org", Origin{System: SystemFS, Path: h + "/s/a.md", Remote: repo("oss-org", "s", VisibilityPrivate)}, Quarantine, PlaceNoPublicProof},
		{"under a public root bound at its folder", Origin{System: SystemFS, Path: h + "/declared/a.md", Remote: repo("stranger", "d", VisibilityPublic)}, Quarantine, PlaceNoPublicProof},
		{"under a private root", Origin{System: SystemFS, Path: h + "/willesq/a.md"}, "personal", PlaceRoot},
		{"stricter remote under a public root", Origin{System: SystemFS, Path: h + "/x/a.md", Remote: repo("acme", "x", VisibilityPrivate)}, Quarantine, PlaceNoPublicProof},
		{"stricter remote under a private root", Origin{System: SystemFS, Path: h + "/willesq/x/a.md", Remote: repo("acme", "x", VisibilityPrivate)}, "acme", PlaceRemoteStricter},
		{"session with a stricter remote under a public root", Origin{System: SystemSession, Path: h + "/x", Remote: repo("acme", "x", VisibilityPrivate)}, "acme", PlaceRemoteStricter},
		{"session under a public root", Origin{System: SystemSession, Path: h + "/gl"}, "personal", PlacePublicRootSession},
		{"docs sync of a public repo", Origin{System: SystemGitHub, Remote: repo("oss-org", "lib", VisibilityPublic)}, "oss", PlaceRemote},
	} {
		got := Classify(c.o, res, Options{Confirmed: allConfirmed})
		if got.Space != c.space || got.Rule != c.rule {
			t.Errorf("%s = %s (%s), want %s (%s)", c.name, got.Space, got.Rule, c.space, c.rule)
		}
	}
}

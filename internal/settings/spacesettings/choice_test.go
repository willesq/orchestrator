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

// soloWorld is a temp home with one isolated job (code/a) under a public
// root (code), and job b's root (code/b) kept for adding a second job.
func soloWorld(t *testing.T, withB bool, ghB string) world {
	t.Helper()
	w := newWorld(t)
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "oss", Kind: spaces.Public, Roots: []string{"~/code"}},
		{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}},
	}}
	if withB {
		p.Spaces = append(p.Spaces, spaces.Space{Name: "jobb", Kind: spaces.Isolated, Roots: []string{"~/code/b"}, GHConfigDir: ghB})
	}
	res, err := spaces.Validate(p, spaces.Env{Home: w.home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	w.res = res
	return w
}

// TestSandboxChoice is the decision of SP-046.4.a2 (D-61): auto gives a
// session the sandbox rules only when they keep it out of another isolated
// space's root; on always; off and the switch never.
func TestSandboxChoice(t *testing.T) {
	solo := soloWorld(t, false, "")
	two := soloWorld(t, true, "")
	off, on, auto := Choice{Setting: spaces.SandboxOff}, ChoiceOn, ChoiceAuto
	switched := Choice{SwitchedOff: true, Setting: spaces.SandboxOn}
	tests := []struct {
		name  string
		r     Rules
		wants map[string]bool // choice -> sandboxes
	}{
		{"the only job", For(solo.res, "joba", solo.layout), map[string]bool{"auto": false, "on": true, "off": false, "switched": false}},
		{"public, one job", For(solo.res, "oss", solo.layout), map[string]bool{"auto": true, "on": true, "off": false, "switched": false}},
		{"job a of two", For(two.res, "joba", two.layout), map[string]bool{"auto": true, "on": true, "off": false, "switched": false}},
		{"unplaced in job a's root", ForUnplaced(solo.res, filepath.Join(solo.code, "a", "x"), solo.layout), map[string]bool{"auto": false, "on": true}},
		{"unplaced outside it", ForUnplaced(solo.res, filepath.Join(solo.code, "x"), solo.layout), map[string]bool{"auto": true, "on": true}},
	}
	choices := map[string]Choice{"auto": auto, "on": on, "off": off, "switched": switched, "empty": {}}
	for _, tt := range tests {
		for name, want := range tt.wants {
			if got := choices[name].Sandboxes(tt.r); got != want {
				t.Errorf("%s with %s: Sandboxes = %v, want %v", tt.name, name, got, want)
			}
		}
		if choices["empty"].Sandboxes(tt.r) != auto.Sandboxes(tt.r) {
			t.Errorf("%s: an empty setting is not auto", tt.name)
		}
	}
	if auto.AnySandboxed(solo.res, solo.layout) != true || off.AnySandboxed(two.res, two.layout) || auto.AnySandboxed(nil, solo.layout) {
		t.Error("AnySandboxed")
	}
	jobOnly := soloWorld(t, false, "")
	res, err := spaces.Validate(spaces.Policy{Spaces: []spaces.Space{{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}}}},
		spaces.Env{Home: jobOnly.home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if auto.AnySandboxed(res, jobOnly.layout) || !on.AnySandboxed(res, jobOnly.layout) {
		t.Error("one job alone: auto sandboxes nothing, on sandboxes it")
	}
}

// TestSyncFollowsSandboxChoice is SP-046.4.a2 on the files: with auto and
// one job nothing is written; a job naming its own gh profile still gets
// GH_CONFIG_DIR without the rules (a login choice, D-59); on writes the
// rules; off takes them out and keeps the gh profile; the switch takes
// everything out; the user's own entries stay throughout.
func TestSyncFollowsSandboxChoice(t *testing.T) {
	w := newWorld(t)
	gh := filepath.Join(w.home, "gh", "a")
	res, err := spaces.Validate(spaces.Policy{Spaces: []spaces.Space{
		{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}, GHConfigDir: "~/gh/a"},
	}}, spaces.Env{Home: w.home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	w.res = res
	w.layout.Tmp = filepath.Join(shortTmp(t), "r")
	co := gitRepo(t, filepath.Join(w.code, "a", "app"))
	writeFile(t, localOf(co), `{"model": "mine"}`)
	s := w.syncer()
	ctx := context.Background()
	check := func(choice Choice, sandbox, login bool) {
		t.Helper()
		rep := s.Sync(ctx, res, choice)
		if len(rep.Problems) != 0 {
			t.Fatalf("%+v: %v", choice, rep.Problems)
		}
		got := readFile(t, localOf(co))
		if !strings.Contains(got, `"model": "mine"`) {
			t.Errorf("%+v lost the user's entry:\n%s", choice, got)
		}
		if strings.Contains(got, `"sandbox"`) != sandbox {
			t.Errorf("%+v: sandbox rules present = %v, want %v:\n%s", choice, !sandbox, sandbox, got)
		}
		if strings.Contains(got, `"GH_CONFIG_DIR": "`+gh+`"`) != login {
			t.Errorf("%+v: GH_CONFIG_DIR present = %v, want %v:\n%s", choice, !login, login, got)
		}
		if strings.Contains(got, "TMPDIR") != sandbox {
			t.Errorf("%+v: TMPDIR present = %v, want %v:\n%s", choice, !sandbox, sandbox, got)
		}
	}
	check(ChoiceAuto, false, true)
	if _, err := os.Stat(w.layout.TmpDir("joba")); !os.IsNotExist(err) {
		t.Errorf("auto with nothing to deny made the temp folder: %v", err)
	}
	if out := git(t, co, "check-ignore", localOf(co)); out == "" {
		t.Error("the file with only GH_CONFIG_DIR is not ignored")
	}
	check(ChoiceOn, true, true)
	check(Choice{Setting: spaces.SandboxOff}, false, true)
	check(Choice{SwitchedOff: true, Setting: spaces.SandboxOn}, false, false)
	check(ChoiceOn, true, true)
	if readFile(t, localOf(co)) == "" {
		t.Fatal("empty file")
	}
	// A space on the default login keeps nothing but the user's entries.
	res2, err := spaces.Validate(spaces.Policy{Spaces: []spaces.Space{{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}}}},
		spaces.Env{Home: w.home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if rep := s.Sync(ctx, res2, ChoiceAuto); len(rep.Cleared) != 1 {
		t.Fatalf("default login, auto: %+v", rep)
	}
	if got := readFile(t, localOf(co)); strings.Contains(got, "GH_CONFIG_DIR") || strings.Contains(got, "sandbox") {
		t.Errorf("default login, auto:\n%s", got)
	}
}

// TestGHReadRules is RC-208 option (a) as D-61 decided it (SP-046.4.a3,
// a4): a space on the default gh login may read the default gh folder
// inside the sandbox and reach github.com; a space naming its own profile
// may read only that profile and is denied the default folder; other
// spaces' profiles stay denied; a checkout of no space reads none; an
// allowRead that would hold another space's profile is left out.
func TestGHReadRules(t *testing.T) {
	ghB := "~/gh/b"
	w := soloWorld(t, true, ghB)
	def := filepath.Join(w.home, ".config", "gh")
	profB := filepath.Join(w.home, "gh", "b")
	w.layout.GHDefault = def

	a := For(w.res, "joba", w.layout)
	if !slices.Equal(a.AllowRead, []string{def}) || slices.Contains(a.Deny, def) || !slices.Contains(a.Deny, profB) {
		t.Errorf("default-login space: allow %v deny %v", a.AllowRead, a.Deny)
	}
	fa := ClaudeFragment(a)
	if !slices.Equal(fa.Lists[keyAllowRead], []string{def}) || !slices.Equal(fa.Lists[keyDomains], []string{"api.github.com", "github.com"}) ||
		fa.Scalars[keyTrustd] != true {
		t.Errorf("default-login fragment: %v %v", fa.Lists, fa.Scalars)
	}
	if lf := LauncherFragment(a); !slices.Equal(lf.Lists[keyAllowRead], []string{def}) || lf.Scalars[keyTrustd] != true {
		t.Errorf("launcher fragment: %v", lf.Lists)
	}
	if _, ok := fa.Scalars["env.GH_CONFIG_DIR"]; ok {
		t.Error("a default-login space sets GH_CONFIG_DIR")
	}

	b := For(w.res, "jobb", w.layout)
	if !slices.Equal(b.AllowRead, []string{profB}) || !slices.Contains(b.Deny, def) || slices.Contains(b.Deny, profB) {
		t.Errorf("own-profile space: allow %v deny %v", b.AllowRead, b.Deny)
	}
	fb := ClaudeFragment(b)
	if !slices.Contains(fb.Lists[keyPermDeny], "Read(/"+def+"/**)") || fb.Scalars["env.GH_CONFIG_DIR"] != profB {
		t.Errorf("own-profile fragment: %v %v", fb.Lists[keyPermDeny], fb.Scalars)
	}

	u := ForUnplaced(w.res, filepath.Join(w.code, "a", "x"), w.layout)
	fu := ClaudeFragment(u)
	if len(u.AllowRead) != 0 || !slices.Contains(u.Deny, def) || !slices.Contains(u.Deny, profB) || fu.Lists[keyDomains] != nil || fu.Scalars[keyTrustd] != nil {
		t.Errorf("unplaced: %+v %v", u, fu.Lists)
	}

	// Another space's profile inside the default folder: the default
	// login's allowRead would re-open it, so it is left out.
	nested := soloWorld(t, true, "~/.config/gh/jobb")
	nested.layout.GHDefault = filepath.Join(nested.home, ".config", "gh")
	if r := For(nested.res, "joba", nested.layout); len(r.AllowRead) != 0 {
		t.Errorf("allowRead holding job b's profile: %v", r.AllowRead)
	}
	// Job b's own profile inside the default folder stays readable to it.
	if r := For(nested.res, "jobb", nested.layout); !slices.Equal(r.AllowRead, []string{filepath.Join(nested.home, ".config", "gh", "jobb")}) {
		t.Errorf("own nested profile: %v", r.AllowRead)
	}
	// A space naming the default folder itself as its profile is not denied it.
	same := soloWorld(t, true, "~/.config/gh")
	same.layout.GHDefault = filepath.Join(same.home, ".config", "gh")
	if r := For(same.res, "jobb", same.layout); slices.Contains(r.Deny, same.layout.GHDefault) {
		t.Errorf("own profile = default folder denied: %v", r.Deny)
	}
	// No default folder known: no allowRead, no deny of it.
	w.layout.GHDefault = ""
	if r := For(w.res, "joba", w.layout); len(r.AllowRead) != 0 {
		t.Errorf("no default folder: %v", r.AllowRead)
	}
}

// TestDefaultGHDir: gh's own order, $GH_CONFIG_DIR, $XDG_CONFIG_HOME/gh,
// ~/.config/gh; relative values are not taken.
func TestDefaultGHDir(t *testing.T) {
	home := realDir(t)
	t.Setenv("HOME", home)
	t.Setenv("GH_CONFIG_DIR", "/x/gh")
	t.Setenv("XDG_CONFIG_HOME", "/y")
	if d := DefaultGHDir(); d != "/x/gh" {
		t.Errorf("GH_CONFIG_DIR: %s", d)
	}
	t.Setenv("GH_CONFIG_DIR", "rel")
	if d := DefaultGHDir(); d != "/y/gh" {
		t.Errorf("XDG: %s", d)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if d := DefaultGHDir(); d != filepath.Join(home, ".config", "gh") {
		t.Errorf("home: %s", d)
	}
	t.Setenv("HOME", "")
	if d := DefaultGHDir(); d != "" {
		t.Errorf("no home: %s", d)
	}
	if l := DefaultLayout(); l.GHDefault != "" {
		t.Errorf("layout: %+v", l)
	}
	if f := LoginFragment(Rules{}); !f.Empty() {
		t.Errorf("login fragment without a profile: %+v", f)
	}
}

// TestAutoDeniesOtherLogins (D-69, RC-217): under auto, a session also gets
// orch's sandbox rules when they keep it out of another space's own login
// folder, a gh_config_dir or claude_config_dir, whatever that space's
// kind, or out of the default gh login's folder (a space naming its own
// gh profile); a space's own login folder alone does not count, and a job
// alone on the default login among spaces naming none still gets none.
func TestAutoDeniesOtherLogins(t *testing.T) {
	w := newWorld(t)
	validate := func(p spaces.Policy) *spaces.Resolved {
		t.Helper()
		res, err := spaces.Validate(p, spaces.Env{Home: w.home, InGitTree: func(string) bool { return false }})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	job := spaces.Space{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}}
	tests := []struct {
		name  string
		other spaces.Space
		want  bool
		deny  string
	}{
		{"public naming a gh profile", spaces.Space{Name: "oss", Kind: spaces.Public, Roots: []string{"~/code"}, GHConfigDir: "~/gh/oss"}, true, "~/gh/oss"},
		{"private naming a Claude Code folder", spaces.Space{Name: "personal", Kind: spaces.Private, Roots: []string{"~/code/me"}, ClaudeConfigDir: "~/claude/me"}, true, "~/claude/me"},
		{"public naming none", spaces.Space{Name: "oss", Kind: spaces.Public, Roots: []string{"~/code"}}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := validate(spaces.Policy{Spaces: []spaces.Space{tt.other, job}})
			r := For(res, "joba", w.layout)
			if r.OtherJobs {
				t.Fatalf("no other isolated space, yet OtherJobs: %+v", r)
			}
			if r.OtherLogins != tt.want || ChoiceAuto.Sandboxes(r) != tt.want {
				t.Errorf("OtherLogins = %v, auto sandboxes = %v, want %v", r.OtherLogins, ChoiceAuto.Sandboxes(r), tt.want)
			}
			if tt.deny != "" {
				d := filepath.Join(w.home, strings.TrimPrefix(tt.deny, "~/"))
				if !slices.Contains(r.Deny, d) {
					t.Errorf("deny %v lacks %s", r.Deny, d)
				}
				co := filepath.Join(w.code, "a", "app")
				if f := ChoiceAuto.CheckoutFor(r, co); !slices.Contains(f.Lists[keyDenyRead], d) {
					t.Errorf("auto's checkout fragment does not deny %s: %+v", d, f.Lists)
				}
				if !ChoiceAuto.AnySandboxed(res, w.layout) {
					t.Error("AnySandboxed is false")
				}
			}
			// The space naming the login folder is not kept out of its own.
			if o := For(res, tt.other.Name, w.layout); o.OtherLogins {
				t.Errorf("%s: its own login folder counted: %+v", tt.other.Name, o)
			}
			// A checkout of no space is denied every space's login folder.
			if u := ForUnplaced(res, filepath.Join(w.code, "a", "x"), w.layout); u.OtherLogins != tt.want || ChoiceAuto.Sandboxes(u) != tt.want {
				t.Errorf("unplaced: OtherLogins = %v, want %v", u.OtherLogins, tt.want)
			}
		})
	}
	// The default gh login counts too: a job naming its own gh profile is
	// denied the default login's folder, so auto sandboxes it even alone;
	// a job on the default login alone is not; a checkout of no space is
	// denied the default folder whenever there is one.
	l := w.layout
	l.GHDefault = filepath.Join(w.home, "gh", "default")
	own := validate(spaces.Policy{Spaces: []spaces.Space{{Name: "joba", Kind: spaces.Isolated, Roots: []string{"~/code/a"}, GHConfigDir: "~/gh/a"}}})
	if r := For(own, "joba", l); !r.OtherLogins || !ChoiceAuto.Sandboxes(r) || !slices.Contains(r.Deny, l.GHDefault) {
		t.Errorf("own gh profile alone: %+v", r)
	}
	plain := validate(spaces.Policy{Spaces: []spaces.Space{job}})
	if r := For(plain, "joba", l); r.OtherLogins || ChoiceAuto.Sandboxes(r) {
		t.Errorf("default login alone: %+v", r)
	}
	if u := ForUnplaced(plain, filepath.Join(w.code, "a", "x"), l); !u.OtherLogins || !ChoiceAuto.Sandboxes(u) {
		t.Errorf("unplaced with a default gh folder: %+v", u)
	}
}

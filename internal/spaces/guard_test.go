package spaces

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// guardFixture is a home laid out as the plan's examples: ~/code public
// (oss), ~/code/willesq private (personal), ~/code/company1 isolated (acme,
// which reads personal) and ~/code/globex isolated (globex). Scratch is a
// folder in no space, standing in for /tmp.
type guardFixture struct {
	home, code, scratch string
	acmeApp, globexApp  string
	personal, lib       string
	worktree            string
	pol                 GuardPolicy
}

func newGuardFixture(t *testing.T) guardFixture {
	t.Helper()
	f := guardFixture{home: realTemp(t), scratch: realTemp(t)}
	f.code = filepath.Join(f.home, "code")
	f.acmeApp = filepath.Join(f.code, "company1", "app")
	f.globexApp = filepath.Join(f.code, "globex", "app")
	f.personal = filepath.Join(f.code, "willesq", "orch")
	f.lib = filepath.Join(f.code, "lib")
	mkdirs(t, filepath.Join(f.acmeApp, "pkg"), filepath.Join(f.globexApp, "pkg"), f.personal, f.lib,
		filepath.Join(f.home, ".claude"), filepath.Join(f.scratch, "build"))
	writeFile(t, filepath.Join(f.globexApp, "secret.txt"), "x")

	// A linked worktree of the acme checkout, outside every root, laid out
	// as git worktree add does.
	gitdir := filepath.Join(f.acmeApp, ".git", "worktrees", "feat")
	f.worktree = filepath.Join(f.scratch, "wt", "feat")
	mkdirs(t, gitdir, filepath.Join(f.worktree, "pkg"))
	writeFile(t, filepath.Join(f.worktree, ".git"), "gitdir: "+gitdir+"\n")
	writeFile(t, filepath.Join(gitdir, "commondir"), "../..\n")

	env := testEnv()
	env.Home = f.home
	res, err := Validate(placementPolicy, env)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	f.pol, err = NewGuardPolicy(res, []string{
		filepath.Join(f.home, ".claude"),
		filepath.Join(f.acmeApp, ".claude", "settings.local.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// event extracts a call's targets the way orchd does and wraps them.
func (f guardFixture) event(tool, input, cwd string) GuardEvent {
	return GuardEvent{Kind: EventPreToolUse, Tool: tool, Cwd: cwd,
		Targets: extractTargets(tool, json.RawMessage(input), cwd, f.home)}
}

func fileTool(tool, path string) (string, string) {
	b, _ := json.Marshal(map[string]string{"file_path": path, "content": "x", "old_string": "a", "new_string": "b"})
	return tool, string(b)
}

// want is one enforcing mode's expected verdict and rule.
type want struct {
	v    Verdict
	rule GuardRule
}

type guardCase struct {
	name   string
	pol    *GuardPolicy // nil: the fixture's policy
	pin    GuardPin
	ev     GuardEvent
	block  want // block-other-jobs; also what off and log record as Would
	strict want
}

var acmePin = GuardPin{Found: true, Space: "acme"}

func guardCases(t *testing.T, f guardFixture) []guardCase {
	t.Helper()
	ok := want{Allow, RuleWithinSpace}
	pathRefused := want{Refuse, RuleOtherJobPath}
	ev := func(call func() (string, string), cwd string) GuardEvent {
		tool, in := call()
		return f.event(tool, in, cwd)
	}
	cdOut := f.event("", "", f.acmeApp)
	cdOut.Kind, cdOut.Targets = EventCwdChanged, ToolTargets{CdDest: []string{f.globexApp}}
	empty := GuardPolicy{}
	return []guardCase{
		{name: "cd within the space", pin: acmePin, ev: ev(func() (string, string) { return bash("cd pkg && ls && cd .. && cat go.mod") }, f.acmeApp), block: ok, strict: ok},
		{name: "cd into another job", pin: acmePin, ev: ev(func() (string, string) { return bash("cd " + f.globexApp) }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "compound cd into another job", pin: acmePin, ev: ev(func() (string, string) { return bash("cd .. && cd ../globex/app && cat secret.txt") }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "absolute path read in another job", pin: acmePin, ev: ev(func() (string, string) { return bash("cat " + filepath.Join(f.globexApp, "secret.txt")) }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "Read tool into another job", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.globexApp, "secret.txt")) }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "Read tool outside every root", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.scratch, "build", "out.log")) }, f.acmeApp), block: ok, strict: ok},
		{name: "Read tool in a public space", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.lib, "README.md")) }, f.acmeApp), block: ok, strict: ok},
		{name: "git -C into another job", pin: acmePin, ev: ev(func() (string, string) { return bash("git -C " + f.globexApp + " log -1") }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "gh api into another job's org", pin: acmePin, ev: ev(func() (string, string) { return bash("gh api repos/globex/app/pulls") }, f.acmeApp), block: want{Refuse, RuleOtherJobRepo}, strict: want{Refuse, RuleOtherJobRepo}},
		{name: "gh -R into another job's org", pin: acmePin, ev: ev(func() (string, string) { return bash("gh pr list -R Globex/app") }, f.acmeApp), block: want{Refuse, RuleOtherJobRepo}, strict: want{Refuse, RuleOtherJobRepo}},
		{name: "gh -R into the session's own org", pin: acmePin, ev: ev(func() (string, string) { return bash("gh pr list -R acme/app") }, f.acmeApp), block: ok, strict: ok},
		{name: "gh -R into a private space's org", pin: GuardPin{Found: true, Space: "globex"}, ev: ev(func() (string, string) { return bash("gh pr list -R willesq/orch") }, f.globexApp), block: ok, strict: want{Refuse, RuleUnreadableSpace}},
		{name: "opaque bash -c", pin: acmePin, ev: ev(func() (string, string) { return bash(`bash -c "$CMD"`) }, f.acmeApp), block: want{Allow, RuleOpaqueUnverified}, strict: want{Refuse, RuleOpaqueStrict}},
		{name: "literal bash -c into another job", pin: acmePin, ev: ev(func() (string, string) { return bash("bash -c 'cat " + filepath.Join(f.globexApp, "secret.txt") + "'") }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "opaque call that also names another job", pin: acmePin, ev: ev(func() (string, string) { return bash(`eval "$X"; cat ` + f.globexApp + "/secret.txt") }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "analysis limit", pin: acmePin, ev: GuardEvent{Kind: EventPreToolUse, Tool: "Bash", Cwd: f.acmeApp, Targets: ToolTargets{Opaque: true, OpaqueReasons: []string{"too many targets"}}}, block: want{Refuse, RuleAnalysisLimit}, strict: want{Refuse, RuleAnalysisLimit}},
		{name: "build in a scratch folder", pin: acmePin, ev: ev(func() (string, string) {
			return bash("cd " + filepath.Join(f.scratch, "build") + " && go build -o out ./...")
		}, f.acmeApp), block: ok, strict: ok},
		{name: "edit in the session's own worktree", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Edit", filepath.Join(f.worktree, "pkg", "a.go")) }, f.worktree), block: ok, strict: ok},
		{name: "edit in another job's worktree", pin: GuardPin{Found: true, Space: "globex"}, ev: ev(func() (string, string) { return fileTool("Edit", filepath.Join(f.worktree, "pkg", "a.go")) }, f.globexApp), block: pathRefused, strict: pathRefused},
		{name: "EnterWorktree within the space", pin: acmePin, ev: ev(func() (string, string) { return toolIn("EnterWorktree", `{"name":"feat"}`) }, f.acmeApp), block: ok, strict: ok},
		{name: "/cd into another job is logged, not refused", pin: acmePin, ev: cdOut, block: pathRefused, strict: pathRefused},
		{name: "first call after /cd into another job", pin: acmePin, ev: ev(func() (string, string) { return bash("ls") }, f.globexApp), block: pathRefused, strict: pathRefused},
		{name: "subagent launch", pin: acmePin, ev: ev(func() (string, string) { return toolIn("Agent", `{"description":"look","prompt":"read the code"}`) }, f.acmeApp), block: ok, strict: ok},
		{name: "subagent's own call into another job", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Grep", f.globexApp) }, f.acmeApp), block: pathRefused, strict: pathRefused},
		{name: "settings tampering with Write", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Write", filepath.Join(f.home, ".claude", "settings.json")) }, f.acmeApp), block: want{Refuse, RuleSelfProtection}, strict: want{Refuse, RuleSelfProtection}},
		{name: "settings tampering with Edit of the checkout's local settings", pin: acmePin, ev: ev(func() (string, string) {
			return fileTool("Edit", filepath.Join(f.acmeApp, ".claude", "settings.local.json"))
		}, f.acmeApp), block: want{Refuse, RuleSelfProtection}, strict: want{Refuse, RuleSelfProtection}},
		{name: "reading harness settings is not tampering", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.home, ".claude", "settings.json")) }, f.acmeApp), block: ok, strict: ok},
		{name: "grep -r over a folder holding another job", pin: acmePin, ev: ev(func() (string, string) { return bash("grep -r password " + f.code) }, f.acmeApp), block: want{Refuse, RuleOtherJobAncestor}, strict: want{Refuse, RuleOtherJobAncestor}},
		{name: "glob prefix holding another job", pin: acmePin, ev: ev(func() (string, string) { return bash("cat " + f.code + "/*/app/secret.txt") }, f.acmeApp), block: want{Refuse, RuleOtherJobAncestor}, strict: want{Refuse, RuleOtherJobAncestor}},
		{name: "cd to a folder holding another job, then search it", pin: acmePin, ev: ev(func() (string, string) { return bash("cd " + f.code + " && rg secret") }, f.acmeApp), block: want{Refuse, RuleOtherJobAncestor}, strict: want{Refuse, RuleOtherJobAncestor}},
		{name: "search in a cwd holding another job", pin: GuardPin{Found: true, Space: "personal"}, ev: ev(func() (string, string) { return bash("grep -r secret") }, f.code), block: ok, strict: want{Refuse, RuleOtherJobAncestor}},
		{name: "malformed repo", pin: acmePin, ev: GuardEvent{Kind: EventPreToolUse, Tool: "Bash", Cwd: f.acmeApp, Targets: ToolTargets{GHRepos: []string{"github.com/globex/app"}}}, block: want{Refuse, RuleMalformedTarget}, strict: want{Refuse, RuleMalformedTarget}},
		{name: "relative target", pin: acmePin, ev: GuardEvent{Kind: EventPreToolUse, Tool: "Read", Cwd: f.acmeApp, Targets: ToolTargets{Paths: []string{"code/globex/app"}}}, block: want{Refuse, RuleMalformedTarget}, strict: want{Refuse, RuleMalformedTarget}},
		{name: "unknown cwd", pin: acmePin, ev: ev(func() (string, string) { return bash("cat ../globex/app/secret.txt") }, "code/company1"), block: want{Refuse, RuleMalformedTarget}, strict: want{Refuse, RuleMalformedTarget}},
		{name: "isolated session reads a private space it lists", pin: acmePin, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.personal, "notes.md")) }, f.acmeApp), block: ok, strict: ok},
		{name: "isolated session reads a private space it does not list", pin: GuardPin{Found: true, Space: "globex"}, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.personal, "notes.md")) }, f.globexApp), block: ok, strict: want{Refuse, RuleUnreadableSpace}},
		{name: "private session reads an isolated space", pin: GuardPin{Found: true, Space: "personal"}, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.acmeApp, "main.go")) }, f.personal), block: pathRefused, strict: pathRefused},
		{name: "session pinned to no space reads a job", pin: GuardPin{Found: true}, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.acmeApp, "main.go")) }, f.lib), block: pathRefused, strict: pathRefused},
		{name: "session pinned to no space reads public and scratch", pin: GuardPin{Found: true}, ev: ev(func() (string, string) { return bash("cat README.md " + f.scratch + "/build/x") }, f.lib), block: ok, strict: ok},
		{name: "session pinned to no space reads a private space", pin: GuardPin{Found: true, Space: Quarantine}, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.personal, "notes.md")) }, f.lib), block: ok, strict: want{Refuse, RuleUnreadableSpace}},
		{name: "session pinned to a space the policy no longer has", pin: GuardPin{Found: true, Space: "gone"}, ev: ev(func() (string, string) { return fileTool("Read", filepath.Join(f.acmeApp, "main.go")) }, f.lib), block: pathRefused, strict: pathRefused},
		{name: "session with no pin", pin: GuardPin{}, ev: ev(func() (string, string) { return bash("ls") }, f.acmeApp), block: want{Refuse, RulePinMissing}, strict: want{Refuse, RulePinMissing}},
		{name: "no policy configured", pol: &empty, pin: GuardPin{}, ev: ev(func() (string, string) { return bash("cat " + f.globexApp + "/secret.txt") }, f.acmeApp), block: want{Allow, RuleNoPolicy}, strict: want{Allow, RuleNoPolicy}},
	}
}

// TestDecideCases is SP-040.1 a1: every case in each mode. Off and log
// never refuse and record the block-other-jobs verdict; the enforcing
// modes refuse a tool call as their verdict says, never a cwd-changed
// report.
func TestDecideCases(t *testing.T) {
	f := newGuardFixture(t)
	cases := guardCases(t, f)
	if len(cases) < 14 {
		t.Fatalf("%d cases, want at least 14", len(cases))
	}
	for _, c := range cases {
		pol := f.pol
		if c.pol != nil {
			pol = *c.pol
		}
		for _, mode := range []GuardMode{GuardOff, GuardLog, GuardBlockOtherJobs, GuardStrict} {
			t.Run(c.name+"/"+string(mode), func(t *testing.T) {
				w := c.block
				if mode == GuardStrict {
					w = c.strict
				}
				got := Decide(pol, c.pin, Enforcement{Guard: mode}, c.ev)
				// Reason: spelled out rather than taken from Enforcing, so the
				// test checks that off and log never refuse.
				enforced := (mode == GuardBlockOtherJobs || mode == GuardStrict) && c.ev.Kind == EventPreToolUse
				verdict := Allow
				if enforced {
					verdict = w.v
				}
				exp := GuardDecision{Verdict: verdict, Would: w.v, Rule: w.rule, Mode: mode, Enforced: enforced}
				if got != exp {
					t.Errorf("Decide = %+v, want %+v (targets %+v)", got, exp, c.ev.Targets)
				}
			})
		}
	}
}

func TestDecideModeDefaults(t *testing.T) {
	f := newGuardFixture(t)
	ev := f.event("Read", `{"file_path":"`+f.globexApp+`/secret.txt"}`, f.acmeApp)
	if d := Decide(f.pol, acmePin, Enforcement{}, ev); d.Mode != GuardBlockOtherJobs || d.Verdict != Refuse {
		t.Errorf("default mode: %+v, want block-other-jobs refusing", d)
	}
	opaque := f.event("Bash", `{"command":"eval \"$X\""}`, f.acmeApp)
	if d := Decide(f.pol, acmePin, Enforcement{Guard: "bogus"}, opaque); d.Mode != GuardStrict || d.Rule != RuleOpaqueStrict {
		t.Errorf("unknown mode: %+v, want it read as strict", d)
	}
	for _, kind := range []GuardEventKind{"", "PreToolUse", "bogus"} {
		ev.Kind = kind
		if d := Decide(f.pol, acmePin, Enforcement{}, ev); !d.Enforced || d.Verdict != Refuse {
			t.Errorf("kind %q: %+v, want the refusal enforced", kind, d)
		}
	}
}

// TestDecideSymlinkedRoots: roots are written as the user knows them, while
// Targets reports physical paths. A home reached through a symlink must
// still place targets in their spaces.
func TestDecideSymlinkedRoots(t *testing.T) {
	f := newGuardFixture(t)
	link := filepath.Join(realTemp(t), "home")
	symlink(t, f.home, link)
	env := testEnv()
	env.Home = link
	res, err := Validate(placementPolicy, env)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := NewGuardPolicy(res, []string{filepath.Join(link, ".claude")})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tool, in string
		rule     GuardRule
	}{
		{"Bash", `{"command":"cat ` + link + `/code/globex/app/secret.txt"}`, RuleOtherJobPath},
		{"Read", `{"file_path":"` + f.globexApp + `/secret.txt"}`, RuleOtherJobPath},
		{"Write", `{"file_path":"` + link + `/.claude/settings.json","content":"x"}`, RuleSelfProtection},
	} {
		ev := GuardEvent{Kind: EventPreToolUse, Tool: c.tool, Cwd: f.acmeApp,
			Targets: extractTargets(c.tool, json.RawMessage(c.in), f.acmeApp, link)}
		if d := Decide(pol, acmePin, Enforcement{}, ev); d.Verdict != Refuse || d.Rule != c.rule {
			t.Errorf("%s %s: %+v, want %s (targets %+v)", c.tool, c.in, d, c.rule, ev.Targets)
		}
	}
	own := GuardEvent{Kind: EventPreToolUse, Tool: "Read", Cwd: f.acmeApp, Targets: ToolTargets{Paths: []string{f.acmeApp + "/main.go"}}}
	if d := Decide(pol, acmePin, Enforcement{}, own); d.Verdict != Allow {
		t.Errorf("own space through the link: %+v", d)
	}
}

// TestDecideUnicodeSpelling: a target spelled in decomposed Unicode is the
// same folder as a root spelled composed, as macOS treats names.
func TestDecideUnicodeSpelling(t *testing.T) {
	p := Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, Roots: []string{"/w/acme"}},
		{Name: "cafe", Kind: Isolated, Roots: []string{"/w/caf\u00e9"}},
	}}
	res, err := Validate(p, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	pol, err := NewGuardPolicy(res, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := GuardEvent{Kind: EventPreToolUse, Tool: "Read", Cwd: "/w/acme", Targets: ToolTargets{Paths: []string{"/w/CAFE\u0301/x"}}}
	if d := Decide(pol, acmePin, Enforcement{}, ev); d.Verdict != Refuse || d.Rule != RuleOtherJobPath {
		t.Errorf("Decide = %+v, want other_job_path", d)
	}
}

// TestDecideLimitReasons checks that the reasons Targets gives at its
// limits are the ones the guard treats as limits, so a renamed reason
// cannot silently turn a limit into an allowed opaque call.
func TestDecideLimitReasons(t *testing.T) {
	f := newGuardFixture(t)
	cmd := "cat"
	for i := 0; i < maxTargets+10; i++ {
		cmd += " /f" + string(rune('a'+i%26)) + "/" + filepath.Base(f.home) + string(rune('a'+i/26%26))
	}
	tool, in := bash(cmd)
	ev := f.event(tool, in, f.acmeApp)
	if !slices.Contains(ev.Targets.OpaqueReasons, "too many targets") {
		t.Fatalf("reasons %v, want too many targets", ev.Targets.OpaqueReasons)
	}
	if d := Decide(f.pol, acmePin, Enforcement{}, ev); d.Rule != RuleAnalysisLimit || d.Verdict != Refuse {
		t.Errorf("Decide = %+v, want an analysis_limit refusal", d)
	}
	if d := Decide(f.pol, acmePin, Enforcement{}, f.event("Bash", `{"command":"ls"}`, "rel")); d.Rule != RuleMalformedTarget {
		t.Errorf("relative cwd: %+v", d)
	}
	ev = GuardEvent{Kind: EventPreToolUse, Tool: "Bash", Cwd: f.acmeApp, Targets: extractTargets("Bash", json.RawMessage(`{"command":"cat ~/x"}`), f.acmeApp, "")}
	if d := Decide(f.pol, acmePin, Enforcement{}, ev); d.Rule != RuleAnalysisLimit {
		t.Errorf("unknown home: %+v (reasons %v)", d, ev.Targets.OpaqueReasons)
	}
	deep := ""
	for i := 0; i < maxComponents+1; i++ {
		deep += "/d"
	}
	tool, in = bash("cat " + deep)
	if got := f.event(tool, in, f.acmeApp).Targets.OpaqueReasons; !slices.ContainsFunc(got, func(r string) bool { return limitReasons[r] }) {
		t.Errorf("reasons %v for a too-deep path name no limit", got)
	}
}

// TestDecideDeterministic is SP-040.1 a2: random events, pins, modes and
// policies always give the same decision, and Decide leaves its inputs
// unchanged.
func TestDecideDeterministic(t *testing.T) {
	f := newGuardFixture(t)
	rng := rand.New(rand.NewSource(40))
	locs := []string{"", f.acmeApp, f.globexApp, f.personal, f.lib, f.code, f.home, f.scratch, f.worktree,
		filepath.Join(f.home, ".claude", "settings.json"), filepath.Join(f.globexApp, "secret.txt"), "/", "rel/path"}
	repos := []string{"acme/app", "globex/app", "willesq/orch", "oss-org/lib", "stranger/x", "bad"}
	reasons := []string{"eval", "too many targets", "variable expansion", "analysis took too long"}
	tools := []string{"Bash", "Read", "Write", "Edit", "Grep", "Agent", "apply_patch"}
	pins := []GuardPin{{}, {Found: true}, acmePin, {Found: true, Space: "globex"}, {Found: true, Space: "personal"}, {Found: true, Space: Quarantine}, {Found: true, Space: "gone"}}
	modes := []GuardMode{"", GuardOff, GuardLog, GuardBlockOtherJobs, GuardStrict, "bogus"}
	bare, err := NewGuardPolicy(f.pol.res, nil)
	if err != nil {
		t.Fatal(err)
	}
	pols := []GuardPolicy{f.pol, {}, bare}
	pick := func(from []string) []string {
		var out []string
		for n := rng.Intn(4); n > 0; n-- {
			out = append(out, from[rng.Intn(len(from))])
		}
		return out
	}
	seen := map[GuardRule]bool{}
	for i := 0; i < 20000; i++ {
		ev := GuardEvent{Kind: EventPreToolUse, Tool: tools[rng.Intn(len(tools))], Cwd: locs[rng.Intn(len(locs))],
			Targets: ToolTargets{Paths: pick(locs), CdDest: pick(locs), GitDirs: pick(locs), Prefixes: pick(locs), GHRepos: pick(repos)}}
		if rng.Intn(4) == 0 {
			ev.Kind = EventCwdChanged
		}
		if rng.Intn(3) == 0 {
			ev.Targets.Opaque, ev.Targets.OpaqueReasons = true, pick(reasons)
		}
		pol, pin, enf := pols[rng.Intn(len(pols))], pins[rng.Intn(len(pins))], Enforcement{Guard: modes[rng.Intn(len(modes))]}
		before := deepCopyEvent(ev)
		first := Decide(pol, pin, enf, ev)
		seen[first.Rule] = true
		for j := 0; j < 3; j++ {
			if got := Decide(pol, pin, enf, deepCopyEvent(ev)); got != first {
				t.Fatalf("case %d: Decide = %+v, then %+v", i, first, got)
			}
		}
		if !reflect.DeepEqual(ev, before) {
			t.Fatalf("case %d: Decide changed its input", i)
		}
		if first.Verdict == Refuse && (!first.Enforced || first.Would != Refuse) {
			t.Fatalf("case %d: refused without enforcing: %+v", i, first)
		}
	}
	if len(seen) < 10 {
		t.Errorf("only %d rules reached: %v", len(seen), seen)
	}
}

func deepCopyEvent(ev GuardEvent) GuardEvent {
	b, _ := json.Marshal(ev)
	var out GuardEvent
	_ = json.Unmarshal(b, &out)
	return out
}

// TestLimitReasonsMatchTargets keeps limitReasons in step with the
// messages targets*.go writes: every reason the guard lists must still be
// written there, and every forced reason (one that stops the analysis as a
// whole) must be listed.
func TestLimitReasonsMatchTargets(t *testing.T) {
	files, err := filepath.Glob("targets*.go")
	if err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src.Write(b)
	}
	text := src.String()
	for r := range limitReasons {
		if !strings.Contains(text, strconv.Quote(r)) {
			t.Errorf("limit reason %q is not written by targets*.go", r)
		}
	}
	for _, m := range regexp.MustCompile(`forceOpaque\(("[^"]*")\)`).FindAllStringSubmatch(text, -1) {
		r, _ := strconv.Unquote(m[1])
		if !limitReasons[r] {
			t.Errorf("forced reason %q is not a limit reason", r)
		}
	}
}

// TestDecideAliasedRoots: a non-isolated root that is a symlink into, or
// onto, another job's root does not take that job's files out of it,
// whatever order the policy lists the roots in.
func TestDecideAliasedRoots(t *testing.T) {
	f := newGuardFixture(t)
	vendor := filepath.Join(f.globexApp, "vendor")
	mkdirs(t, vendor)
	alias := filepath.Join(f.home, "oss")
	symlink(t, vendor, alias)
	exact := filepath.Join(f.home, "mirror")
	symlink(t, filepath.Join(f.code, "globex"), exact)
	env := testEnv()
	env.Home = f.home
	for _, order := range []string{"alias first", "job first"} {
		p := Policy{Spaces: []Space{
			{Name: "pub", Kind: Public, Roots: []string{"~/oss"}},
			{Name: "personal", Kind: Private, Roots: []string{"~/mirror"}},
			{Name: "acme", Kind: Isolated, Roots: []string{"~/code/company1"}},
			{Name: "globex", Kind: Isolated, Roots: []string{"~/code/globex"}},
		}}
		if order == "job first" {
			slices.Reverse(p.Spaces)
		}
		res, err := Validate(p, env)
		if err != nil {
			t.Fatal(err)
		}
		pol, err := NewGuardPolicy(res, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{filepath.Join(vendor, "x"), filepath.Join(f.globexApp, "secret.txt")} {
			ev := GuardEvent{Kind: EventPreToolUse, Tool: "Read", Cwd: f.acmeApp, Targets: ToolTargets{Paths: []string{target}}}
			if d := Decide(pol, acmePin, Enforcement{}, ev); d.Verdict != Refuse || d.Rule != RuleOtherJobPath {
				t.Errorf("%s, %s: %+v, want other_job_path", order, target, d)
			}
		}
	}
}

func TestNewGuardPolicyRefusesRelativeProtectedPath(t *testing.T) {
	if _, err := NewGuardPolicy(nil, []string{"~/.claude"}); err == nil {
		t.Error("a relative protected path was accepted")
	}
}

// TestDecideRuleRankIgnoresTargetOrder: when several targets refuse
// without ending the scan, the rule is the one ranked first.
func TestDecideRuleRankIgnoresTargetOrder(t *testing.T) {
	f := newGuardFixture(t)
	globex := GuardPin{Found: true, Space: "globex"}
	for _, paths := range [][]string{{"rel", f.personal}, {f.personal, "rel"}} {
		ev := GuardEvent{Kind: EventPreToolUse, Tool: "Read", Cwd: f.globexApp, Targets: ToolTargets{Paths: paths}}
		if d := Decide(f.pol, globex, Enforcement{Guard: GuardStrict}, ev); d.Rule != RuleUnreadableSpace {
			t.Errorf("%v: %+v, want unreadable_space", paths, d)
		}
	}
}

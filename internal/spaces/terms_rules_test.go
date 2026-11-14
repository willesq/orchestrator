package spaces

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// baselineCase is a command whose verdict one baseline rule decides: on is
// the verdict with every rule on, off with that rule alone off. "" is
// allowed.
type baselineCase struct {
	rule    OutboundRule
	cmd     string
	setup   func(t *testing.T) string
	on, off GuardRule
}

func baselineCases(t *testing.T) []baselineCase {
	acme := func(t *testing.T) string { return newTestRepo(t, "acme").dir }
	return []baselineCase{
		{rule: OutboundFailClosed, cmd: `git commit --allow-empty -m "$EDITOR"`, setup: acme,
			on: RuleOutboundUnchecked, off: ""},
		{rule: OutboundGHDefaultRepo, cmd: `gh pr create -t x -b "falcon"`, setup: func(t *testing.T) string {
			r := newTestRepo(t, "acme")
			r.git("remote", "add", "gx", "https://github.com/globex/app.git")
			return r.dir
		}, on: "", off: RuleOutboundTerm},
		{rule: OutboundPlainVariables, cmd: `git -C "$PWD" commit --allow-empty -m "falcon"`, setup: acme,
			on: "", off: RuleOutboundUnchecked},
		{rule: OutboundCommandAssignments, cmd: `M=hello; git commit --allow-empty -m "$M"`, setup: acme,
			on: "", off: RuleOutboundUnchecked},
		{rule: OutboundGitConfigKeys, cmd: `git -c color.ui=never commit --allow-empty -m x`, setup: acme,
			on: "", off: RuleOutboundUnchecked},
		{rule: OutboundPendingWork, cmd: `git add README.md && git commit -m x`, setup: func(t *testing.T) string {
			r := newTestRepo(t, "acme")
			writeFile(t, filepath.Join(r.dir, "README.md"), "hello again\n")
			writeFile(t, filepath.Join(r.dir, "notes.txt"), "the zephyr plan\n")
			return r.dir
		}, on: RuleOutboundTerm, off: ""},
	}
}

func verdictRule(r OutboundResult) GuardRule {
	if !r.Refuse {
		return ""
	}
	return r.Rule
}

// TestOutboundBaselineRuleSwitches is SP-075.1.a1: every baseline rule has
// a stable id and is on by default, and switching one off changes only that
// rule's verdicts. Each row is decided with every rule on, then with each
// rule alone off.
func TestOutboundBaselineRuleSwitches(t *testing.T) {
	cases := baselineCases(t)
	if len(cases) != len(OutboundBaseline) {
		t.Fatalf("%d cases for %d baseline rules", len(cases), len(OutboundBaseline))
	}
	t.Setenv("EDITOR", "")
	dirs := make([]string, len(cases))
	for i, c := range cases {
		if c.rule != OutboundBaseline[i].ID {
			t.Fatalf("case %d is rule %s, baseline has %s", i, c.rule, OutboundBaseline[i].ID)
		}
		if !(*OutboundRules)(nil).On(c.rule) || !(&OutboundRules{}).On(c.rule) {
			t.Errorf("%s is not on by default", c.rule)
		}
		dirs[i] = c.setup(t)
	}
	configs := []OutboundRule{""}
	for _, b := range OutboundBaseline {
		configs = append(configs, b.ID)
	}
	for _, off := range configs {
		chk := termsChecker(t)
		if off != "" {
			chk.Rules = &OutboundRules{Off: []OutboundRule{off}}
		}
		for i, c := range cases {
			want := c.on
			if c.rule == off {
				want = c.off
			}
			if got := verdictRule(check(chk, c.cmd, dirs[i])); got != want {
				t.Errorf("rule %q off: %s row %q: got %q, want %q", off, c.rule, c.cmd, got, want)
			}
		}
	}
}

// TestOutboundUserRulesStricter is SP-075.1.a2 at check time: each kind of
// added rule refuses what the baseline let through, names the rule by id
// only, and stops applying when switched off or removed.
func TestOutboundUserRulesStricter(t *testing.T) {
	repo := newTestRepo(t, "acme")
	base := termsChecker(t)
	add := func(rs OutboundRules, kind, value string) (OutboundRules, string) {
		t.Helper()
		next, u, err := rs.Add(kind, value)
		if err != nil {
			t.Fatalf("add %s: %v", kind, err)
		}
		return next, u.ID
	}
	rs, cmdID := add(OutboundRules{}, "command", "glab")
	rs, destID := add(rs, "destination", "acme/public-sdk")
	rs, failID := add(rs, "fail_closed", `re:mcp__github__(create|update)_\w+`)
	rs, curlID := add(rs, "fail_closed", "api.github.com")
	chk := base
	chk.Rules = &rs

	// The baseline lets these through.
	glab := `glab mr create -d "zephyr notes"`
	sdk := `gh issue create -R acme/public-sdk -t x -b "falcon"`
	curl := `curl -X POST https://api.github.com/repos/acme/app/issues -d x`
	wantAllowed(t, base, repo.dir, glab, sdk, curl)
	mcp := []byte(`{"owner":"acme","repo":"app","title":"x"}`)
	ctx := context.Background()
	if r := base.Check(ctx, "mcp__github__create_issue", mcp, repo.dir); r.Refuse {
		t.Errorf("baseline refused an MCP tool: %+v", r)
	}

	checks := []struct {
		name, tool string
		input      []byte
		rule       GuardRule
		id         string
	}{
		{"command", "Bash", shellInput(glab), RuleOutboundTerm, ""},
		{"destination", "Bash", shellInput(sdk), RuleOutboundTerm, ""},
		{"fail_closed tool", "mcp__github__create_issue", mcp, RuleOutboundUnchecked, failID},
		{"fail_closed text", "Bash", shellInput(curl), RuleOutboundUnchecked, curlID},
	}
	for _, c := range checks {
		r := chk.Check(ctx, c.tool, c.input, repo.dir)
		if !r.Refuse || r.Rule != c.rule || r.UserRule != c.id {
			t.Errorf("%s: %+v, want %s by %q", c.name, r, c.rule, c.id)
		}
		reason := strings.ToLower(r.Reason())
		for _, v := range []string{"glab", "public-sdk", "mcp__github", "api.github.com", "zephyr", "falcon"} {
			if strings.Contains(reason, v) {
				t.Errorf("%s: reason %q names %q (INV-07)", c.name, r.Reason(), v)
			}
		}
		if c.id != "" && !strings.Contains(r.Reason(), c.id) {
			t.Errorf("%s: reason %q does not name rule %s", c.name, r.Reason(), c.id)
		}
	}
	// A command rule reads the command like an outbound one: its own
	// text without a term goes, and what cannot be read fails closed.
	wantAllowed(t, chk, repo.dir, `glab mr create -d "notes"`, `gh issue create -R acme/app -t x -b "falcon"`)
	wantRefused(t, chk, repo.dir, RuleOutboundUnchecked, `glab mr create -d "$X"`)
	// A destination rule on an owner holds every repo of it.
	owner, _ := add(OutboundRules{}, "destination", "acme")
	ownerChk := base
	ownerChk.Rules = &owner
	wantRefused(t, ownerChk, repo.dir, RuleOutboundTerm, `gh issue create -R acme/app -t x -b "falcon"`,
		`git commit --allow-empty -m "falcon"`)

	// Switched off, or removed, a rule no longer applies.
	for _, id := range []string{cmdID, destID, failID, curlID} {
		off, changed, err := rs.Switch(id, false)
		if err != nil || !changed {
			t.Fatalf("switch %s off: %v %v", id, changed, err)
		}
		gone, err := rs.Remove(id)
		if err != nil {
			t.Fatalf("remove %s: %v", id, err)
		}
		for _, cfg := range []OutboundRules{off, gone} {
			c := base
			c.Rules = &cfg
			for _, cc := range checks {
				if cc.name == map[string]string{cmdID: "command", destID: "destination", failID: "fail_closed tool", curlID: "fail_closed text"}[id] {
					if r := c.Check(ctx, cc.tool, cc.input, repo.dir); r.Refuse {
						t.Errorf("%s off or removed: still refused: %+v", id, r)
					}
				}
			}
		}
	}

	// A destination rule never leaves a push's commits unchecked that
	// the baseline checks: commits already on a remote that holds no
	// space (GitLab) still count for a push to a URL the rule covers.
	gl := newTestRepo(t, "acme")
	gl.git("remote", "add", "gl", "https://gitlab.com/x/app.git")
	writeFile(t, filepath.Join(gl.dir, "plan.md"), "the zephyr plan\n")
	gl.git("add", "plan.md")
	gl.git("commit", "-q", "-m", "plan")
	gl.git("update-ref", "refs/remotes/gl/main", "HEAD")
	push := `git push https://github.com/acme/public-sdk.git main`
	wantRefused(t, base, gl.dir, RuleOutboundTerm, push)
	wantRefused(t, chk, gl.dir, RuleOutboundTerm, push)
	// A large call is refused while a fail_closed rule is on, since its
	// patterns could not all be matched in time.
	big := []byte(`{"body":"` + strings.Repeat("a ", maxOutboundText/2+1) + `"}`)
	if r := chk.Check(ctx, "mcp__x__y", big, repo.dir); !r.Refuse || r.UserRule != failID {
		t.Errorf("large call: %+v", r)
	}
	if r := base.Check(ctx, "mcp__x__y", big, repo.dir); r.Refuse {
		t.Errorf("large call, baseline: %+v", r)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if r := chk.Check(done, "mcp__x__y", mcp, repo.dir); !r.Refuse || r.Rule != RuleOutboundUnchecked {
		t.Errorf("out of time: %+v", r)
	}

	// With no terms anywhere nothing is checked, rules or not (INV-10).
	none := OutboundChecker{Policy: base.Policy, Rules: &rs}
	if r := none.Check(ctx, "mcp__github__create_issue", mcp, repo.dir); r.Refuse {
		t.Errorf("no terms: %+v", r)
	}
}

// TestOutboundUserRulesNeverLoosen is SP-075.1.a2: no added rule, of any
// kind, lets through a call the baseline refuses.
func TestOutboundUserRulesNeverLoosen(t *testing.T) {
	repo := newTestRepo(t, "acme")
	base := termsChecker(t)
	t.Setenv("EDITOR", "")
	corpus := []string{
		`git commit --allow-empty -m "zephyr"`,
		`git commit --allow-empty -m "$EDITOR"`,
		`gh issue create -R globex/app -t x -b "falcon"`,
		`gh issue create -R acme/app -t x -b "GX-12"`,
		`eval "git push"`,
		`bash -c 'git commit --allow-empty -m zephyr'`,
		`env git commit --allow-empty -m zephyr`,
		`git -c core.pager=less commit --allow-empty -m x`,
	}
	adds := [][2]string{
		{"command", "glab"}, {"command", "curl"}, {"command", "make"},
		{"destination", "acme"}, {"destination", "globex/app"}, {"destination", "oss-org"},
		{"fail_closed", "deploy"}, {"fail_closed", "re:x{3}"},
	}
	for _, cmd := range corpus {
		want := check(base, cmd, repo.dir)
		if !want.Refuse {
			t.Fatalf("corpus %q is not refused by the baseline", cmd)
		}
		for _, a := range adds {
			rs, _, err := OutboundRules{}.Add(a[0], a[1])
			if err != nil {
				t.Fatal(err)
			}
			c := base
			c.Rules = &rs
			if r := check(c, cmd, repo.dir); !r.Refuse {
				t.Errorf("%s rule let %q through", a[0], cmd)
			}
		}
	}
}

// TestParseUserRuleRejects is SP-075.1.a2 at save time: a rule that would
// allow, exempt or replace how the baseline reads a command is rejected,
// and no error quotes the value.
func TestParseUserRuleRejects(t *testing.T) {
	cases := []struct {
		kind, value string
		want        error
	}{
		{"allow", "glab", ErrRuleLoosens},
		{"exempt", "acme/app", ErrRuleLoosens},
		{"command", "!glab", ErrRuleLoosens},
		{"fail_closed", "!secret-thing", ErrRuleLoosens},
		{"command", "git", ErrBadRule},
		{"command", "GH", ErrBadRule},
		{"command", "bash", ErrBadRule},
		{"command", "sudo", ErrBadRule},
		{"command", "git-push", ErrBadRule},
		{"command", "/usr/bin/glab", ErrBadRule},
		{"command", "glab mr", ErrBadRule},
		{"destination", "*", ErrBadRule},
		{"destination", "acme/*", ErrBadRule},
		{"destination", "https://github.com/acme", ErrBadRule},
		{"fail_closed", "re:", ErrBadRule},
		{"fail_closed", "re:.*", ErrBadRule},
		{"fail_closed", "re:(secret", ErrBadRule},
		{"fail_closed", "# secret", ErrBadRule},
		{"fail_closed", "secret\nother", ErrBadRule},
		{"fail_closed", strings.Repeat("s", maxUserRuleValue+1), ErrBadRule},
		{"secretkind", "secretvalue", ErrBadRule},
		{"command", "", ErrBadRule},
	}
	for _, c := range cases {
		_, err := ParseUserRule(c.kind, c.value)
		if !errors.Is(err, c.want) {
			t.Errorf("%s %q: %v, want %v", c.kind, c.value, err, c.want)
			continue
		}
		msg := strings.ToLower(err.Error())
		for _, leak := range []string{"glab", "secret", "acme", "usr/bin"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s %q: error %q quotes the value", c.kind, c.value, err)
			}
		}
	}
	// Accepted values are stored in their normal form.
	for in, want := range map[[2]string]UserRule{
		{"Command", " Glab "}:                  {Kind: UserRuleCommand, Value: "glab"},
		{"destination", "Acme/Public-SDK.git"}: {Kind: UserRuleDestination, Value: "acme/public-sdk"},
		{"FAIL_CLOSED", "re:Secret-[0-9]+"}:    {Kind: UserRuleFailClosed, Value: "re:Secret-[0-9]+"},
	} {
		got, err := ParseUserRule(in[0], in[1])
		if err != nil || got != want {
			t.Errorf("%v: %+v %v, want %+v", in, got, err, want)
		}
	}
}

// TestGuardSnapshotReadProtected: the guard snapshot carries the rules'
// values, so no tool call may read it (INV-07).
func TestGuardSnapshotReadProtected(t *testing.T) {
	p := GuardProtection("/h", "/h/.config/orch", "/h/data", "/h/data/guard", "", nil)
	if !slicesContains(p.Read, GuardSnapshotPath("/h/data/guard")) {
		t.Errorf("snapshot not read-protected: %v", p.Read)
	}
}

// TestOutboundRulesChanges covers Switch, Add, Remove and Validate.
func TestOutboundRulesChanges(t *testing.T) {
	var rs OutboundRules
	next, changed, err := rs.Switch(string(OutboundPendingWork), false)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	next, _, _ = next.Switch(string(OutboundFailClosed), false)
	if len(next.Off) != 2 || next.Off[0] != OutboundFailClosed || next.Off[1] != OutboundPendingWork {
		t.Errorf("off not in baseline order: %v", next.Off)
	}
	if len(rs.Off) != 0 {
		t.Errorf("Switch changed the receiver: %v", rs.Off)
	}
	if _, changed, _ := next.Switch(string(OutboundFailClosed), false); changed {
		t.Error("switching an off rule off changed it")
	}
	next, changed, _ = next.Switch(string(OutboundFailClosed), true)
	if !changed || !next.On(OutboundFailClosed) || next.On(OutboundPendingWork) {
		t.Errorf("switch on: %+v", next)
	}
	if _, _, err := next.Switch("user-9", false); !errors.Is(err, ErrUnknownRule) {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := next.Remove(string(OutboundFailClosed)); !errors.Is(err, ErrBadRule) {
		t.Errorf("remove baseline: %v", err)
	}
	if _, err := next.Remove("user-1"); !errors.Is(err, ErrUnknownRule) {
		t.Errorf("remove unknown: %v", err)
	}
	a, u1, _ := next.Add("command", "glab")
	a, u2, _ := a.Add("command", "hub")
	if u1.ID != "user-1" || u2.ID != "user-2" {
		t.Errorf("ids %s %s", u1.ID, u2.ID)
	}
	if _, _, err := a.Add("command", "GLAB"); !errors.Is(err, ErrBadRule) || strings.Contains(err.Error(), "glab") {
		t.Errorf("duplicate: %v", err)
	}
	a, _ = a.Remove("user-2")
	a, u3, _ := a.Add("command", "hub")
	if u3.ID != "user-3" {
		t.Errorf("an id was reused: %s", u3.ID)
	}
	if _, changed, _ := a.Switch("user-1", true); changed {
		t.Error("switching an on rule on changed it")
	}
	if err := a.Validate(); err != nil {
		t.Errorf("valid rules: %v", err)
	}
	bad := []OutboundRules{
		{Off: []OutboundRule{"nope"}},
		{User: []UserRule{{ID: "user-1", Kind: UserRuleCommand, Value: "glab"}}},
		{LastID: 2, User: []UserRule{{ID: "x", Kind: UserRuleCommand, Value: "glab"}}},
		{LastID: 2, User: []UserRule{{ID: "user-1", Kind: UserRuleCommand, Value: "glab"}, {ID: "user-1", Kind: UserRuleCommand, Value: "hub"}}},
		{LastID: 1, User: []UserRule{{ID: "user-1", Kind: UserRuleCommand, Value: "git"}}},
		{LastID: 1, User: []UserRule{{ID: "user-1", Kind: UserRuleCommand, Value: "GLAB"}}},
		{LastID: 1, User: make([]UserRule, maxUserRules+1)},
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("bad %d validated", i)
		}
		chk := OutboundChecker{Terms: []TermList{{Space: "x", terms: []term{{}}}}, Rules: &b}
		if r := chk.Check(context.Background(), "Bash", shellInput("git push"), t.TempDir()); !r.Refuse ||
			r.Rule != RuleOutboundUnchecked || !strings.Contains(r.Reason(), "cannot be read") {
			t.Errorf("bad %d: check %+v", i, r)
		}
	}
	full := OutboundRules{}
	for i := range maxUserRules {
		var err error
		full, _, err = full.Add("destination", "o"+strings.Repeat("x", i%30)+"/r"+string(rune('a'+i%26))+strings.Repeat("y", i/26))
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, _, err := full.Add("command", "glab"); !errors.Is(err, ErrBadRule) {
		t.Errorf("over the limit: %v", err)
	}
}

package spaces

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// protectFixture is a home with orch's directories, harness settings, a
// checkout with git hooks, a terms file and a gh profile, and a guard
// policy protecting them as orchd does.
type protectFixture struct {
	home, config, data, checkout, terms, ghDir string
	pol                                        GuardPolicy
}

func newProtectFixture(t *testing.T) protectFixture {
	t.Helper()
	home := realTemp(t)
	f := protectFixture{home: home,
		config:   filepath.Join(home, ".config", "orch"),
		data:     filepath.Join(home, ".local", "share", "orch"),
		checkout: filepath.Join(home, "code", "job", "app"),
		terms:    filepath.Join(home, "terms", "job.txt"),
		ghDir:    filepath.Join(home, ".config", "gh-job"),
	}
	mkdirs(t, f.config, filepath.Join(f.data, "guard", "pins"), filepath.Join(f.checkout, ".git", "hooks"),
		filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), filepath.Join(home, ".gemini"),
		filepath.Join(home, "terms"), f.ghDir, filepath.Join(home, ".ssh"), filepath.Join(home, "scratch"))
	env := testEnv()
	env.Home = home
	res, err := Validate(Policy{Spaces: []Space{
		{Name: "job", Kind: Isolated, Roots: []string{"~/code/job"}, TermsFile: "~/terms/job.txt", GHConfigDir: "~/.config/gh-job"},
		{Name: "oss", Kind: Public, Roots: []string{"~/code/oss"}},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	f.pol, err = NewGuardPolicyProtecting(res, ProtectedPaths(home, f.config, f.data, res))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// decide runs a call from the checkout, pinned to the job's space, in
// block-other-jobs.
func (f protectFixture) decide(tool, input string) GuardDecision {
	ev := GuardEvent{Kind: EventPreToolUse, Tool: tool, Cwd: f.checkout,
		Targets: extractTargets(tool, json.RawMessage(input), f.checkout, f.home)}
	return Decide(f.pol, GuardPin{Found: true, Space: "job"}, GuardEnforcement{}, ev)
}

// protectedTargets are the protected paths SP-042 names, one file each.
func (f protectFixture) protectedTargets() map[string]string {
	return map[string]string{
		"orch config":              filepath.Join(f.config, "AGENTS.md"),
		"orch data (orch.db)":      filepath.Join(f.data, "orch.db"),
		"policy snapshot":          filepath.Join(f.data, "guard", "policy.json"),
		"pin file":                 filepath.Join(f.data, "guard", "pins", "sess.json"),
		"terms file":               f.terms,
		"Claude Code settings":     filepath.Join(f.home, ".claude", "settings.json"),
		"Claude Code local":        filepath.Join(f.home, ".claude", "settings.local.json"),
		"Codex hooks":              filepath.Join(f.home, ".codex", "hooks.json"),
		"Codex config":             filepath.Join(f.home, ".codex", "config.toml"),
		"Gemini settings":          filepath.Join(f.home, ".gemini", "settings.json"),
		"git hook":                 filepath.Join(f.checkout, ".git", "hooks", "pre-commit"),
		"checkout Claude settings": filepath.Join(f.checkout, ".claude", "settings.local.json"),
	}
}

// writeMethods are the ways SP-042.1.a1 names to write a file.
func writeMethods() map[string]func(path string) (string, string) {
	file := func(tool string) func(string) (string, string) {
		return func(p string) (string, string) {
			b, _ := json.Marshal(map[string]any{"file_path": p, "content": "x", "old_string": "a", "new_string": "b",
				"edits": []map[string]string{{"old_string": "a", "new_string": "b"}}})
			return tool, string(b)
		}
	}
	sh := func(format string) func(string) (string, string) {
		return func(p string) (string, string) { return bash(strings.ReplaceAll(format, "P", "'"+p+"'")) }
	}
	return map[string]func(string) (string, string){
		"Write":     file("Write"),
		"Edit":      file("Edit"),
		"MultiEdit": file("MultiEdit"),
		"Bash >":    sh(`echo '{"hooks":{}}' > P`),
		"Bash >>":   sh(`echo x >> P`),
		"tee":       sh(`echo x | tee -a P`),
		"sed -i":    sh(`sed -i '' 's/a/b/' P`),
		"cp":        sh(`cp /tmp/forged P`),
		"mv":        sh(`mv /tmp/forged P`),
	}
}

// TestSelfProtectionRefusesWrites is SP-042.1.a1: every write method into
// every protected path is refused (self_protection).
func TestSelfProtectionRefusesWrites(t *testing.T) {
	f := newProtectFixture(t)
	n := 0
	for pname, path := range f.protectedTargets() {
		for mname, method := range writeMethods() {
			t.Run(pname+"/"+mname, func(t *testing.T) {
				d := f.decide(method(path))
				if d.Verdict != Refuse || d.Rule != RuleSelfProtection {
					t.Errorf("%s into %s: %s (%s)", mname, path, d.Verdict, d.Rule)
				}
			})
			n++
		}
	}
	if n < 30 {
		t.Errorf("%d path x method cases, want at least 30", n)
	}
	t.Logf("%d path x method cases", n)
}

// TestSelfProtectionAllowsReads is SP-042.1.a2: reading a protected path is
// allowed, except a terms file or a credential.
func TestSelfProtectionAllowsReads(t *testing.T) {
	f := newProtectFixture(t)
	read := func(tool, path string) (string, string) {
		if tool == "Read" {
			b, _ := json.Marshal(map[string]string{"file_path": path})
			return "Read", string(b)
		}
		return bash("cat '" + path + "'")
	}
	refused := map[string]bool{"terms file": true}
	for name, path := range f.protectedTargets() {
		for _, tool := range []string{"Read", "Bash"} {
			d := f.decide(read(tool, path))
			if want := refused[name]; (d.Verdict == Refuse) != want || (want && d.Rule != RuleSelfProtection) {
				t.Errorf("%s of %s: %s (%s), refused want %v", tool, name, d.Verdict, d.Rule, want)
			}
		}
	}
	for _, cred := range []string{
		filepath.Join(f.home, ".ssh", "id_ed25519"), filepath.Join(f.home, ".config", "gh", "config.yml"),
		filepath.Join(f.ghDir, "config.yml"), filepath.Join(f.data, "qdrant-api-key"),
		filepath.Join(f.home, ".netrc"), filepath.Join(f.home, ".aws", "credentials"),
	} {
		for _, tool := range []string{"Read", "Bash"} {
			if d := f.decide(read(tool, cred)); d.Verdict != Refuse || d.Rule != RuleSelfProtection {
				t.Errorf("%s of credential %s: %s (%s)", tool, cred, d.Verdict, d.Rule)
			}
		}
	}
}

// TestSelfProtectionForms: more ways to write, and ordinary calls that
// must not be taken for writes to protected paths.
func TestSelfProtectionForms(t *testing.T) {
	f := newProtectFixture(t)
	refused := []string{
		"rm -rf " + f.config,
		"rm -rf ~/.local",
		"mv .git /tmp/old-git",
		"rm -rf .claude",
		"cp -t ~/.codex /tmp/hooks.json",
		"install -m 644 /tmp/x ~/.claude/settings.json",
		"dd if=/tmp/x of=" + filepath.Join(f.data, "orch.db"),
		"truncate -s 0 " + filepath.Join(f.data, "guard", "policy.json"),
		"touch .git/hooks/post-checkout",
		"ln -sf /tmp/evil .git/hooks/pre-push",
		"chmod +x .git/hooks/pre-commit",
		"echo x > ~/.claude/settings-extra.json",
		"echo x > ~/.claude/$NAME",
		"cat ~/.ssh/*",
		"cat ~/.config/gh-job/*",
		"sudo tee " + filepath.Join(f.home, ".codex", "config.toml"),
		"bash -c 'echo {} > ~/.gemini/settings.json'",
		"cd ~/.claude && echo {} > settings.json",
		"apply_patch '*** Begin Patch\n*** Update File: .git/hooks/pre-commit\n*** End Patch'",
	}
	for _, cmd := range refused {
		if d := f.decide(bash(cmd)); d.Verdict != Refuse || d.Rule != RuleSelfProtection {
			t.Errorf("%q: %s (%s), want self_protection", cmd, d.Verdict, d.Rule)
		}
	}
	patch, _ := json.Marshal(map[string]string{"command": "*** Begin Patch\n*** Add File: " +
		filepath.Join(f.home, ".codex", "hooks.json") + "\n+{}\n*** End Patch"})
	if d := f.decide("apply_patch", string(patch)); d.Verdict != Refuse || d.Rule != RuleSelfProtection {
		t.Errorf("apply_patch tool: %s (%s)", d.Verdict, d.Rule)
	}
	allowed := []string{
		"echo x > /tmp/out.txt",
		"go test ./... | tee build.log",
		"sed -i '' 's/a/b/' main.go",
		"cp " + filepath.Join(f.home, ".claude", "settings.json") + " /tmp/settings-copy.json",
		"mv build/out bin/out",
		"git commit -m 'edit .claude/settings.json and .git/hooks'",
		"ls ~/.ssh",
		"GH_CONFIG_DIR=~/.config/gh-job gh pr list",
		"env GH_CONFIG_DIR=" + f.ghDir + " gh repo view",
		"ls ~/.config/gh",
		"rm -rf build",
		"cat .git/hooks/pre-commit.sample",
	}
	for _, cmd := range allowed {
		if d := f.decide(bash(cmd)); d.Verdict != Allow {
			t.Errorf("%q: %s (%s), want allowed", cmd, d.Verdict, d.Rule)
		}
	}
	// A file tool's content may name protected paths; only its file is written.
	b, _ := json.Marshal(map[string]string{"file_path": filepath.Join(f.checkout, "README.md"),
		"content": "edit ~/.claude/settings.json and " + filepath.Join(f.config, "AGENTS.md")})
	if d := f.decide("Write", string(b)); d.Verdict != Allow {
		t.Errorf("Write naming protected paths in its content: %s (%s)", d.Verdict, d.Rule)
	}
}

// TestShellWritesListed: Targets lists what a call writes apart from what
// it reads.
func TestShellWritesListed(t *testing.T) {
	cases := []struct {
		cmd           string
		writes, reads []string
	}{
		{"cat /a > /b", []string{"/b"}, []string{"/a"}},
		{"sort < /a >> /b 2>/c", []string{"/b", "/c"}, []string{"/a"}},
		{"cp /a /b /dir", []string{"/dir"}, []string{"/a", "/b"}},
		{"cp -t /dir /a", []string{"/dir"}, []string{"/a"}},
		{"cp --target-directory=/dir /a", []string{"/dir"}, []string{"/a"}},
		{"mv /a /b", []string{"/a", "/b"}, nil},
		{"tee -a /a /b", []string{"/a", "/b"}, nil},
		{"sed -n p /a", nil, []string{"/a"}},
		{"sed -i.bak s/x/y/ /a", []string{"/a"}, nil},
		{"sed --in-place=.bak s/x/y/ /a", []string{"/a"}, nil},
		{"dd if=/a of=/b", []string{"/b"}, nil},
		{"rm -- -x /a", []string{"/a", testCwd + "/-x"}, nil},
		{"echo x > /nowhere/$X", []string{"/nowhere"}, nil},
		{"echo hi 2>&1", nil, nil},
	}
	for _, c := range cases {
		tool, in := bash(c.cmd)
		got := extractTargets(tool, json.RawMessage(in), testCwd, testHome)
		for _, w := range c.writes {
			if !contains(got.Writes, w) {
				t.Errorf("%q: writes %q, want %q in it", c.cmd, got.Writes, w)
			}
		}
		for _, r := range c.reads {
			if contains(got.Writes, r) {
				t.Errorf("%q: writes %q, want %q only read", c.cmd, got.Writes, r)
			}
		}
		if len(c.writes) == 0 && len(got.Writes) > 0 {
			t.Errorf("%q: writes %q, want none", c.cmd, got.Writes)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestProtectionValidation: entries must be absolute (Write, Read) or
// relative (Anywhere); without spaces nothing is protected (INV-10).
func TestProtectionValidation(t *testing.T) {
	res := mustValid(t, Policy{Spaces: []Space{{Name: "job", Kind: Isolated, Roots: []string{"/j"}}}}, testEnv())
	for _, p := range []Protection{{Write: []string{"rel"}}, {Read: []string{"rel"}}, {Anywhere: []string{"/abs"}}, {Anywhere: []string{""}}} {
		if _, err := NewGuardPolicyProtecting(res, p); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	pol, err := NewGuardPolicyProtecting(nil, ProtectedPaths("/h", "/h/.config/orch", "/h/.local/share/orch", nil))
	if err != nil {
		t.Fatal(err)
	}
	tool, in := bash("echo x > /h/.claude/settings.json")
	ev := GuardEvent{Kind: EventPreToolUse, Tool: tool, Cwd: "/h", Targets: extractTargets(tool, json.RawMessage(in), "/h", "/h")}
	if d := Decide(pol, GuardPin{}, GuardEnforcement{}, ev); d.Verdict != Allow || d.Rule != RuleNoPolicy {
		t.Errorf("no spaces: %s (%s)", d.Verdict, d.Rule)
	}
	if p := ProtectedPaths("/h", "/c", "/d", nil); len(p.Read) == 0 || len(p.Write) == 0 || len(p.Anywhere) == 0 {
		t.Errorf("ProtectedPaths without a policy = %+v", p)
	}
}

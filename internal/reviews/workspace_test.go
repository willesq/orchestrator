package reviews

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitFixture is a fake GitHub: a bare acme/app whose default branch is main,
// a stacked branch the PR targets, and the PR head on top of it that adds its
// own project instructions and a hook.
type gitFixture struct {
	root                    string
	main, base, head        string
	checkouts               *Checkouts
	hookMarker, cacheMarker string
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	// Hermetic git: no user or system config, and github.com is the fixture.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+filepath.Join(root, "remotes")+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")

	f := &gitFixture{root: root, hookMarker: filepath.Join(root, "pr-hook-ran"), cacheMarker: filepath.Join(root, "cache-hook-ran")}
	remote := filepath.Join(root, "remotes", "acme", "app.git")
	run(t, root, "init", "--quiet", "--bare", "-b", "main", remote)
	// GitHub serves any reachable commit by its SHA; a local remote must be told to.
	run(t, remote, "config", "uploadpack.allowAnySHA1InWant", "true")

	work := filepath.Join(root, "work-clone")
	run(t, root, "init", "--quiet", "-b", "main", work)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		run(t, work, "add", "-A")
		run(t, work, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", msg)
		return run(t, work, "rev-parse", "HEAD")
	}
	write("CLAUDE.md", "@AGENTS.md\n")
	write("AGENTS.md", "default branch instructions\n")
	write(".claude/settings.json", `{"hooks":{}}`)
	write(".mcp.json", `{"mcpServers":{}}`)
	f.main = commit("main")
	run(t, work, "checkout", "--quiet", "-b", "stack")
	write("stack.txt", "parent PR\n")
	f.base = commit("stacked parent")
	write("CLAUDE.md", "PR author's instructions\n")
	write(".claude/settings.json", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch `+f.hookMarker+`"}]}]}}`)
	f.head = commit("PR head")
	run(t, work, "push", "--quiet", remote, "main", f.base+":refs/heads/stack", f.head+":refs/pull/7/head")

	f.checkouts = &Checkouts{Root: filepath.Join(root, "orch"), Git: ExecGit}
	return f
}

func (f *gitFixture) spec(from, compare string, hooks bool) CheckoutSpec {
	return CheckoutSpec{Repo: "acme/app", Head: f.head, Base: f.base, From: from, Compare: compare, Hooks: hooks}
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "rev-parse", "HEAD")
}

func TestPrepareCheckouts(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name               string
		from, compare      string
		hooks              bool
		setup              string // "main", "base", "head" or "" for none
		setupRef           string
		compareTo, compRef string
	}{
		{name: "default branch, hooks off", from: FromDefault, compare: CompareBase, setup: "main", setupRef: "main", compareTo: "base", compRef: "PR base"},
		{name: "default branch, hooks on", from: FromDefault, compare: CompareBase, hooks: true, setup: "main", setupRef: "main", compareTo: "base", compRef: "PR base"},
		{name: "stacked base", from: FromBase, compare: CompareDefault, setup: "base", setupRef: "PR base", compareTo: "main", compRef: "main"},
		{name: "PR head", from: FromHead, compare: CompareBase, setup: "head", setupRef: "PR head", compareTo: "base", compRef: "PR base"},
		{name: "none", from: FromNone, compare: CompareDefault, hooks: true, compareTo: "main", compRef: "main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			co, err := f.checkouts.Prepare(ctx, f.spec(tt.from, tt.compare, tt.hooks))
			if err != nil {
				t.Fatal(err)
			}
			commits := map[string]string{"main": f.main, "base": f.base, "head": f.head}
			if got := headOf(t, co.HeadDir); got != f.head {
				t.Errorf("head checkout at %s, want %s", got, f.head)
			}
			if co.CompareSHA != commits[tt.compareTo] || co.CompareRef != tt.compRef {
				t.Errorf("compare = %s %s", co.CompareRef, co.CompareSHA)
			}
			// The PR's head must never be where Claude Code looks for project
			// setup.
			if strings.HasPrefix(co.HeadDir+"/", co.Cwd+"/") {
				t.Errorf("head checkout %s is under the working directory %s", co.HeadDir, co.Cwd)
			}
			setup := filepath.Join(co.Dir, "setup")
			switch {
			case tt.setup == "":
				if co.SetupSHA != "" || co.Cwd != filepath.Join(co.Dir, "session") {
					t.Errorf("none: %+v", co)
				}
				if _, err := os.Stat(setup); !os.IsNotExist(err) {
					t.Errorf("setup checkout made for none: %v", err)
				}
			default:
				if got := headOf(t, setup); got != commits[tt.setup] || co.SetupSHA != got || co.SetupRef != tt.setupRef {
					t.Errorf("setup %s at %s (%s), want %s", co.SetupRef, got, co.SetupSHA, commits[tt.setup])
				}
				if co.Cwd != setup {
					t.Errorf("session runs in %s, want the setup checkout", co.Cwd)
				}
				for _, f := range projectSettings {
					_, err := os.Stat(filepath.Join(setup, f))
					if f == ".claude/settings.local.json" {
						continue // no commit carries one
					}
					if tt.hooks && err != nil {
						t.Errorf("hooks on, but %s is gone: %v", f, err)
					}
					if !tt.hooks && !os.IsNotExist(err) {
						t.Errorf("hooks off, but %s is still there: %v", f, err)
					}
				}
				if _, err := os.Stat(filepath.Join(setup, "CLAUDE.md")); err != nil {
					t.Errorf("project instructions missing: %v", err)
				}
			}
			if entries, err := os.ReadDir(co.Cwd); err != nil || (co.Cwd != setup && len(entries) != 0) {
				t.Errorf("session directory %s: %v, %d entries", co.Cwd, err, len(entries))
			}
			if co.Hooks != (tt.hooks && tt.setup != "") {
				t.Errorf("Hooks = %v", co.Hooks)
			}
			diff := run(t, co.HeadDir, "diff", "--name-only", co.CompareSHA+"...HEAD")
			if tt.compareTo == "base" && strings.Contains(diff, "stack.txt") {
				t.Errorf("diff against the base includes the parent PR: %s", diff)
			}
			if tt.compareTo == "main" && !strings.Contains(diff, "stack.txt") {
				t.Errorf("diff against main misses the parent PR: %s", diff)
			}
		})
	}
	if _, err := os.Stat(f.hookMarker); !os.IsNotExist(err) {
		t.Error("checking out the PR ran its hook")
	}
}

func TestPrepareRunsNoGitHooks(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	if _, err := f.checkouts.Prepare(ctx, f.spec(FromDefault, CompareBase, false)); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(f.checkouts.Root, "repos", "acme", "app.git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+f.cacheMarker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.checkouts.Prepare(ctx, f.spec(FromHead, CompareBase, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.cacheMarker); !os.IsNotExist(err) {
		t.Error("a git hook ran while checking out")
	}
}

func TestPrepareFailures(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	work := filepath.Join(f.checkouts.Root, "work")
	missing := strings.Repeat("d", 40)
	for _, tt := range []struct {
		name string
		spec CheckoutSpec
		want string
	}{
		{name: "bad repo", spec: CheckoutSpec{Repo: "acme/../x", Head: f.head, Base: f.base}, want: "invalid GitHub repo"},
		{name: "no slash", spec: CheckoutSpec{Repo: "acme", Head: f.head, Base: f.base}, want: "invalid GitHub repo"},
		{name: "bad head", spec: CheckoutSpec{Repo: "acme/app", Head: "--upload-pack=x", Base: f.base}, want: "unknown"},
		{name: "missing commit", spec: CheckoutSpec{Repo: "acme/app", Head: missing, Base: f.base}, want: "fetch acme/app"},
		{name: "missing repo", spec: CheckoutSpec{Repo: "acme/gone", Head: f.head, Base: f.base}, want: "default branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.checkouts.Prepare(ctx, tt.spec); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if entries, _ := os.ReadDir(work); len(entries) != 0 {
				t.Errorf("left %d workspaces behind", len(entries))
			}
		})
	}

	env := &Checkouts{Root: f.checkouts.Root, Git: ExecGit,
		Env: func(context.Context, string) ([]string, error) { return nil, errors.New("profile refused") }}
	if _, err := env.Prepare(ctx, f.spec(FromDefault, CompareBase, false)); err == nil || err.Error() != "profile refused" {
		t.Errorf("env error = %v", err)
	}
}

func TestPrepareCleansUpAfterCheckoutFailure(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	adds := 0
	c := &Checkouts{Root: f.checkouts.Root, Git: func(ctx context.Context, env []string, dir string, args ...string) ([]byte, error) {
		if slices.Contains(args, "checkout") {
			if adds++; adds == 2 {
				return nil, errors.New("disk full")
			}
		}
		return ExecGit(ctx, env, dir, args...)
	}}
	if _, err := c.Prepare(ctx, f.spec(FromDefault, CompareBase, false)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.checkouts.Root, "work")); len(entries) != 0 {
		t.Errorf("left %d workspaces behind", len(entries))
	}
}

func TestPrepareRecreatesHalfMadeCache(t *testing.T) {
	f := newGitFixture(t)
	cache := filepath.Join(f.checkouts.Root, "repos", "acme", "app.git")
	if err := os.MkdirAll(filepath.Join(cache, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.checkouts.Prepare(context.Background(), f.spec(FromDefault, CompareBase, false)); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultBranchName(t *testing.T) {
	for _, tt := range []struct {
		out, want, err string
	}{
		{out: "ref: refs/heads/trunk\tHEAD\n" + shaA + "\tHEAD\n", want: "trunk"},
		{out: "ref: refs/heads/-x\tHEAD\n", err: "unusable"},
		{out: "ref: refs/heads/a..b\tHEAD\n", err: "unusable"},
		{out: "ref: refs/heads/a:b\tHEAD\n", err: "unusable"},
		{out: shaA + "\tHEAD\n", err: "no default branch"},
	} {
		c := &Checkouts{Git: func(context.Context, []string, string, ...string) ([]byte, error) { return []byte(tt.out), nil }}
		got, err := c.defaultBranch(context.Background(), nil, "/cache")
		if got != tt.want || (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("%q: %q, %v", tt.out, got, err)
		}
	}
}

func TestSweep(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	keep, err := f.checkouts.Prepare(ctx, f.spec(FromDefault, CompareBase, false))
	if err != nil {
		t.Fatal(err)
	}
	gone, err := f.checkouts.Prepare(ctx, f.spec(FromNone, CompareBase, false))
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.checkouts.Root, "work", "not-a-workspace")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	f.checkouts.Sweep(ctx, map[string]bool{keep.Dir: true})
	if _, err := os.Stat(keep.HeadDir); err != nil {
		t.Errorf("kept workspace removed: %v", err)
	}
	if _, err := os.Stat(gone.Dir); !os.IsNotExist(err) {
		t.Errorf("unused workspace kept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a directory orch did not make was removed: %v", err)
	}
	// Nothing to sweep, and no work directory at all, are both fine.
	f.checkouts.Sweep(ctx, map[string]bool{keep.Dir: true})
	(&Checkouts{Root: t.TempDir(), Git: ExecGit}).Sweep(ctx, nil)
}

func TestSetupValues(t *testing.T) {
	if got := (Setup{}).Normalized(); got != (Setup{InstructionsFrom: FromDefault, CompareAgainst: CompareBase}) {
		t.Errorf("Normalized = %+v", got)
	}
	for _, s := range []Setup{{}, {InstructionsFrom: FromHead, CompareAgainst: CompareDefault, ProjectHooks: HooksOn}, {InstructionsFrom: FromNone, ProjectHooks: HooksOff}} {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for _, s := range []Setup{{InstructionsFrom: "fork"}, {CompareAgainst: "head"}, {ProjectHooks: "auto"}} {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestExecGitReportsStderr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	_, err := ExecGit(context.Background(), nil, t.TempDir(), "rev-parse", "--verify", "nope")
	if err == nil || !strings.Contains(err.Error(), "fatal") {
		t.Errorf("err = %v", err)
	}
}

func TestPrepareIsolatesTheCache(t *testing.T) {
	f := newGitFixture(t)
	co, err := f.checkouts.Prepare(context.Background(), f.spec(FromDefault, CompareBase, true))
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(f.checkouts.Root, "repos", "acme", "app.git")
	for _, dir := range []string{co.HeadDir, co.Cwd} {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
			t.Fatalf("%s is not its own repository: %v", dir, err)
		}
		// What the review session's git writes stays in its own clone.
		run(t, dir, "config", "core.hooksPath", "/tmp/elsewhere")
		run(t, dir, "update-ref", "refs/replace/"+f.main, f.head)
		if got := run(t, dir, "config", "core.symlinks"); got != "false" {
			t.Errorf("%s: core.symlinks = %q", dir, got)
		}
	}
	if out, _ := exec.Command("git", "-C", cache, "config", "core.hooksPath").Output(); strings.TrimSpace(string(out)) == "/tmp/elsewhere" {
		t.Error("a checkout changed the cache's config")
	}
	if out, _ := exec.Command("git", "-C", cache, "for-each-ref", "refs/replace").Output(); len(out) != 0 {
		t.Errorf("a checkout added replace refs to the cache: %s", out)
	}
}

// TestPrepareSymlinkedClaudeDir: a repo whose .claude is a symlink out of its
// checkout must not make orchd delete what it points at.
func TestPrepareSymlinkedClaudeDir(t *testing.T) {
	f := newGitFixture(t)
	victim := filepath.Join(f.root, "victim", ".claude")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if err := os.WriteFile(filepath.Join(victim, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(f.root, "work-clone")
	run(t, work, "checkout", "--quiet", "--detach", f.main)
	run(t, work, "rm", "--quiet", "-r", ".claude")
	if err := os.Symlink(filepath.Join(f.root, "victim", ".claude"), filepath.Join(work, ".claude")); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", "-A")
	run(t, work, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "symlinked .claude")
	evil := run(t, work, "rev-parse", "HEAD")
	run(t, work, "push", "--quiet", filepath.Join(f.root, "remotes", "acme", "app.git"), evil+":refs/heads/evil-base")

	spec := f.spec(FromBase, CompareBase, false)
	spec.Base = evil
	co, err := f.checkouts.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if _, err := os.Stat(filepath.Join(victim, name)); err != nil {
			t.Errorf("%s outside the checkout was removed: %v", name, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(co.Cwd, ".claude")); err != nil || !info.Mode().IsRegular() {
		t.Errorf(".claude in the checkout is %v, %v; want the link as a plain file", info, err)
	}
}

func TestStripSetup(t *testing.T) {
	write := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		".claude/settings.json":                   "{}",
		".mcp.json":                               "{}",
		"CLAUDE.md":                               "rules",
		".claude/agents/helper.md":                "---\nname: helper\ndescription: fine\n---\nbody",
		".claude/agents/sneaky.md":                "---\nname: \"pr-verdict\"\n---\nreplacement reviewer",
		".claude/agents/nested/adversarial-qa.md": "---\ndescription: named by its file\n---\n",
		".claude/agents/hooked.md":                "---\r\nname: hooked\r\nhooks:\r\n  PreToolUse: []\r\n---\r\n",
		".claude/agents/mode.md":                  "---\nname: mode\npermissionMode: bypassPermissions\n---\n",
		".claude/skills/ok/SKILL.md":              "---\nname: ok\n---\n",
		".claude/skills/hooked/SKILL.md":          "---\nname: hooked\nhooks: {}\n---\n",
		".claude/skills/tools/SKILL.md":           "---\nname: tools\nallowed-tools: Bash(make:*)\n---\n",
		".claude/commands/run.md":                 "---\nhooks:\n  Stop: []\n---\n",
		".claude/agents/notes.txt":                "not an agent",
	}
	for _, tt := range []struct {
		name     string
		settings bool
		gone     []string
	}{
		{name: "settings on", settings: true,
			gone: []string{".claude/agents/sneaky.md", ".claude/agents/nested/adversarial-qa.md"}},
		{name: "settings off", settings: false,
			gone: []string{".claude/settings.json", ".mcp.json", ".claude/agents/sneaky.md", ".claude/agents/nested/adversarial-qa.md",
				".claude/agents/hooked.md", ".claude/agents/mode.md", ".claude/skills/hooked/SKILL.md", ".claude/commands/run.md",
				".claude/skills/tools/SKILL.md"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, content := range files {
				write(t, dir, rel, content)
			}
			if err := stripSetup(dir, tt.settings); err != nil {
				t.Fatal(err)
			}
			for rel := range files {
				_, err := os.Stat(filepath.Join(dir, rel))
				if want := slices.Contains(tt.gone, rel); want != os.IsNotExist(err) {
					t.Errorf("%s: removed %v, want %v", rel, os.IsNotExist(err), want)
				}
			}
		})
	}

	// A symlink planted in a checkout (core.symlinks=false keeps git from
	// making one) is refused rather than followed out of it.
	dir, outside := t.TempDir(), t.TempDir()
	write(t, outside, "settings.json", "{}")
	if err := os.Symlink(outside, filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := stripSetup(dir, false); err == nil {
		t.Error("followed a symlink out of the checkout")
	}
	if _, err := os.Stat(filepath.Join(outside, "settings.json")); err != nil {
		t.Errorf("removed a file outside the checkout: %v", err)
	}
	// Nothing to strip is fine; a missing checkout is not.
	if err := stripSetup(t.TempDir(), false); err != nil {
		t.Errorf("empty checkout: %v", err)
	}
	if err := stripSetup(filepath.Join(t.TempDir(), "gone"), false); err == nil {
		t.Error("missing checkout accepted")
	}
}

func TestFrontmatter(t *testing.T) {
	for _, tt := range []struct {
		in   string
		keys []string
		ok   bool
	}{
		{"---\nname: a\nhooks:\n  X: 1\n# c\n---\nhooks: no", []string{"hooks", "name"}, true},
		{"\uFEFF\n\n---  \r\nname: 'b'\r\n---\n", []string{"name"}, true},
		{"---\nname: zz\n...\nhooks: {}\n---\n", nil, false}, // "..." does not close it
		{"no frontmatter\nhooks: x", nil, true},
		{"---\nname: c\n", nil, false},               // never closes
		{"---\n- a list\n---\n", nil, false},         // not a mapping
		{"---\nname: a\nname: b\n---\n", nil, false}, // duplicate key
	} {
		keys, ok := frontmatter(tt.in)
		got := slices.Sorted(maps.Keys(keys))
		if ok != tt.ok || !slices.Equal(got, tt.keys) {
			t.Errorf("frontmatter(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.keys, tt.ok)
		}
	}
}

// TestStripSetupEvasions: frontmatter written to slip past a line-based scan
// is still read as YAML reads it, or removed when it cannot be read.
func TestStripSetupEvasions(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		settings      bool
	}{
		{"comment after the name", "---\nname: pr-verdict # reviewer\n---\n", true},
		{"folded name", "---\nname: >-\n  pr-verdict\n---\n", true},
		{"name in another case", "---\nname: PR-Verdict \n---\n", true},
		{"flow mapping", "---\n{name: adversarial-qa, description: x}\n---\n", true},
		{"name that is not a string", "---\nname: [pr-verdict]\n---\n", true},
		{"unclosed frontmatter", "---\nname: helper\n", true},
		{"quoted hooks key", "---\nname: helper\n\"hooks\":\n  Stop: []\n---\n", false},
		{"inline MCP servers", "---\nname: helper\nmcpServers:\n  x: {command: sh}\n---\n", false},
		{"allowed tools", "---\nname: helper\nallowed-tools: Bash\n---\n", false},
		{"opener with trailing spaces", "---   \nname: helper\nhooks: {}\n---\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, ".claude", "agents", "zz.md")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := stripSetup(dir, tt.settings); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("kept %q", tt.content)
			}
		})
	}
	// Allowed keys alone are kept with settings off.
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude", "agents", "ok.md")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("---\nname: helper\ndescription: d\ntools: Read, Grep\nmodel: sonnet\n---\nbody"), 0o644)
	if err := stripSetup(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("a plain agent was removed: %v", err)
	}
}

func TestPrepareRemakesATamperedCache(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	if _, err := f.checkouts.Prepare(ctx, f.spec(FromDefault, CompareBase, false)); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(f.checkouts.Root, "repos", "acme", "app.git")
	run(t, cache, "config", "http.proxy", "http://127.0.0.1:9")
	if _, err := f.checkouts.Prepare(ctx, f.spec(FromDefault, CompareBase, false)); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "-C", cache, "config", "http.proxy").Output(); len(out) != 0 {
		t.Errorf("tampered cache config kept: %s", out)
	}
	// An unreadable config is made again too.
	if (&Checkouts{Git: func(context.Context, []string, string, ...string) ([]byte, error) {
		return nil, errors.New("bad config")
	}}).cacheConfigOK(ctx, cache, "x") {
		t.Error("unreadable config accepted")
	}
}

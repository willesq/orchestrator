package spaces

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestGitConfigParsing covers git's config syntax as the SP-043 review
// found it could be used to hide a remote.
func TestGitConfigParsing(t *testing.T) {
	cfg, err := parseGitConfig([]byte(`# comment
; another
[remote "a"] url = git@github.com:globex/app.git ; old remote
[remote "b"]
	url = "https://github.com/globex/b.git" # quoted, then a comment
	pushurl=git@github.com:globex/b-push.git
[remote.legacy]
	url = git@github.com:globex/legacy.git
[remote "cont"]
	url = https://github.com/glo\
bex/cont.git
[REMOTE "Case"]
	URL = git@github.com:globex/case.git
[remotes]
	both = a b
[branch "main"]
	remote = b
	pushRemote = a
[remote]
	pushDefault = legacy
[core]
	bare
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"a":      {"git@github.com:globex/app.git"},
		"b":      {"https://github.com/globex/b.git", "git@github.com:globex/b-push.git"},
		"legacy": {"git@github.com:globex/legacy.git"},
		"cont":   {"https://github.com/globex/cont.git"},
		"Case":   {"git@github.com:globex/case.git"},
	}
	for name, urls := range want {
		if !slices.Equal(cfg.remotes[name], urls) {
			t.Errorf("remote %s = %q, want %q", name, cfg.remotes[name], urls)
		}
	}
	if !slices.Equal(cfg.groups["both"], []string{"a", "b"}) || cfg.branchRemote["main"] != "b" ||
		cfg.pushRemote["main"] != "a" || cfg.pushDefault != "legacy" || cfg.rewrites {
		t.Errorf("config = %+v", cfg)
	}
	for _, rw := range []string{"[url \"git@github.com:globex/\"]\n\tinsteadOf = gh:", "[include]\n\tpath = x", "[includeIf \"gitdir:~/\"]\n\tpath = y"} {
		if c, err := parseGitConfig([]byte(rw)); err != nil || !c.rewrites {
			t.Errorf("%q: rewrites = %v, %v", rw, c != nil && c.rewrites, err)
		}
	}
	for _, bad := range []string{"[remote \"x\"\n", "[ ]", "[remote \"x\"]\n\turl = \"open", "[remote \"x\"]\n\turl = a\\q", "[remote \"x]", "[remote \"x\" ]", "=x", "[a.b \"c\"]", "[r]\n\tkey value", "[remote \"x\\"} {
		if _, err := parseGitConfig([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if v, err := parseValue(`a\tb\n\"\\ c  `); err != nil || v != "a\tb\n\"\\ c" {
		t.Errorf("escapes: %q %v", v, err)
	}
	if v, err := parseValue(`x\`); err == nil {
		t.Errorf("dangling backslash: %q", v)
	}
}

// TestGitRemoteReviewCases are the SP-043 review's reproducers: each now
// reaches the other job's repo (or is opaque where it cannot be placed).
func TestGitRemoteReviewCases(t *testing.T) {
	f := newGuardFixture(t)
	cfgPath := filepath.Join(f.acmeApp, ".git", "config")
	head := filepath.Join(f.acmeApp, ".git", "HEAD")
	writeFile(t, head, "ref: refs/heads/main\n")
	refused := []struct{ config, cmd string }{
		{"[remote \"x\"]\n\turl = git@github.com:globex/app.git ; old", "git fetch x"},
		{"[remote \"x\"]\n\turl = git@github.com:globex/app.git # c", "git fetch x"},
		{"[remote \"x\"] url = git@github.com:globex/app.git", "git fetch x"},
		{"[remote.x]\n\turl = git@github.com:globex/app.git", "git fetch x"},
		{"[remote \"x\"]\n\turl = https://github.com/glo\\\nbex/app.git", "git fetch x"},
		{"[remote \"x\"]\n\turl = git+ssh://git@github.com/globex/app", "git fetch x"},
		{"[remote \"x\"]\n\turl = " + f.globexApp, "git fetch x"},
		{"[remote \"x\"]\n\turl = file://" + f.globexApp, "git pull x main"},
		{"[remote \"1\"]\n\turl = git@github.com:acme/ok.git\n[remote \"x\"]\n\turl = git@github.com:globex/app.git", "git fetch --depth 1 x"},
		{"[remote \"o\"]\n\turl = git@github.com:acme/ok.git\n[remote \"x\"]\n\turl = git@github.com:globex/app.git", "git fetch --multiple o x"},
		{"[remote \"x/y\"]\n\turl = git@github.com:globex/app.git", "git push x/y"},
		{"[remote \"x\"]\n\turl = git@github.com:globex/app.git\n[remotes]\n\tgrp = x", "git fetch grp"},
		{"[remote \"x\"]\n\turl = git@github.com:globex/app.git", "git remote update"},
		{"[remote \"x\"]\n\turl = git@github.com:globex/app.git", "git fetch --all"},
		{"[remote \"x\"]\n\turl = git@github.com:globex/app.git", "git push --repo=x"},
		{"[remote \"origin\"]\n\turl = git@github.com:acme/ok.git\n[remote \"x\"]\n\turl = git@github.com:globex/app.git\n[branch \"main\"]\n\tpushRemote = x", "git push"},
	}
	for _, c := range refused {
		writeFile(t, cfgPath, c.config+"\n")
		tool, in := bash(c.cmd)
		ev := f.event(tool, in, f.acmeApp)
		if d := Decide(f.pol, acmePin, GuardEnforcement{}, ev); d.Verdict != Refuse ||
			(d.Rule != RuleOtherJobRepo && d.Rule != RuleOtherJobPath) {
			t.Errorf("%q with %q: %s (%s); targets %+v", c.cmd, c.config, d.Verdict, d.Rule, ev.Targets)
		}
	}
	opaque := []struct{ config, cmd string }{
		{"[remote \"x\"]\n\turl = globexgh:globex/app.git", "git fetch x"},
		{"[remote \"x\"]\n\turl = git@gitlab.example:globex/app.git", "git fetch x"},
		{"[remote \"x\"]\n\turl = weird remote", "git fetch x"},
		{"[remote \"x\"]\n\turl = git@github.com:acme/ok.git\n[url \"git@github.com:globex/\"]\n\tinsteadOf = git@github.com:acme/", "git fetch x"},
		{"[remote \"x\"]\n\turl = git@github.com:acme/ok.git", "git fetch nosuch"},
		{"[remote \"x\"\n", "git fetch x"},
	}
	for _, c := range opaque {
		writeFile(t, cfgPath, c.config+"\n")
		tool, in := bash(c.cmd)
		if ev := f.event(tool, in, f.acmeApp); !ev.Targets.Opaque {
			t.Errorf("%q with %q: not opaque; targets %+v", c.cmd, c.config, ev.Targets)
		}
	}
	// The branch's own remote, not every remote, is the default.
	writeFile(t, cfgPath, "[remote \"origin\"]\n\turl = git@github.com:acme/app.git\n[remote \"up\"]\n\turl = git@github.com:globex/app.git\n[branch \"main\"]\n\tremote = origin\n")
	for _, cmd := range []string{"git push", "git fetch", "git pull", "git -C . fetch"} {
		tool, in := bash(cmd)
		if d := Decide(f.pol, acmePin, GuardEnforcement{}, f.event(tool, in, f.acmeApp)); d.Verdict != Allow {
			t.Errorf("%q in a checkout whose upstream is another job: %s (%s)", cmd, d.Verdict, d.Rule)
		}
	}
	// --git-dir names the config read.
	other := filepath.Join(f.scratch, "copy.git")
	mkdirs(t, other)
	writeFile(t, filepath.Join(other, "config"), "[remote \"origin\"]\n\turl = git@github.com:globex/app.git\n")
	tool, in := bash("git --git-dir=" + other + " fetch origin")
	if d := Decide(f.pol, acmePin, GuardEnforcement{}, f.event(tool, in, f.acmeApp)); d.Verdict != Refuse || d.Rule != RuleOtherJobRepo {
		t.Errorf("--git-dir: %s (%s)", d.Verdict, d.Rule)
	}
	// Oversized and unreadable configs are opaque, never partly read.
	writeFile(t, cfgPath, "[core]\n"+strings.Repeat("# pad\n", maxGitConfig/6+10)+"[remote \"x\"]\n\turl = git@github.com:globex/app.git\n")
	tool, in = bash("git fetch x")
	if ev := f.event(tool, in, f.acmeApp); !ev.Targets.Opaque {
		t.Errorf("oversized config: %+v", ev.Targets)
	}
	_ = os.Remove(cfgPath)
	if err := syscall.Mkfifo(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if ev := f.event(tool, in, f.acmeApp); !ev.Targets.Opaque {
		t.Errorf("FIFO config: %+v", ev.Targets)
	}
	_ = os.Remove(cfgPath)
}

// TestGitDirSymlink: a .git that is a symlink to a directory is followed,
// as git follows it.
func TestGitDirSymlink(t *testing.T) {
	dir := realTemp(t)
	real := filepath.Join(dir, "store.git")
	mkdirs(t, real, filepath.Join(dir, "co"))
	writeFile(t, filepath.Join(real, "config"), "[remote \"origin\"]\n\turl = git@github.com:Globex/app.git\n")
	if err := os.Symlink(real, filepath.Join(dir, "co", ".git")); err != nil {
		t.Fatal(err)
	}
	tool, in := bash("git fetch origin")
	got := extractTargets(tool, []byte(in), filepath.Join(dir, "co"), dir)
	if !slices.Contains(got.GHRepos, "globex/app") {
		t.Errorf("repos %q", got.GHRepos)
	}
}

// TestGitHubURLReviewCases: account URLs in other spellings are read, and
// text ending in a URL is not taken for a malformed target.
func TestGitHubURLReviewCases(t *testing.T) {
	f := newGuardFixture(t)
	refused := []string{
		"open https://www.github.com/globex",
		"echo '<https://github.com/globex>'",
		"curl https://gist.github.com/globex",
		"git commit -m 'Moved from https://github.com/globex.'",
		"echo github.com/globex, then",
	}
	for _, cmd := range refused {
		tool, in := bash(cmd)
		if d := Decide(f.pol, acmePin, GuardEnforcement{}, f.event(tool, in, f.acmeApp)); d.Verdict != Refuse || d.Rule != RuleOtherJobRepo {
			t.Errorf("%q: %s (%s)", cmd, d.Verdict, d.Rule)
		}
	}
	allowed := []string{
		"git commit -m 'Moved from https://github.com/foo.'",
		"echo see github.com/foo.bar",
		`gh search code 'org:foo. thing'`,
		"echo https://github.com/acme",
	}
	for _, cmd := range allowed {
		tool, in := bash(cmd)
		if d := Decide(f.pol, acmePin, GuardEnforcement{}, f.event(tool, in, f.acmeApp)); d.Verdict != Allow {
			t.Errorf("%q: %s (%s)", cmd, d.Verdict, d.Rule)
		}
	}
}

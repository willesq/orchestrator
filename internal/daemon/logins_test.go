package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaude answers 'claude auth status --json' per CLAUDE_CONFIG_DIR.
type fakeClaude struct {
	mu       sync.Mutex
	answers  map[string]string // dir ("" default) -> output
	failures map[string]error
	envs     [][]string
}

func (f *fakeClaude) run(_ context.Context, env []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envs = append(f.envs, env)
	dir := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			dir = v
		}
	}
	if err := f.failures[dir]; err != nil {
		return nil, err
	}
	return []byte(f.answers[dir]), nil
}

const claudeIn = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"someone@example.com","orgName":"Example Org"}`

func loginsServer(t *testing.T, doc string, fc *fakeClaude) *server {
	t.Helper()
	s, _ := profilesServer(t, doc)
	s.claudeLogins = newClaudeLogins()
	s.claudeLogins.run = fc.run
	return s
}

func loginOf(t *testing.T, ls []spaceLogin, space string) spaceLogin {
	t.Helper()
	for _, l := range ls {
		if l.Space == space {
			return l
		}
	}
	t.Fatalf("no login for %s in %+v", space, ls)
	return spaceLogin{}
}

// TestSpaceLoginsDetection is SP-074.3.a2: for each space orch reports which
// gh login and Claude Code configuration it uses (the default or its own)
// and whether each is logged in, in one-login and two-login layouts, from
// 'gh auth status' and 'claude auth status' metadata only: no token is asked
// for and no email or organization is kept.
func TestSpaceLoginsDetection(t *testing.T) {
	t.Setenv("GH_TOKEN", "fake-token-value-123")
	t.Setenv("CLAUDE_CONFIG_DIR", "/elsewhere/claude")
	ctx := context.Background()

	t.Run("one login", func(t *testing.T) {
		f := newProfileGH(t)
		f.login(t, "default", "default-me", "acme")
		fc := &fakeClaude{answers: map[string]string{"": claudeIn}}
		s := loginsServer(t, `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"]},
 {"name":"personal","kind":"private"}]}`, fc)
		ls, err := s.spaceLogins(ctx)
		if err != nil || len(ls) != 2 {
			t.Fatalf("spaceLogins = %+v, %v", ls, err)
		}
		for _, l := range ls {
			want := loginState{State: loginLoggedIn, Account: "default-me", Storage: "keyring"}
			if l.GH != want {
				t.Errorf("%s gh = %+v, want %+v", l.Space, l.GH, want)
			}
			if l.Claude != (loginState{State: loginLoggedIn, Account: "claude.ai"}) {
				t.Errorf("%s claude = %+v", l.Space, l.Claude)
			}
		}
		if got := fmt.Sprintf("%+v", ls); strings.Contains(got, "someone@example.com") || strings.Contains(got, "Example Org") {
			t.Errorf("kept account details: %s", got)
		}
		// The user's own configuration: no CLAUDE_CONFIG_DIR, even one set
		// in orchd's environment.
		for _, env := range fc.envs {
			if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") }) {
				t.Errorf("checked the default Claude configuration with %v", env)
			}
		}
		if len(fc.envs) != 1 {
			t.Errorf("claude ran %d times for one configuration, want 1 (cached)", len(fc.envs))
		}
		for _, c := range f.calls(t) {
			if c.dir != "" {
				t.Errorf("%q ran with GH_CONFIG_DIR %q", c.args, c.dir)
			}
		}
	})

	t.Run("two logins", func(t *testing.T) {
		f := newProfileGH(t)
		f.login(t, "default", "default-me")
		f.login(t, "acme", "work-acme", "acme")
		writeFake(t, f, "acme.storage", "/profiles/acme/hosts.yml")
		fc := &fakeClaude{answers: map[string]string{
			"":                   claudeIn,
			"/claude/acme":       `{"loggedIn":false}`,
			"/claude/initech":    `not json`,
			"/claude/broken-run": "",
		}, failures: map[string]error{"/claude/broken-run": errors.New("exec: claude not found")}}
		s := loginsServer(t, `{"spaces":[
 {"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"/profiles/acme","claude_config_dir":"/claude/acme"},
 {"name":"globex","kind":"isolated","github_orgs":["globex"],"claude_config_dir":"/claude/broken-run"},
 {"name":"initech","kind":"isolated","github_orgs":["initech"],"gh_config_dir":"/profiles/initech","claude_config_dir":"/claude/initech"},
 {"name":"personal","kind":"private"}]}`, fc)
		ls, err := s.spaceLogins(ctx)
		if err != nil {
			t.Fatal(err)
		}
		acme := loginOf(t, ls, "acme")
		if acme.GH != (loginState{Own: true, Dir: acmeProfile, State: loginLoggedIn, Account: "work-acme", Storage: "file"}) {
			t.Errorf("acme gh = %+v", acme.GH)
		}
		if acme.Claude.State != loginLoggedOut || !acme.Claude.Own || acme.Claude.Dir != "/claude/acme" || !strings.Contains(acme.Claude.Detail, "/login") {
			t.Errorf("acme claude = %+v", acme.Claude)
		}
		globex := loginOf(t, ls, "globex")
		if globex.GH.Own || globex.GH.Account != "default-me" || globex.GH.State != loginLoggedIn {
			t.Errorf("globex gh = %+v", globex.GH)
		}
		if globex.Claude.State != loginUnknown || !strings.Contains(globex.Claude.Detail, "not found") {
			t.Errorf("globex claude = %+v", globex.Claude)
		}
		initech := loginOf(t, ls, "initech")
		if initech.GH.State != loginLoggedOut || !strings.Contains(initech.GH.Detail, "GH_CONFIG_DIR='/profiles/initech' gh auth login") {
			t.Errorf("initech gh = %+v", initech.GH)
		}
		if initech.Claude.State != loginUnknown {
			t.Errorf("initech claude = %+v", initech.Claude)
		}
		if p := loginOf(t, ls, "personal"); p.GH.Own || p.Claude.Own || p.Claude.State != loginLoggedIn {
			t.Errorf("personal = %+v", p)
		}
		for _, c := range f.calls(t) {
			if strings.Contains(c.args, "token") || slices.Contains(c.names, "GH_TOKEN") && c.dir != "" {
				t.Errorf("gh ran %q (dir %q, names %v): a token was asked for or passed to a profile", c.args, c.dir, c.names)
			}
		}

		// A refused profile directory is reported as such, and gh never
		// runs with it.
		s.ghProfiles.checkDir = func(dir string) error {
			if dir == acmeProfile {
				return errors.New("not usable as a gh profile: other users can open it")
			}
			return nil
		}
		before := len(f.calls(t))
		if st := s.ghLogin(ctx, acmeProfile); st.State != loginRefused || !strings.Contains(st.Detail, "other users") {
			t.Errorf("refused profile = %+v", st)
		}
		if len(f.calls(t)) != before {
			t.Error("gh ran with a refused profile")
		}
	})

	t.Run("no spaces or services", func(t *testing.T) {
		if ls, err := (&server{}).spaceLogins(ctx); ls != nil || err != nil {
			t.Errorf("without spaces = %v, %v", ls, err)
		}
		s := &server{}
		if st := s.ghLogin(ctx, ""); st.State != loginUnknown {
			t.Errorf("without gh = %+v", st)
		}
		if st := s.claudeLogin(ctx, ""); st.State != loginUnknown {
			t.Errorf("without claude checks = %+v", st)
		}
	})
}

// TestClaudeLoginCache: a logged-in answer is kept for the check TTL, a
// failure only for the retry TTL, and a caller's own cancellation is never
// cached.
func TestClaudeLoginCache(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	fc := &fakeClaude{answers: map[string]string{"": claudeIn, "/c": `{"loggedIn":false}`}}
	c := newClaudeLogins()
	c.run, c.now = fc.run, func() time.Time { return now }
	ctx := context.Background()
	c.check(ctx, "")
	c.check(ctx, "/c")
	now = now.Add(2 * time.Minute)
	c.check(ctx, "")
	c.check(ctx, "/c")
	if len(fc.envs) != 3 {
		t.Errorf("claude ran %d times, want 3 (the failure rechecked after a minute)", len(fc.envs))
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	fc.failures = map[string]error{"/d": context.Canceled}
	if got := c.check(cancelled, "/d"); got.state != loginUnknown {
		t.Errorf("failed check = %+v", got)
	}
	if _, cached := c.checked["/d"]; cached {
		t.Error("a cancelled caller's failure was cached")
	}
	t.Setenv("ANTHROPIC_API_KEY", "fake-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-oauth")
	env := claudeEnv("/x")
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/x") || slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=")
	}) {
		t.Errorf("claudeEnv = %v", env)
	}
}

// TestRunClaudeAuth runs the claude found on PATH as 'claude auth status
// --json' with the environment given, and nothing else.
func TestRunClaudeAuth(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"{\\\"args\\\":\\\"$*\\\",\\\"dir\\\":\\\"$CLAUDE_CONFIG_DIR\\\"}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := runClaudeAuth(context.Background(), []string{"CLAUDE_CONFIG_DIR=/c/acme"})
	if err != nil || strings.TrimSpace(string(out)) != `{"args":"auth status --json","dir":"/c/acme"}` {
		t.Errorf("runClaudeAuth = %q, %v", out, err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := runClaudeAuth(context.Background(), nil); err == nil && agentrunFindsNone() {
		t.Error("ran without a claude command")
	}
}

// agentrunFindsNone reports whether no claude is installed where FindClaude
// also looks (/opt/homebrew/bin, /usr/local/bin), so the not-found case can
// be checked on this machine.
func agentrunFindsNone() bool {
	for _, p := range []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude"} {
		if _, err := os.Stat(p); err == nil {
			return false
		}
	}
	return true
}

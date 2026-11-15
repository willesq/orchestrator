package status

import (
	"context"
	"errors"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func loggedIn(account, storage string) *orchdv1.LoginState {
	return &orchdv1.LoginState{State: "logged in", Account: account, Storage: storage}
}

// TestSpaceLoginLines is SP-074.3.a3 for 'orch status': the spaces section
// shows each space's gh login and Claude Code configuration, default or its
// own, and their state, in one-login and two-login layouts; nothing shows
// when orchd refuses (an agent session) or no policy is applied.
func TestSpaceLoginLines(t *testing.T) {
	configured := func(t *testing.T, logins []*orchdv1.SpaceLogin, err error) Section {
		t.Helper()
		d := healthyDeps(t)
		d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
			return &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{Configured: true, AppliedRevision: 3}}, nil
		}
		d.SpaceLogins = func(context.Context) ([]*orchdv1.SpaceLogin, error) { return logins, err }
		return section(t, Collect(context.Background(), d), "spaces")
	}

	t.Run("one login", func(t *testing.T) {
		sec := configured(t, []*orchdv1.SpaceLogin{
			{Space: "acme", Gh: loggedIn("willesq", "keyring"), Claude: loggedIn("claude.ai", "")},
			{Space: "personal", Gh: loggedIn("willesq", "keyring"), Claude: loggedIn("claude.ai", "")},
		}, nil)
		for _, name := range []string{"login acme", "login personal"} {
			l := line(t, sec, name)
			want := "gh: default login, logged in (willesq, token in keyring); claude: default config, logged in (claude.ai)"
			if l.Value != want || l.Level != LevelOK || l.Detail != "" {
				t.Errorf("%s = %+v, want %q", name, l, want)
			}
		}
	})

	t.Run("two logins", func(t *testing.T) {
		sec := configured(t, []*orchdv1.SpaceLogin{
			{Space: "acme",
				Gh:     &orchdv1.LoginState{Own: true, Dir: "/Users/u/.config/gh-acme", State: "logged in", Account: "work-acme", Storage: "file"},
				Claude: &orchdv1.LoginState{Own: true, Dir: "/Users/u/.claude-acme", State: "not logged in", Detail: "log in with: orch run <space> -- claude, then /login"}},
			{Space: "globex", Gh: &orchdv1.LoginState{Own: true, Dir: "/p/globex", State: "refused", Detail: "other users can open it\x1b[31m"},
				Claude: &orchdv1.LoginState{State: "unknown", Detail: "the claude command was not found"}},
			{Space: "oss", Gh: loggedIn("willesq", "keyring"), Claude: loggedIn("claude.ai", "")},
		}, nil)
		acme := line(t, sec, "login acme")
		if !strings.Contains(acme.Value, "gh: own profile, logged in (work-acme, token in file)") ||
			!strings.Contains(acme.Value, "claude: own config, not logged in") || acme.Level != LevelWarn ||
			!strings.Contains(acme.Detail, "/Users/u/.config/gh-acme") || !strings.Contains(acme.Detail, "/login") {
			t.Errorf("acme = %+v", acme)
		}
		globex := line(t, sec, "login globex")
		if !strings.Contains(globex.Value, "gh: own profile, refused") || globex.Level != LevelWarn || strings.Contains(globex.Detail, "\x1b") {
			t.Errorf("globex = %+v", globex)
		}
		if oss := line(t, sec, "login oss"); oss.Level != LevelOK {
			t.Errorf("oss = %+v", oss)
		}
	})

	t.Run("refused or not configured", func(t *testing.T) {
		sec := configured(t, nil, errors.New("PermissionDenied: operator only"))
		for _, l := range sec.Lines {
			if strings.HasPrefix(l.Name, "login ") {
				t.Errorf("a refused caller got %+v", l)
			}
		}
		d := healthyDeps(t)
		d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
			return &orchdv1.StatusResponse{Spaces: &orchdv1.SpacesStatus{AppliedRevision: 1}}, nil
		}
		called := false
		d.SpaceLogins = func(context.Context) ([]*orchdv1.SpaceLogin, error) { called = true; return nil, nil }
		Collect(context.Background(), d)
		if called {
			t.Error("asked for logins with no spaces configured")
		}
		if loginLines(context.Background(), nil) != nil {
			t.Error("nil fetch produced lines")
		}
		if worse(LevelBad, LevelOK) != LevelBad {
			t.Error("worse")
		}
	})
}

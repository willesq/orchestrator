package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGHBinary puts a gh on PATH that appends one line per run to the
// returned log: the GH_CONFIG_DIR it saw, its arguments and the names (never
// the values) of its environment variables. It then prints out.
func fakeGHBinary(t *testing.T, out string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"{ printf 'dir=%s|args=%s|names=' \"$GH_CONFIG_DIR\" \"$*\"; env | cut -d= -f1 | tr '\\n' ' '; echo; } >> '" + logPath + "'\n" +
		"printf '%s' '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

type ghCall struct {
	dir, args string
	names     []string
}

func readCalls(t *testing.T, logPath string) []ghCall {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var calls []ghCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("bad log line %q", line)
		}
		calls = append(calls, ghCall{
			dir:   strings.TrimPrefix(parts[0], "dir="),
			args:  strings.TrimPrefix(parts[1], "args="),
			names: strings.Fields(strings.TrimPrefix(parts[2], "names=")),
		})
	}
	return calls
}

func TestProfileGHRunsWithProfileDirAndNoTokenOverrides(t *testing.T) {
	const secret = "fake-token-value-123"
	logPath := fakeGHBinary(t, "ok")
	for _, name := range profileOverrides {
		t.Setenv(name, secret)
	}
	out, err := ProfileGH("/profiles/acme")(context.Background(), "api", "user")
	if err != nil || string(out) != "ok" {
		t.Fatalf("ProfileGH = %q, %v", out, err)
	}
	calls := readCalls(t, logPath)
	if len(calls) != 1 || calls[0].dir != "/profiles/acme" || calls[0].args != "api user" {
		t.Fatalf("calls = %+v", calls)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST"} {
		if slices.Contains(calls[0].names, name) {
			t.Errorf("profile run kept %s", name)
		}
	}
	if !slices.Contains(calls[0].names, "GH_CONFIG_DIR") || !slices.Contains(calls[0].names, "PATH") {
		t.Errorf("profile run names = %v", calls[0].names)
	}
	if b, _ := os.ReadFile(logPath); strings.Contains(string(b), secret) {
		t.Error("the log recorded a credential value")
	}
}

func TestGHKeepsTheDefaultEnvironment(t *testing.T) {
	logPath := fakeGHBinary(t, "ok")
	t.Setenv("GH_CONFIG_DIR", "/default/profile")
	if _, err := GH(context.Background(), "api", "user"); err != nil {
		t.Fatal(err)
	}
	if calls := readCalls(t, logPath); len(calls) != 1 || calls[0].dir != "/default/profile" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestProfileEnvSetsOnlyOneConfigDir(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", "/elsewhere")
	env := ProfileEnv("/profiles/acme")
	var dirs []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GH_CONFIG_DIR="); ok {
			dirs = append(dirs, v)
		}
	}
	if !slices.Equal(dirs, []string{"/profiles/acme"}) {
		t.Fatalf("GH_CONFIG_DIR values = %v", dirs)
	}
}

func TestAuthLogin(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		runErr    error
		want      string
		wantErr   error
	}{
		{name: "logged in", out: `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"work-me"}]}}`, want: "work-me"},
		{name: "no login", out: `{"hosts":{}}`, wantErr: ErrNotLoggedIn},
		{name: "inactive only", out: `{"hosts":{"github.com":[{"state":"success","active":false,"login":"x"}]}}`, wantErr: ErrNotLoggedIn},
		{name: "broken login", out: `{"hosts":{"github.com":[{"state":"error","active":true,"login":"work-me"}]}}`, wantErr: ErrNotLoggedIn},
		{name: "bad login", out: `{"hosts":{"github.com":[{"state":"success","active":true,"login":"-bad-"}]}}`, wantErr: ErrNotLoggedIn},
		{name: "not json", out: `You are not logged in`},
		{name: "gh fails", runErr: errors.New("exit 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				got = args
				return []byte(tc.out), tc.runErr
			}
			login, err := AuthLogin(context.Background(), gh)
			if strings.Join(got, " ") != "auth status --active --hostname github.com --json hosts" {
				t.Errorf("args = %v", got)
			}
			switch {
			case tc.want != "":
				if err != nil || login != tc.want {
					t.Errorf("AuthLogin = %q, %v; want %q", login, err, tc.want)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			case err == nil:
				t.Errorf("AuthLogin = %q, want an error", login)
			}
		})
	}
}

func TestOrgs(t *testing.T) {
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "api --paginate user/orgs --jq .[].login" {
			t.Errorf("args = %v", args)
		}
		return []byte("acme\n\nnot valid!\nglobex\n"), nil
	}
	orgs, err := Orgs(context.Background(), gh)
	if err != nil || !slices.Equal(orgs, []string{"acme", "globex"}) {
		t.Fatalf("Orgs = %v, %v", orgs, err)
	}
	fail := func(context.Context, ...string) ([]byte, error) { return nil, errors.New("HTTP 401") }
	if _, err := Orgs(context.Background(), fail); err == nil {
		t.Error("Orgs ignored a gh failure")
	}
}

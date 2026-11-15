package slack

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/feeds"
)

// childRunner is the real connector process: this test binary, started as
// orchd would start itself, against the fake API.
func childRunner(t *testing.T, api *fakeSlack) Runner {
	t.Helper()
	t.Setenv("ORCH_SLACK_TEST_CONNECTOR", "1")
	t.Setenv("ORCH_SLACK_TEST_API", api.url())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Process(exe)
}

// captureLog sends the standard logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// TestSlackTokenNeverLeaks is SP-061.1.a2: the token is read only in the
// connector process (a separate process here, as in orchd), and no log,
// error, stored content or state holds its value: not when a chat message
// quotes it, not when Slack refuses it, and not when the profile is logged
// in to another workspace. The count of token occurrences must be 0.
func TestSlackTokenNeverLeaks(t *testing.T) {
	logs := captureLog(t)
	e := newSyncEnv(t)
	e.conn.Run = childRunner(t, e.api)
	// Someone pasted the token into a channel; the store must not get it.
	e.api.post("CPRIV", 70, "UBOB", "my token is "+testToken+" oops", "")
	e.sync(t)
	var errs []string

	// Slack refuses the token.
	badProfile := filepath.Join(filepath.Dir(e.conn.Profile), "slack-bad")
	if err := os.Mkdir(badProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	other := "xoxb-1111111111-another-fake-token"
	if err := os.WriteFile(filepath.Join(badProfile, TokenFile), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := *e.conn
	bad.Profile = badProfile
	_, err := feeds.Sync(context.Background(), &bad, e.conn.Allow, e.ing)
	var apiErr *APIError
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") || errors.As(err, &apiErr) {
		t.Errorf("refused token: %v", err)
	}
	errs = append(errs, err.Error())

	// The profile is logged in to another workspace.
	e.api.team = "T9"
	_, err = feeds.Sync(context.Background(), e.conn, e.conn.Allow, e.ing)
	if err == nil || !strings.Contains(err.Error(), "another workspace") {
		t.Errorf("wrong workspace: %v", err)
	}
	errs = append(errs, err.Error())
	e.api.team = "T1"

	// The status check reports metadata only.
	st, err := Check(context.Background(), e.conn.Run, e.conn.Profile)
	if err != nil || st.TeamID != "T1" || st.UserID != "UME" {
		t.Errorf("status = %+v, %v", st, err)
	}

	state, err := os.ReadFile(e.state.Path)
	if err != nil {
		t.Fatal(err)
	}
	stored := strings.Join(e.ing.texts, "\n")
	if !strings.Contains(stored, "my token is") {
		t.Fatalf("the message quoting the token was not stored:\n%s", stored)
	}
	for _, secret := range []string{testToken, other, strings.TrimPrefix(testToken, "xoxp-"), strings.TrimPrefix(other, "xoxb-")} {
		for name, text := range map[string]string{
			"log": logs.String(), "errors": strings.Join(errs, "\n"), "stored content": stored,
			"state": string(state), "status": st.Team + st.TeamID + st.UserID,
		} {
			if n := strings.Count(text, secret); n != 0 {
				t.Errorf("%s holds the token %d times", name, n)
			}
		}
	}
}

// TestReadTokenRefusals: a profile or token file anyone else could read or
// replace is refused, and no refusal repeats the file's content.
func TestReadTokenRefusals(t *testing.T) {
	profile := profileDir(t)
	tokenPath := filepath.Join(profile, TokenFile)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"open token file", func(t *testing.T) { chmod(t, tokenPath, 0o644) }, "chmod 600"},
		{"not a token", func(t *testing.T) { write(t, tokenPath, "hunter2-not-a-token") }, "does not hold a Slack token"},
		{"too large", func(t *testing.T) { write(t, tokenPath, strings.Repeat("x", 5000)) }, "is not a token"},
		{"missing", func(t *testing.T) { _ = os.Remove(tokenPath) }, "has no token file"},
		{"symlink", func(t *testing.T) {
			target := filepath.Join(profile, "elsewhere")
			write(t, target, testToken)
			_ = os.Remove(tokenPath)
			if err := os.Symlink(target, tokenPath); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"open profile", func(t *testing.T) { chmod(t, profile, 0o755) }, "chmod 700"},
		{"relative profile", nil, "not an absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, tokenPath, testToken)
			chmod(t, profile, 0o700)
			dir := profile
			if tc.setup != nil {
				tc.setup(t)
			} else {
				dir = "slack-acme"
			}
			_, err := readToken(dir)
			if !errors.Is(err, ErrProfile) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readToken = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "gh profile") {
				t.Errorf("the refusal repeats content or names gh: %v", err)
			}
		})
	}
	write(t, tokenPath, "  "+testToken+"\n")
	chmod(t, profile, 0o700)
	if tok, err := readToken(profile); err != nil || tok != testToken {
		t.Errorf("readToken = %v", err)
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestServeScrubsToken: the connector process itself takes the token out
// of record text, before anything leaves it (feeds.Sync's redaction is a
// second layer).
func TestServeScrubsToken(t *testing.T) {
	e := newSyncEnv(t)
	e.api.post("CPUB", 46, "UBOB", "token: "+testToken, "")
	req := Request{Op: OpFetch, Profile: e.conn.Profile, Tenant: "T1", Containers: []string{"CPUB"}}
	n := 0
	err := testFetcher(e.api).Serve(context.Background(), req, func(l Line) error {
		if l.Record != nil {
			n++
			if strings.Contains(l.Record.Text, testToken) || !strings.Contains(l.Record.Text, "token: [REDACTED]") {
				t.Errorf("record text:\n%s", l.Record.Text)
			}
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("Serve = %d records, %v", n, err)
	}
}

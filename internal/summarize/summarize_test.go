package summarize

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/willesq/orchestrator/internal/session"
)

// fakeCLI records its argv and stdin into dir and prints reply, exiting code.
func fakeCLI(t *testing.T, reply string, code int, sleep string) (bin, dir string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir = t.TempDir()
	bin = filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + dir + "/args\ncat > " + dir + "/stdin\n"
	if sleep != "" {
		script += "sleep " + sleep + "\n"
	}
	script += "printf '%s' '" + reply + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

var msgs = []session.Message{
	{Role: "user", Text: "add retries to the uploader"},
	{Role: "assistant", Text: "Added exponential backoff in uploader.go."},
}

func TestSummarize(t *testing.T) {
	bin, dir := fakeCLI(t, `{"result":"## Outcome\nRetries added.","is_error":false}`, 0, "")
	s := Summarizer{Bin: bin, Model: "haiku", Dir: t.TempDir(), Env: os.Environ()}
	got, err := s.Summarize(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "## Outcome\nRetries added." {
		t.Errorf("summary = %q", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"-p", "--safe-mode", "--no-session-persistence", "--tools", "--system-prompt", "--model\nhaiku", "--output-format\njson"} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("argv missing %q:\n%s", want, args)
		}
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.Contains(string(stdin), "[user] add retries to the uploader") {
		t.Errorf("transcript not on stdin:\n%s", stdin)
	}
}

func TestSummarizeFailures(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	tests := []struct {
		name    string
		reply   string
		code    int
		msgs    []session.Message
		wantErr string
		want    string
	}{
		{name: "model error", reply: `{"result":"overloaded","is_error":true}`, msgs: msgs, wantErr: "summarizer failed: overloaded"},
		{name: "crash", reply: ``, code: 2, msgs: msgs, wantErr: "summarizer: exit status 2"},
		{name: "unreadable", reply: `nope`, msgs: msgs, wantErr: "unreadable output"},
		{name: "empty transcript", msgs: []session.Message{{Role: "user", Text: "  "}}, wantErr: "empty transcript"},
		{name: "summary is redacted", reply: `{"result":"token ` + secret + `","is_error":false}`, msgs: msgs, want: "token [REDACTED:github-token]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, _ := fakeCLI(t, tt.reply, tt.code, "")
			got, err := Summarizer{Bin: bin, Model: "haiku", Dir: t.TempDir(), Env: os.Environ()}.Summarize(context.Background(), tt.msgs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestSummarizeTimesOut(t *testing.T) {
	bin, _ := fakeCLI(t, `{"result":"late"}`, 0, "30")
	start := time.Now()
	_, err := Summarizer{Bin: bin, Model: "haiku", Dir: t.TempDir(), Env: os.Environ(), Timeout: 200 * time.Millisecond}.Summarize(context.Background(), msgs)
	// sleep 30 can never finish within 200ms; an unkilled one would take 30s.
	if err == nil || time.Since(start) > 25*time.Second {
		t.Errorf("err = %v after %s", err, time.Since(start))
	}
}

func TestCondense(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	short := Condense([]session.Message{{Role: "user", Text: "use " + secret}, {Role: "assistant", Text: ""}})
	if short != "[user] use [REDACTED:github-token]" {
		t.Errorf("short = %q", short)
	}
	long := []session.Message{
		{Role: "user", Text: "GOAL " + strings.Repeat("é", headChars)},
		{Role: "assistant", Text: strings.Repeat("middle ", tailChars)},
		{Role: "assistant", Text: "OUTCOME done"},
	}
	got := Condense(long)
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "[user] GOAL") || !strings.HasSuffix(got, "OUTCOME done") ||
		!strings.Contains(got, "middle of the session omitted") || len(got) > headChars+tailChars+100 {
		t.Errorf("long transcript condensed badly: %d bytes", len(got))
	}
}

func TestEnvAndTempDir(t *testing.T) {
	env := Env([]string{"HOME=/h", "QDRANT_API_KEY=k", "PATH=/p"}, "QDRANT_API_KEY")
	if strings.Join(env, ",") != "HOME=/h,PATH=/p" {
		t.Errorf("Env = %v", env)
	}
	dir, cleanup, err := TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("temp dir: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir survived cleanup")
	}
}

package agentrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRisk(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"git status", ""},
		{"git push origin HEAD:add-cloudflare", "git push"},
		{"cd /tmp/x && git status -s && git push origin HEAD", "git push"},
		{"git -C /tmp/x push", "git push"},
		{"GIT_TRACE=1 git push", "git push"},
		{"git push --force-with-lease", "force push"},
		{"git push -f origin x", "force push"},
		{"git push origin +x", "force push"},
		{`git commit -m "ready for git push later"`, ""},
		{"echo git push", ""},
		{"git reset --hard origin/main", "hard reset"},
		{"git reset HEAD~1", ""},
		{"gh pr merge 3 --squash", "merges a PR"},
		{"gh pr close 3", "closes a PR"},
		{"gh pr view 3", ""},
		{"gh pr comment 3 -b hi", "posts to GitHub"},
		{"gh api repos/o/r/pulls", ""},
		{"gh api -X DELETE repos/o/r", "calls the GitHub API to write"},
		{"rm -rf build", "deletes files"},
		{"rm -fr build", "deletes files"},
		{"rm -r -f build", "deletes files"},
		{"rm file.txt", ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			if got := Risk(tc.command); got != tc.want {
				t.Errorf("Risk(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

func writeTranscript(t *testing.T, home, taskID string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-tmp-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, SessionID(taskID)+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func toolLine(t *testing.T, at, name string, input map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": at,
		"message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "thinking"},
			{"type": "tool_use", "name": name, "input": input},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadActivityDescribesEachKindOfTool(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "t1",
		`{"type":"user","message":{"content":"quoted \"tool_use\" text"}}`,
		`not json, "tool_use", a line still being written`,
		toolLine(t, "2026-11-01T00:00:01Z", "Read", map[string]string{"file_path": "/a/b.go"}),
		toolLine(t, "2026-11-01T00:00:02Z", "Grep", map[string]string{"pattern": "needle"}),
		toolLine(t, "2026-11-01T00:00:03Z", "Edit", map[string]string{"file_path": "/a/c.go"}),
		toolLine(t, "2026-11-01T00:00:04Z", "Bash", map[string]string{"command": "git push\n  origin x", "description": "harmless, honest"}),
		toolLine(t, "2026-11-01T00:00:05Z", "Task", map[string]string{"subagent_type": "reviewer", "description": "check it"}),
		toolLine(t, "2026-11-01T00:00:06Z", "WebFetch", map[string]string{"url": "https://x"}),
	)
	got, err := ReadActivity(home, "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Total != 6 {
		t.Fatalf("got %+v", got)
	}
	want := []ActivityStep{
		{At: "2026-11-01T00:00:01Z", Kind: "read", Text: "/a/b.go"},
		{At: "2026-11-01T00:00:02Z", Kind: "read", Text: "needle"},
		{At: "2026-11-01T00:00:03Z", Kind: "edit", Text: "/a/c.go"},
		// The command itself, never the agent's description of it.
		{At: "2026-11-01T00:00:04Z", Kind: "run", Text: "git push origin x", Risk: "git push"},
		{At: "2026-11-01T00:00:05Z", Kind: "agent", Text: "reviewer: check it"},
		{At: "2026-11-01T00:00:06Z", Kind: "other", Text: "WebFetch"},
	}
	for i, step := range want {
		if got.Steps[i] != step {
			t.Errorf("step %d = %+v, want %+v", i, got.Steps[i], step)
		}
	}
}

func TestReadActivityRedactsAndCapsCommands(t *testing.T) {
	home := t.TempDir()
	secret := "ghp_" + strings.Repeat("a", 36)
	writeTranscript(t, home, "t1", toolLine(t, "", "Bash", map[string]string{
		"command": "curl -H 'Authorization: " + secret + "' " + strings.Repeat("x", 1000) + "\x1b[31m",
	}))
	got, err := ReadActivity(home, "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	text := got.Steps[0].Text
	if strings.Contains(text, secret) {
		t.Errorf("the token survived: %q", text)
	}
	if len(text) > activityTextLimit+len("…") || strings.ContainsRune(text, '\x1b') {
		t.Errorf("text not cleaned: %d bytes %q", len(text), text)
	}
}

func TestReadActivityKeepsRiskyStepsPastTheLimit(t *testing.T) {
	home := t.TempDir()
	lines := []string{toolLine(t, "", "Bash", map[string]string{"command": "git push"})}
	for i := range 10 {
		lines = append(lines, toolLine(t, "", "Read", map[string]string{"file_path": fmt.Sprintf("/f%d", i)}))
	}
	writeTranscript(t, home, "t1", lines...)
	got, err := ReadActivity(home, "t1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 11 || len(got.Steps) != 4 || got.Steps[0].Risk != "git push" || got.Steps[3].Text != "/f9" {
		t.Errorf("got %+v", got)
	}
}

func TestReadActivityWithoutATranscript(t *testing.T) {
	for _, home := range []string{"", t.TempDir()} {
		got, err := ReadActivity(home, "absent", 10)
		if err != nil || got.Available || len(got.Steps) != 0 {
			t.Errorf("home %q: got %+v, %v", home, got, err)
		}
	}
}

func TestReadActivityReadsOnlyTheTailOfALongSession(t *testing.T) {
	home := t.TempDir()
	path := writeTranscript(t, home, "t1", toolLine(t, "", "Read", map[string]string{"file_path": "/too-old"}))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", activityTail) + "\n" + toolLine(t, "", "Read", map[string]string{"file_path": "/recent"}) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadActivity(home, "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Text != "/recent" {
		t.Errorf("got %+v", got)
	}
}

func TestReadActivityIgnoresAFIFO(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, SessionID("t1")+".jsonl"), 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	got, err := ReadActivity(home, "t1", 10)
	if err != nil || got.Available {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestTrimStepsCapsEarlierRiskySteps(t *testing.T) {
	var steps []ActivityStep
	for range 10 {
		steps = append(steps, ActivityStep{Kind: "run", Risk: "git push"})
	}
	if got := trimSteps(steps, 2); len(got) != 4 {
		t.Errorf("kept %d", len(got))
	}
}

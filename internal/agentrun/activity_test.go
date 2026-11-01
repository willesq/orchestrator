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
	want := []struct{ at, kind, text, risk string }{
		{"2026-11-01T00:00:01Z", "read", "/a/b.go", ""},
		{"2026-11-01T00:00:02Z", "read", "needle", ""},
		{"2026-11-01T00:00:03Z", "edit", "/a/c.go", ""},
		// The command itself, never the agent's description of it.
		{"2026-11-01T00:00:04Z", "run", "git push origin x", "git push"},
		{"2026-11-01T00:00:05Z", "agent", "reviewer: check it", ""},
		{"2026-11-01T00:00:06Z", "other", "WebFetch", ""},
	}
	for i, w := range want {
		step := got.Steps[i]
		if step.At != w.at || step.Kind != w.kind || step.Text != w.text || step.Risk != w.risk || step.Seq != i {
			t.Errorf("step %d = %+v, want %+v", i, step, w)
		}
	}
}

func resultLine(t *testing.T, at, id string, isError bool, content any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "user", "timestamp": at,
		"message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": id, "is_error": isError, "content": content},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func usedTool(t *testing.T, at, id, name string, input any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": at,
		"message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": id, "name": name, "input": input},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadActivityMatchesResultsAndSummarizes(t *testing.T) {
	home := t.TempDir()
	secret := "ghp_" + strings.Repeat("b", 36)
	writeTranscript(t, home, "t1",
		usedTool(t, "2026-11-01T00:00:00.000Z", "a", "Bash", map[string]string{"command": "cd /x && go test ./...\ngit status"}),
		resultLine(t, "2026-11-01T00:00:02.500Z", "a", true, "FAIL pkg\n\x1b[31mtoken "+secret+"\n"),
		usedTool(t, "2026-11-01T00:00:03Z", "b", "Edit", map[string]string{"file_path": "/x/a.go", "old_string": "one\ntwo", "new_string": "three\n"}),
		resultLine(t, "2026-11-01T00:00:04Z", "b", false, []map[string]string{{"type": "text", "text": "ok"}, {"type": "image", "text": "no"}}),
		usedTool(t, "2026-11-01T00:00:05Z", "c", "MultiEdit", map[string]any{"file_path": "/x/a.go", "edits": []map[string]string{{"old_string": "a", "new_string": "b\nc"}}}),
		usedTool(t, "2026-11-01T00:00:06Z", "d", "Write", map[string]string{"file_path": "/x/b.go", "content": "x\ny\n"}),
		usedTool(t, "2026-11-01T00:00:07Z", "e", "Read", map[string]string{"file_path": "/x/a.go"}),
		usedTool(t, "2026-11-01T00:00:08Z", "f", "Read", map[string]string{"file_path": "/x/a.go"}),
		// A result for nothing this tail has seen is ignored.
		resultLine(t, "2026-11-01T00:00:09Z", "older", false, "x"),
	)
	got, err := ReadActivity(home, "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	first := got.Steps[0]
	if !first.Finished || !first.Failed || first.DurationMS != 2500 {
		t.Errorf("first: %+v", first)
	}
	if first.Title != "go test ./..." || len(first.Commands) != 3 {
		t.Errorf("commands: %q, title %q", first.Commands, first.Title)
	}
	if strings.Contains(first.Output, secret) || strings.ContainsRune(first.Output, '\x1b') || !strings.Contains(first.Output, "FAIL pkg\n") {
		t.Errorf("output: %q", first.Output)
	}
	if !strings.Contains(first.Detail, "\n") {
		t.Errorf("detail lost its lines: %q", first.Detail)
	}
	if got.Steps[1].Output != "ok" || got.Steps[1].Additions != 1 || got.Steps[1].Deletions != 2 {
		t.Errorf("edit: %+v", got.Steps[1])
	}
	if got.Steps[2].Finished {
		t.Error("a step with no result counted as finished")
	}
	if got.Commands != 3 || got.Failed != 1 || got.FilesRead != 1 {
		t.Errorf("counts: %+v", got)
	}
	if len(got.Files) != 2 || got.Files[0] != (FileEdit{"/x/a.go", 3, 3}) || got.Files[1] != (FileEdit{"/x/b.go", 2, 0}) {
		t.Errorf("files: %+v", got.Files)
	}
	if got.Tools[0] != (ToolCount{"Read", 2}) || len(got.Tools) != 5 {
		t.Errorf("tools: %+v", got.Tools)
	}
	if got.StartedAt != "2026-11-01T00:00:00.000Z" || got.LastAt != "2026-11-01T00:00:08Z" {
		t.Errorf("times: %q %q", got.StartedAt, got.LastAt)
	}
}

func TestCommandCountsAreNotCapped(t *testing.T) {
	home := t.TempDir()
	command := strings.TrimSuffix(strings.Repeat("ls; ", activityCommandLimit+3), "; ")
	writeTranscript(t, home, "t1", usedTool(t, "", "a", "Bash", map[string]string{"command": command}))
	got, err := ReadActivity(home, "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	step := got.Steps[0]
	if len(step.Commands) != activityCommandLimit || step.CommandTotal != activityCommandLimit+3 || got.Commands != activityCommandLimit+3 {
		t.Errorf("listed %d, step total %d, run total %d", len(step.Commands), step.CommandTotal, got.Commands)
	}
}

// A transcript is written by an agent, so nothing in it may grow a poll or carry
// a secret past redaction.
func TestReadActivityWithAHostileTranscript(t *testing.T) {
	home := t.TempDir()
	secret := "ghp_" + strings.Repeat("c", 36)
	split := "ghp_" + strings.Repeat("d", 18) + "\x00" + strings.Repeat("d", 18)
	pem := "-----BEGIN RSA PRIVATE KEY-----\nsecretbody\n-----END RSA PRIVATE KEY-----"
	writeTranscript(t, home, "t1",
		usedTool(t, strings.Repeat("9", 3<<20), "a", secret+strings.Repeat("x", 500), map[string]string{}),
		usedTool(t, "2026-11-01T00:00:00Z", "b", "Bash", map[string]string{"command": "echo " + pem + "; echo " + split}),
		resultLine(t, "2026-11-01T00:00:01Z", "b", false, split),
	)
	got, err := ReadActivity(home, "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	first := got.Steps[0]
	if first.At != "" || len(first.Tool) > activityToolLimit+len("…") || strings.Contains(first.Tool, secret) {
		t.Errorf("first: at %d bytes, tool %q", len(first.At), first.Tool)
	}
	if got.Tools[0].Name == secret || len(got.Tools[0].Name) > activityToolLimit+len("…") {
		t.Errorf("tool count: %q", got.Tools[0].Name)
	}
	second := got.Steps[1]
	joined := strings.ReplaceAll(split, "\x00", "")
	for name, text := range map[string]string{
		"commands": strings.Join(second.Commands, " "), "detail": second.Detail,
		"text": second.Text, "title": second.Title, "output": second.Output,
	} {
		if strings.Contains(text, "secretbody") || strings.Contains(text, joined) {
			t.Errorf("%s carries a secret: %q", name, text)
		}
	}
}

func TestAFloodOfRiskyStepsCannotHideALaterPush(t *testing.T) {
	var steps []ActivityStep
	for range 20 {
		steps = append(steps, ActivityStep{Kind: "run", Risk: "hard reset"})
	}
	steps = append(steps, ActivityStep{Kind: "run", Risk: "git push", Text: "the push"})
	for range 5 {
		steps = append(steps, ActivityStep{Kind: "read"})
	}
	got := trimSteps(steps, 3)
	found := false
	for _, s := range got {
		found = found || s.Text == "the push"
	}
	if !found || len(got) != 6 {
		t.Errorf("found %v, kept %d", found, len(got))
	}
	sum := summarize(steps)
	if !sum.Pushed || sum.Risky != 21 {
		t.Errorf("summary: %+v", sum)
	}
}

func TestSplitCommands(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		main string
	}{
		{"git status", []string{"git status"}, "git status"},
		{"cd /x && git log | head -5; echo done", []string{"cd /x", "git log | head -5", "echo done"}, "git log | head -5"},
		{"a || b\nc", []string{"a", "b", "c"}, "a"},
		{`git commit -m "a; b && c"; ls`, []string{`git commit -m "a; b && c"`, "ls"}, `git commit -m "a; b && c"`},
		{`echo 'x; y'`, []string{`echo 'x; y'`}, `echo 'x; y'`},
		{`echo "q\"; r"; ls`, []string{`echo "q\"; r"`, "ls"}, "ls"},
		{"X=$(a; b); c", []string{"X=$(a; b)", "c"}, "X=$(a; b)"},
		{"(cd /x; ls) && d", []string{"(cd /x; ls)", "d"}, "(cd /x; ls)"},
		{"cat > f <<'E'\nline; one\nE\ngit add f", []string{"cat > f <<'E' …", "git add f"}, "cat > f <<'E' …"},
		{"cat <<E\nnever closed", []string{"cat <<E …"}, "cat <<E …"},
		{"cat <<< x; ls", []string{"cat <<< x", "ls"}, "cat <<< x"},
		{"go test \\\n  ./...", []string{"go test    ./..."}, "go test    ./..."},
		{"# a note\nls", []string{"ls"}, "ls"},
		{"W=/x; cd $W", []string{"W=/x", "cd $W"}, "W=/x"},
		{"a &", []string{"a &"}, "a &"},
		{"", nil, ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got := splitCommands(tc.in)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("split %q, want %q", got, tc.want)
			}
			if m := mainCommand(got); m != tc.main {
				t.Errorf("main %q, want %q", m, tc.main)
			}
		})
	}
}

func TestTrimStepsKeepsOutputOnlyWhereItMatters(t *testing.T) {
	steps := make([]ActivityStep, activityOutputSteps+2)
	for i := range steps {
		steps[i].Output = "out"
	}
	steps[1].Failed = true
	got := trimSteps(steps, 0)
	if got[0].Output != "" || got[1].Output != "out" || got[len(got)-1].Output != "out" {
		t.Errorf("first %q, failed %q, newest %q", got[0].Output, got[1].Output, got[len(got)-1].Output)
	}
}

func TestCleanMultilineCaps(t *testing.T) {
	if got := cleanMultiline(strings.Repeat("a", 100)+"END", 10, true); got != "…\naaaaaaaEND" {
		t.Errorf("end: %q", got)
	}
	if got := cleanMultiline("first line\nsecond\nEND", 12, true); got != "…\nsecond\nEND" {
		t.Errorf("line: %q", got)
	}
	if got := cleanMultiline("START"+strings.Repeat("a", 100), 7, false); got != "STARTaa" {
		t.Errorf("start: %q", got)
	}
	if got := cleanMultiline("a\tb\x07\nc  \n", 100, false); got != "a\tb\nc" {
		t.Errorf("lines: %q", got)
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

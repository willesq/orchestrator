package capture

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func readRecords(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line is not one JSON record: %v: %s", err, sc.Text())
		}
		recs = append(recs, r)
	}
	return recs
}

func TestCaptureAppendsOneLinePerEvent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ScenarioFile), []byte("cd outside project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{Dir: dir, Harness: "claude-code", Event: "PreToolUse", HookCwd: "/scratch/project", Now: now}
	in := `{"session_id":"s1","cwd":"/scratch/project","tool_name":"Bash","tool_input":{"command":"cd ../outside"}}`
	for range 2 {
		out, err := Capture(opts, []byte(in))
		if err != nil || out != "" {
			t.Fatalf("Capture = %q, %v; want no output", out, err)
		}
	}
	recs := readRecords(t, filepath.Join(dir, "claude-code.jsonl"))
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	r := recs[0]
	if r.Scenario != "cd outside project" || r.Event != "PreToolUse" || r.HookCwd != "/scratch/project" ||
		r.Harness != "claude-code" || r.At != "2026-11-08T12:00:00Z" {
		t.Errorf("record metadata = %+v", r)
	}
	var p struct {
		Cwd       string          `json:"cwd"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(r.Payload, &p); err != nil || p.Cwd != "/scratch/project" ||
		!strings.Contains(string(p.ToolInput), "cd ../outside") {
		t.Errorf("payload not preserved: %s (%v)", r.Payload, err)
	}
}

func TestCaptureRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	secret := "ghp_" + strings.Repeat("a", 36)
	in := `{"tool_name":"Bash","tool_input":{"command":"GH_TOKEN=` + secret + ` gh pr list"}}`
	if _, err := Capture(Options{Dir: dir, Harness: "codex", Event: "PreToolUse", Now: now}, []byte(in)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "codex.jsonl"))
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret written to capture log: %s", raw)
	}
	recs := readRecords(t, filepath.Join(dir, "codex.jsonl"))
	if recs[0].Redactions != 1 || !strings.Contains(string(recs[0].Payload), "[REDACTED:github-token]") {
		t.Errorf("record = %+v", recs[0])
	}
}

func TestCaptureTruncatesLargeToolResponse(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxToolResponse+10)
	in := `{"tool_name":"Read","tool_response":{"content":"` + big + `"},"cwd":"/p"}`
	if _, err := Capture(Options{Dir: dir, Harness: "claude-code", Event: "PostToolUse", Now: now}, []byte(in)); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, filepath.Join(dir, "claude-code.jsonl"))
	var p map[string]any
	if err := json.Unmarshal(recs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if s, _ := p["tool_response"].(string); !strings.HasPrefix(s, "[truncated ") || p["cwd"] != "/p" {
		t.Errorf("payload = %v", p)
	}
}

func TestCaptureKeepsNonObjectInput(t *testing.T) {
	dir := t.TempDir()
	for _, in := range []string{"not json", "", "[1,2]", "null"} {
		if _, err := Capture(Options{Dir: dir, Harness: "h", Event: "Stop", Now: now}, []byte(in)); err != nil {
			t.Fatalf("Capture(%q): %v", in, err)
		}
	}
	recs := readRecords(t, filepath.Join(dir, "h.jsonl"))
	if len(recs) != 4 || string(recs[0].Payload) != `"not json"` || recs[0].Scenario != "" {
		t.Errorf("records = %+v", recs)
	}
}

func TestCaptureDeniesMarkedPreToolUse(t *testing.T) {
	tests := []struct {
		name, event, input string
		wantDeny           bool
	}{
		{"marked PreToolUse", "PreToolUse", `{"tool_input":{"command":"echo ` + DenyMarker + `"}}`, true},
		{"unmarked PreToolUse", "PreToolUse", `{"tool_input":{"command":"echo hi"}}`, false},
		{"marked PostToolUse", "PostToolUse", `{"tool_input":{"command":"echo ` + DenyMarker + `"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Capture(Options{Dir: t.TempDir(), Harness: "claude-code", Event: tt.event, Now: now}, []byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if !tt.wantDeny {
				if out != "" {
					t.Errorf("out = %q, want empty", out)
				}
				return
			}
			var d struct {
				H struct {
					Event    string `json:"hookEventName"`
					Decision string `json:"permissionDecision"`
					Reason   string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &d); err != nil || d.H.Event != "PreToolUse" ||
				d.H.Decision != "deny" || d.H.Reason == "" {
				t.Errorf("deny output = %q (%v)", out, err)
			}
		})
	}
}

func TestCaptureReportsUnwritableDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Capture(Options{Dir: missing, Harness: "h", Event: "Stop", Now: now}, []byte("{}")); err == nil {
		t.Fatal("want error for a capture dir that does not exist")
	}
}

func TestCaptureRedactsTokensAfterJSONEscapes(t *testing.T) {
	tok := "ghp_" + strings.Repeat("b", 36)
	for _, prefix := range []string{`\n`, `\t`, "<", "&", ">"} {
		t.Run(prefix, func(t *testing.T) {
			dir := t.TempDir()
			in := `{"tool_input":{"content":"X=1` + prefix + tok + `"}}`
			if _, err := Capture(Options{Dir: dir, Harness: "claude-code", Event: "PreToolUse", Now: now}, []byte(in)); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "claude-code.jsonl"))
			if strings.Contains(string(raw), tok) {
				t.Fatalf("token after %q not redacted: %s", prefix, raw)
			}
		})
	}
}

func TestCapturePrivateKeyAcrossFieldsKeepsValidJSON(t *testing.T) {
	dir := t.TempDir()
	in := `{"tool_input":{"command":"cat <<K\n-----BEGIN RSA PRIVATE KEY-----\nabc"},` +
		`"tool_response":{"stdout":"def\n-----END RSA PRIVATE KEY-----"},"cwd":"/p"}`
	if _, err := Capture(Options{Dir: dir, Harness: "claude-code", Event: "PostToolUse", Now: now}, []byte(in)); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, filepath.Join(dir, "claude-code.jsonl"))
	var p map[string]any
	if len(recs) != 1 || json.Unmarshal(recs[0].Payload, &p) != nil || p["cwd"] != "/p" {
		t.Fatalf("event lost or payload broken: %+v", recs)
	}
	if _, ok := p["tool_input"].(map[string]any); !ok {
		t.Errorf("tool_input structure lost: %v", p)
	}
}

func TestCaptureRedactsScenarioAndHookCwd(t *testing.T) {
	dir := t.TempDir()
	tok := "ghp_" + strings.Repeat("c", 36)
	if err := os.WriteFile(filepath.Join(dir, ScenarioFile), []byte("label "+tok), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(Options{Dir: dir, Harness: "codex", Event: "Stop", HookCwd: "/d/" + tok, Now: now}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, filepath.Join(dir, "codex.jsonl"))
	if strings.Contains(recs[0].Scenario, tok) || strings.Contains(recs[0].HookCwd, tok) || recs[0].Redactions != 2 {
		t.Errorf("record = %+v", recs[0])
	}
}

func TestCaptureRejectsUnsafeHarnessName(t *testing.T) {
	for _, h := range []string{"", "../x", "a/b", `a\b`, ".."} {
		if _, err := Capture(Options{Dir: t.TempDir(), Harness: h, Event: "Stop", Now: now}, []byte("{}")); err == nil {
			t.Errorf("harness %q accepted", h)
		}
	}
}

func TestCaptureDenyMarkerOnlyInToolInput(t *testing.T) {
	in := `{"prompt":"` + DenyMarker + `","cwd":"/` + DenyMarker + `","tool_input":{"command":"ls"}}`
	out, err := Capture(Options{Dir: t.TempDir(), Harness: "codex", Event: "PreToolUse", Now: now}, []byte(in))
	if err != nil || out != "" {
		t.Errorf("Capture = %q, %v; want no deny", out, err)
	}
}

func TestCaptureRedactsNonObjectJSON(t *testing.T) {
	dir := t.TempDir()
	tok := "ghp_" + strings.Repeat("d", 36)
	if _, err := Capture(Options{Dir: dir, Harness: "h", Event: "Stop", Now: now}, []byte(`["a\n`+tok+`"]`)); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, filepath.Join(dir, "h.jsonl"))
	if strings.Contains(string(recs[0].Payload), tok) || recs[0].Redactions != 1 {
		t.Errorf("record = %+v", recs[0])
	}
}

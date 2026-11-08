// Package capture records raw harness hook payloads for study and test fixtures.
// It backs scripts/hook-capture.sh: a throwaway logging hook installed into a
// scratch project, never into a real harness home. Each event becomes one
// redacted JSON line, tagged with the scenario the operator is running.
//
// Like orch-hook, a capture hook must never break the session it observes:
// Record reports errors to its caller, and the command exits zero regardless.
package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/redact"
)

// DenyMarker in a PreToolUse tool input makes the capture hook refuse the call,
// so a scenario can check whether the harness honors a hook's deny.
const DenyMarker = "HOOK_CAPTURE_DENY"

// ScenarioFile, inside the capture directory, holds the label of the scenario
// being recorded; the script writes it before each scenario.
const ScenarioFile = "scenario"

// maxToolResponse bounds the stored tool_response. Its content is not under
// study (only where the call ran), and file reads would bloat the fixtures.
const maxToolResponse = 2048

// Record is one captured hook invocation.
type Record struct {
	At       string `json:"at"`
	Harness  string `json:"harness"`
	Event    string `json:"event"`
	Scenario string `json:"scenario,omitempty"`
	// HookCwd is the working directory the harness started the hook process in,
	// which may differ from the payload's cwd field.
	HookCwd string          `json:"hook_cwd"`
	Payload json.RawMessage `json:"payload"`
	// Redactions counts secret-shaped substrings replaced in the payload.
	Redactions int `json:"redactions,omitempty"`
}

// Options configures one capture.
type Options struct {
	Dir     string // capture directory; lines go to <Dir>/<Harness>.jsonl
	Harness string
	Event   string
	HookCwd string
	Now     time.Time
}

// Capture appends the payload read from stdin as one JSON line and returns what
// the hook should print on stdout: a deny decision for a PreToolUse call
// carrying DenyMarker, otherwise nothing.
func Capture(opts Options, stdin []byte) (string, error) {
	if opts.Harness == "" || strings.ContainsAny(opts.Harness, `/\`) || strings.Contains(opts.Harness, "..") {
		return "", fmt.Errorf("invalid harness name %q", opts.Harness)
	}
	payload, n := sanitize(stdin)
	scenario, n1 := redact.Redact(readScenario(opts.Dir))
	hookCwd, n2 := redact.Redact(opts.HookCwd)
	rec := Record{
		At:         opts.Now.UTC().Format(time.RFC3339Nano),
		Harness:    opts.Harness,
		Event:      opts.Event,
		Scenario:   scenario,
		HookCwd:    hookCwd,
		Payload:    payload,
		Redactions: n + n1 + n2,
	}
	if err := appendLine(filepath.Join(opts.Dir, opts.Harness+".jsonl"), rec); err != nil {
		return "", err
	}
	if opts.Event == "PreToolUse" && toolInputHasMarker(stdin) {
		return denyOutput(), nil
	}
	return "", nil
}

func toolInputHasMarker(stdin []byte) bool {
	var p struct {
		ToolInput json.RawMessage `json:"tool_input"`
	}
	return json.Unmarshal(stdin, &p) == nil && strings.Contains(string(p.ToolInput), DenyMarker)
}

// sanitize redacts secrets and trims tool_response. Redaction runs on decoded
// string values: on encoded JSON, an escape such as \n before a token hides its
// word boundary, and a multi-line match could swallow JSON structure. Input
// that is not JSON is kept as a redacted JSON string, so a malformed payload is
// still recorded.
func sanitize(raw []byte) (json.RawMessage, int) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		text, n := redact.Redact(string(raw))
		b, _ := json.Marshal(text)
		return b, n
	}
	if fields, ok := v.(map[string]any); ok {
		if tr, ok := fields["tool_response"]; ok {
			if b, _ := json.Marshal(tr); len(b) > maxToolResponse {
				fields["tool_response"] = fmt.Sprintf("[truncated %d bytes]", len(b))
			}
		}
	}
	n := 0
	clean := redactValue(v, &n)
	b, _ := json.Marshal(clean)
	return b, n
}

// redactValue returns v with every string, including object keys, redacted.
func redactValue(v any, n *int) any {
	switch t := v.(type) {
	case string:
		s, c := redact.Redact(t)
		*n += c
		return s
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			key, c := redact.Redact(k)
			*n += c
			out[key] = redactValue(val, n)
		}
		return out
	case []any:
		for i := range t {
			t[i] = redactValue(t[i], n)
		}
		return t
	}
	return v
}

func readScenario(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ScenarioFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func appendLine(path string, rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// denyOutput uses the hookSpecificOutput shape both Claude Code and Codex
// accept; Codex rejects a deny without a non-empty reason.
func denyOutput() string {
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": "hook-capture: denied by " + DenyMarker,
	}}
	b, _ := json.Marshal(out)
	return string(b)
}

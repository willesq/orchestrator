package install

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The regressions below all presented the same way: `orch status` reported a
// fully-wired harness as drifted, and `orch install --apply` "fixed" it by
// rewriting files it had no reason to touch.

// TestPendingIgnoresHarnessFormatting covers the core false positive: the plan is
// compared against a canonical re-encoding of the whole file, so any formatting
// the harness owns — key order, HTML escaping, indentation — used to read as drift.
func TestPendingIgnoresHarnessFormatting(t *testing.T) {
	tests := []struct {
		name     string
		settings string
	}{
		{
			name: "keys in harness insertion order rather than sorted",
			settings: `{
  "theme": "dark",
  "mcpServers": {"rag": {"command": "/opt/orch/orch", "args": ["mcp", "rag"]},
                 "queue": {"command": "/opt/orch/orch", "args": ["mcp", "queue"]}},
  "env": {"A": "1"}
}`,
		},
		{
			name: "shell metacharacters left unescaped by the harness",
			settings: `{
  "hooks": {"PostToolUse": [{"hooks": [{"type": "command", "command": "log 2>&1 && echo <done>"}]}]},
  "mcpServers": {}
}`,
		},
		{
			name:     "four-space indentation",
			settings: "{\n    \"theme\": \"dark\",\n    \"mcpServers\": {}\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.settings), 0o644); err != nil {
				t.Fatal(err)
			}
			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			// Wire it, then reformat the file the way a harness would and confirm
			// the plan still reads as installed.
			if err := plan.Apply(io.Discard, filepath.Join(t.TempDir(), "backups")); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			reformatted := reformat(t, path)
			if err := os.WriteFile(path, reformatted, 0o644); err != nil {
				t.Fatal(err)
			}
			n, err := plan.Pending()
			if err != nil {
				t.Fatalf("Pending: %v", err)
			}
			if n != 0 {
				var buf strings.Builder
				_ = plan.Print(&buf)
				t.Errorf("Pending() = %d after a formatting-only rewrite, want 0\n%s", n, buf.String())
			}
			// An apply over a formatting-only difference must leave the file alone.
			if err := plan.Apply(io.Discard, filepath.Join(t.TempDir(), "backups")); err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(reformatted) {
				t.Errorf("Apply rewrote a file that only differed in formatting:\n%s", after)
			}
		})
	}
}

// reformat re-emits a JSON file with different indentation and without Go's HTML
// escaping, imitating how Claude Code and friends rewrite their own settings.
func reformat(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "\t")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

// TestApplyHooksPreservesPosition covers the second false positive: a third-party
// hook registered after ours used to push the orchestrator entry to the end of the
// array on every apply, so the wiring never converged.
func TestApplyHooksPreservesPosition(t *testing.T) {
	const orchHook = "/opt/orch/orch-hook"
	orca := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/orca/claude-hook.sh"}}}
	tests := []struct {
		name      string
		existing  []any
		wantIndex int
	}{
		{
			name:      "orchestrator entry stays ahead of a later third-party hook",
			existing:  []any{orchEntry(orchHook, "stop"), orca},
			wantIndex: 0,
		},
		{
			name:      "orchestrator entry stays behind an earlier third-party hook",
			existing:  []any{orca, orchEntry(orchHook, "stop")},
			wantIndex: 1,
		},
		{
			name:      "a missing entry is appended",
			existing:  []any{orca},
			wantIndex: 1,
		},
		{
			name:      "duplicates collapse into the first slot",
			existing:  []any{orchEntry(orchHook, "stop"), orca, orchEntry(orchHook, "stop")},
			wantIndex: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{"hooks": map[string]any{"Stop": tt.existing}}
			applyHooks(root, orchHook)
			got := root["hooks"].(map[string]any)["Stop"].([]any)
			idx, count := -1, 0
			for i, item := range got {
				if isOrchHook(item, orchHook) {
					count++
					if idx == -1 {
						idx = i
					}
				}
			}
			if count != 1 {
				t.Errorf("orchestrator entries = %d, want exactly 1", count)
			}
			if idx != tt.wantIndex {
				t.Errorf("orchestrator entry at index %d, want %d", idx, tt.wantIndex)
			}
			if len(got) != 2 {
				t.Errorf("entry count = %d, want 2 (ours plus the third-party hook)", len(got))
			}
		})
	}
}

func orchEntry(bin, slug string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": bin + " " + slug}}}
}

// TestIsHomebrewManagedResolvesSymlinks covers the third false positive: bins come
// from os.Executable(), which reports the <brew prefix>/bin symlink, so the Cellar
// check missed and the plan tried to add a launchd agent competing with brew.
func TestIsHomebrewManagedResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	cellarBin := filepath.Join(root, "Cellar", "orch", "0.4.2", "bin")
	prefixBin := filepath.Join(root, "bin")
	for _, dir := range []string{cellarBin, prefixBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(cellarBin, "orchd")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(prefixBin, "orchd")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "build", "bin", "orchd")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		bin  string
		want bool
	}{
		{name: "cellar path", bin: target, want: true},
		{name: "brew prefix symlink", bin: link, want: true},
		{name: "source build", bin: source, want: false},
		{name: "missing binary", bin: filepath.Join(root, "bin", "absent"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHomebrewManaged(tt.bin); got != tt.want {
				t.Errorf("isHomebrewManaged(%q) = %v, want %v", tt.bin, got, tt.want)
			}
		})
	}
}

// TestPlistSkippedForSymlinkedHomebrewBin is the user-visible consequence: a
// Homebrew install must contribute no pending plist edit at all.
func TestPlistSkippedForSymlinkedHomebrewBin(t *testing.T) {
	tests := []struct{ name string }{{name: "brew-managed orchd leaves the plist alone"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cellarBin := filepath.Join(root, "Cellar", "orch", "0.4.2", "bin")
			if err := os.MkdirAll(cellarBin, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(cellarBin, "orchd")
			if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "bin", "orchd")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			home := t.TempDir()
			e := plistEdit(link, home)
			ev, err := e.evaluate()
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if ev.changed {
				t.Errorf("plist edit pending for a brew-managed orchd: %s", ev.summary)
			}
			if !strings.Contains(ev.summary, "skipped") {
				t.Errorf("summary should note the skip: %s", ev.summary)
			}
			if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.orchestrator.orchd.plist")); !os.IsNotExist(err) {
				t.Errorf("evaluate must not create the plist")
			}
		})
	}
}

// TestNormalizeRoundTrips guards the shared encoders the comparison depends on:
// normalizing an already-canonical file must be a no-op, and an unparseable file
// must surface an error rather than silently reading as installed.
func TestNormalizeRoundTrips(t *testing.T) {
	tests := []struct {
		name      string
		normalize func([]byte) ([]byte, error)
		input     string
		want      string
		wantErr   bool
	}{
		{
			name:      "json reorders and escapes to the canonical form",
			normalize: normalizeJSON,
			input:     "{\"b\": 1, \"a\": \"x>y\"}",
			want:      "{\n  \"a\": \"x\\u003ey\",\n  \"b\": 1\n}\n",
		},
		{name: "empty json stays empty", normalize: normalizeJSON, input: "", want: ""},
		{name: "invalid json errors", normalize: normalizeJSON, input: "{nope", wantErr: true},
		{
			name:      "toml round-trips through one encoder",
			normalize: normalizeTOML,
			input:     "[a]\nx = 'y'\n",
			want:      "[a]\nx = 'y'\n",
		},
		{name: "empty toml stays empty", normalize: normalizeTOML, input: "", want: ""},
		{name: "invalid toml errors", normalize: normalizeTOML, input: "= nope", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.normalize([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEditRejectsUnparseableFile pins the other half of that contract: a file the
// edit itself cannot parse must fail loudly, naming the file, rather than being
// silently overwritten with a tree built from nothing.
func TestEditRejectsUnparseableFile(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(Bins, string) Edit
		rel     string
		content string
		wantErr string
	}{
		{
			name:    "claude settings.json",
			edit:    func(b Bins, home string) Edit { return ClaudeCode{}.settingsEdit(b, home) },
			rel:     ".claude/settings.json",
			content: "{ not json",
			wantErr: "parse settings.json",
		},
		{
			name:    "claude .claude.json",
			edit:    func(b Bins, home string) Edit { return ClaudeCode{}.claudeJSONEdit(b, home) },
			rel:     ".claude.json",
			content: "{ not json",
			wantErr: "parse .claude.json",
		},
		{
			name:    "codex config.toml",
			edit:    codexTOMLEdit,
			rel:     ".codex/config.toml",
			content: "= not toml",
			wantErr: "parse codex config.toml",
		},
		{
			name:    "codex hooks.json",
			edit:    codexHooksEdit,
			rel:     ".codex/hooks.json",
			content: "{ not json",
			wantErr: "parse codex hooks.json",
		},
		{
			name:    "gemini settings.json",
			edit:    geminiSettingsEdit,
			rel:     ".gemini/settings.json",
			content: "{ not json",
			wantErr: "parse gemini settings.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, filepath.FromSlash(tt.rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			e := tt.edit(testBins(), home)
			if e.Path != path {
				t.Fatalf("edit path = %s, want %s", e.Path, path)
			}
			if _, err := e.evaluate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("evaluate() err = %v, want one mentioning %q", err, tt.wantErr)
			} else if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q should name the file", err)
			}
			// The bad file must survive untouched for the user to fix by hand.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tt.content {
				t.Errorf("file was modified: %q", after)
			}
		})
	}
}

// TestEvaluateKeepsUnparseableFilePending makes sure the semantic comparison never
// masks a broken file: if it can't be normalized, the edit stays pending.
func TestEvaluateKeepsUnparseableFilePending(t *testing.T) {
	tests := []struct{ name string }{{name: "a file the normalizer rejects stays pending"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
				t.Fatal(err)
			}
			e := Edit{
				Path:      path,
				Normalize: normalizeJSON,
				Compute: func([]byte) ([]byte, string, error) {
					return []byte("{}\n"), "rewrite", nil
				},
			}
			ev, err := e.evaluate()
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if !ev.changed {
				t.Error("changed = false, want true for a file that cannot be normalized")
			}
		})
	}
}

package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func testBins() Bins {
	return Bins{Orch: "/opt/orch/orch", OrchHook: "/opt/orch/orch-hook", Orchd: "/opt/orch/orchd"}
}

func TestAdapterFor(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "claude-code", target: "claude-code", wantErr: false},
		{name: "unknown", target: "emacs", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := AdapterFor(tt.target)
			if (err != nil) != tt.wantErr {
				t.Errorf("AdapterFor(%q) err = %v, wantErr %v", tt.target, err, tt.wantErr)
			}
		})
	}
}

func TestApplyHooksSurgical(t *testing.T) {
	tests := []struct {
		name          string
		existing      map[string]any
		wantPreserved bool // a non-orchestrator entry survives
	}{
		{
			name:     "empty settings gets 7 events",
			existing: map[string]any{},
		},
		{
			name: "preserves third-party, replaces orchestrator",
			existing: map[string]any{
				"hooks": map[string]any{
					"UserPromptSubmit": []any{
						map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/opt/orch/orch-hook user-prompt-submit"}}},
						map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/some/orca/claude-hook.sh"}}},
					},
				},
			},
			wantPreserved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{}
			maps.Copy(root, tt.existing)
			summary := applyHooks(root, "/opt/orch/orch-hook")
			hooks := root["hooks"].(map[string]any)
			if len(hooks) != len(eventSlugs) {
				t.Errorf("got %d events, want %d", len(hooks), len(eventSlugs))
			}
			ups := hooks["UserPromptSubmit"].([]any)
			// exactly one orchestrator entry after apply
			orchCount, orcaFound := 0, false
			for _, item := range ups {
				cmd := item.(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
				if strings.Contains(cmd, "orch-hook") {
					orchCount++
				}
				if strings.Contains(cmd, "orca") {
					orcaFound = true
				}
			}
			if orchCount != 1 {
				t.Errorf("orchestrator entries = %d, want 1 (summary: %s)", orchCount, summary)
			}
			if tt.wantPreserved && !orcaFound {
				t.Errorf("third-party Orca hook was not preserved")
			}
		})
	}
}

func TestApplyMCP(t *testing.T) {
	tests := []struct {
		name          string
		existing      map[string]any
		wantPreserved int
	}{
		{name: "empty", existing: map[string]any{}, wantPreserved: 0},
		{
			name: "preserves serena",
			existing: map[string]any{
				"mcpServers": map[string]any{"serena": map[string]any{"command": "serena"}},
			},
			wantPreserved: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{}
			maps.Copy(root, tt.existing)
			applyMCP(root, "/opt/orch/orch")
			mcp := root["mcpServers"].(map[string]any)
			if _, ok := mcp["rag"]; !ok {
				t.Errorf("rag not registered")
			}
			if _, ok := mcp["queue"]; !ok {
				t.Errorf("queue not registered")
			}
			if tt.wantPreserved == 1 {
				if _, ok := mcp["serena"]; !ok {
					t.Errorf("serena not preserved")
				}
			}
		})
	}
}

// TestApplyPermissions covers the other half of MCP wiring: a registered server
// whose tools are not in permissions.allow still prompts for every call, which
// unattended hooks and background agents cannot answer.
func TestApplyPermissions(t *testing.T) {
	tests := []struct {
		name          string
		existing      map[string]any
		wantAllow     []string
		wantNotAllow  []string
		wantOtherKept string
		wantSummary   string
	}{
		{
			name:      "no permissions block yet",
			existing:  map[string]any{},
			wantAllow: []string{"mcp__rag__*", "mcp__queue__*"},
		},
		{
			name: "preserves unrelated allow rules",
			existing: map[string]any{
				"permissions": map[string]any{"allow": []any{"Bash(go test:*)"}},
			},
			wantAllow:     []string{"mcp__rag__*", "mcp__queue__*"},
			wantOtherKept: "Bash(go test:*)",
		},
		{
			name: "wildcard rule already present is not duplicated",
			existing: map[string]any{
				"permissions": map[string]any{"allow": []any{"mcp__rag__*", "mcp__queue__*"}},
			},
			wantAllow:   []string{"mcp__rag__*", "mcp__queue__*"},
			wantSummary: "already allowed",
		},
		{
			name: "bare server rule counts as covered",
			existing: map[string]any{
				"permissions": map[string]any{"allow": []any{"mcp__rag", "mcp__queue"}},
			},
			wantNotAllow: []string{"mcp__rag__*", "mcp__queue__*"},
			wantSummary:  "already allowed",
		},
		{
			name: "blanket mcp allow counts as covered",
			existing: map[string]any{
				"permissions": map[string]any{"allow": []any{"mcp__*"}},
			},
			wantNotAllow: []string{"mcp__rag__*", "mcp__queue__*"},
			wantSummary:  "already allowed",
		},
		// Every deny spelling below must hold the allow rule back. A deny beats an
		// allow in Claude Code, so appending over one is dead config at best.
		{
			name: "deny of the server wildcard is respected",
			existing: map[string]any{
				"permissions": map[string]any{"deny": []any{"mcp__queue__*"}},
			},
			wantAllow:    []string{"mcp__rag__*"},
			wantNotAllow: []string{"mcp__queue__*"},
			wantSummary:  "an existing deny rule",
		},
		{
			name: "deny of the bare server name is respected",
			existing: map[string]any{
				"permissions": map[string]any{"deny": []any{"mcp__queue"}},
			},
			wantAllow:    []string{"mcp__rag__*"},
			wantNotAllow: []string{"mcp__queue__*"},
			wantSummary:  "an existing deny rule",
		},
		{
			name: "deny of a single tool holds back the whole wildcard",
			existing: map[string]any{
				"permissions": map[string]any{"deny": []any{"mcp__rag__rag_delete_source"}},
			},
			wantAllow:    []string{"mcp__queue__*"},
			wantNotAllow: []string{"mcp__rag__*"},
			wantSummary:  "an existing deny rule",
		},
		{
			name: "blanket mcp deny holds back every server",
			existing: map[string]any{
				"permissions": map[string]any{"deny": []any{"mcp__*"}},
			},
			wantNotAllow: []string{"mcp__rag__*", "mcp__queue__*"},
			wantSummary:  "an existing deny rule",
		},
		{
			name: "deny wins over a stale allow for the same server",
			existing: map[string]any{
				"permissions": map[string]any{
					"allow": []any{"mcp__rag__*"},
					"deny":  []any{"mcp__rag__*"},
				},
			},
			wantAllow:   []string{"mcp__queue__*"},
			wantSummary: "an existing deny rule",
		},
		// An ask rule is how a user keeps a specific tool gated behind
		// confirmation. It outranks allow, so appending over it would write a
		// contradiction into their file and report a gate we did not remove.
		{
			name: "ask on the server wildcard is respected",
			existing: map[string]any{
				"permissions": map[string]any{"ask": []any{"mcp__rag__*"}},
			},
			wantAllow:    []string{"mcp__queue__*"},
			wantNotAllow: []string{"mcp__rag__*"},
			wantSummary:  "an existing ask rule",
		},
		{
			name: "ask on a single destructive tool holds back the wildcard",
			existing: map[string]any{
				"permissions": map[string]any{"ask": []any{"mcp__rag__rag_delete_source"}},
			},
			wantAllow:    []string{"mcp__queue__*"},
			wantNotAllow: []string{"mcp__rag__*"},
			wantSummary:  "an existing ask rule",
		},
		{
			name: "blanket mcp ask holds back every server",
			existing: map[string]any{
				"permissions": map[string]any{"ask": []any{"mcp__*"}},
			},
			wantNotAllow: []string{"mcp__rag__*", "mcp__queue__*"},
			wantSummary:  "an existing ask rule",
		},
		{
			name: "deny is reported ahead of ask when both cover a server",
			existing: map[string]any{
				"permissions": map[string]any{
					"deny": []any{"mcp__rag__*"},
					"ask":  []any{"mcp__rag__*"},
				},
			},
			wantAllow:   []string{"mcp__queue__*"},
			wantSummary: "an existing deny rule",
		},
		{
			name:        "exploit-finder sandbox rule is added on a fresh file",
			existing:    map[string]any{},
			wantAllow:   []string{exploitFinderSandboxRule},
			wantSummary: "exploit-finder PoCs run in its sandboxed container",
		},
		{
			name: "existing sandbox rule is not duplicated",
			existing: map[string]any{
				"permissions": map[string]any{"allow": []any{exploitFinderSandboxRule}},
			},
			wantAllow: []string{exploitFinderSandboxRule},
		},
		{
			name: "a deny of the sandbox rule is respected",
			existing: map[string]any{
				"permissions": map[string]any{"deny": []any{exploitFinderSandboxRule}},
			},
			wantNotAllow: []string{exploitFinderSandboxRule},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{}
			maps.Copy(root, tt.existing)
			summary, err := applyPermissions(root)
			if err != nil {
				t.Fatalf("applyPermissions: %v", err)
			}

			allow := map[string]bool{}
			if perms, ok := root["permissions"].(map[string]any); ok {
				rules, _ := perms["allow"].([]any)
				for _, r := range rules {
					if s, ok := r.(string); ok {
						allow[s] = true
					}
				}
			}
			for _, want := range tt.wantAllow {
				if !allow[want] {
					t.Errorf("permissions.allow missing %q, got %v", want, allow)
				}
			}
			for _, unwanted := range tt.wantNotAllow {
				if allow[unwanted] {
					t.Errorf("permissions.allow gained redundant/denied rule %q", unwanted)
				}
			}
			if tt.wantOtherKept != "" && !allow[tt.wantOtherKept] {
				t.Errorf("pre-existing rule %q was dropped", tt.wantOtherKept)
			}
			if tt.wantSummary != "" && !strings.Contains(summary, tt.wantSummary) {
				t.Errorf("summary %q does not mention %q", summary, tt.wantSummary)
			}
		})
	}
}

// TestApplyPermissionsRefusesMalformedShapes covers the other half of "this file
// belongs to the user": a container whose type we do not recognize is reported,
// not quietly replaced with an empty one of the right type.
func TestApplyPermissionsRefusesMalformedShapes(t *testing.T) {
	tests := []struct {
		name     string
		root     map[string]any
		wantErr  string
		wantKept any
	}{
		{
			name:     "permissions is an array",
			root:     map[string]any{"permissions": []any{"mcp__rag__*"}},
			wantErr:  "permissions is []interface {}, not an object",
			wantKept: []any{"mcp__rag__*"},
		},
		{
			name: "allow is a bare string",
			root: map[string]any{
				"permissions": map[string]any{"allow": "Bash(deploy:*)"},
			},
			wantErr:  "permissions.allow is string, not an array",
			wantKept: "Bash(deploy:*)",
		},
		{
			name: "deny is a bare string",
			root: map[string]any{
				"permissions": map[string]any{"deny": "mcp__rag__*"},
			},
			wantErr:  "permissions.deny is string, not an array",
			wantKept: "mcp__rag__*",
		},
		{
			name: "ask is a bare string",
			root: map[string]any{
				"permissions": map[string]any{"ask": "mcp__rag__*"},
			},
			wantErr:  "permissions.ask is string, not an array",
			wantKept: "mcp__rag__*",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := applyPermissions(tt.root)
			if err == nil {
				t.Fatalf("applyPermissions accepted a malformed shape, root is now %v", tt.root)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			// The refusal must leave the user's value untouched.
			got := tt.root["permissions"]
			if perms, ok := got.(map[string]any); ok {
				for _, k := range []string{"allow", "deny", "ask"} {
					if v, present := perms[k]; present {
						got = v
					}
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.wantKept) {
				t.Errorf("original value was modified: got %v, want %v", got, tt.wantKept)
			}
		})
	}
}

// TestSettingsEditSurfacesMalformedPermissions makes sure the refusal reaches the
// caller as a plan error instead of being swallowed into a partial write.
func TestSettingsEditSurfacesMalformedPermissions(t *testing.T) {
	tests := []struct {
		name string
		seed string
	}{
		{name: "permissions of the wrong type aborts the edit", seed: `{"permissions": 7}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.seed), 0o644); err != nil {
				t.Fatal(err)
			}
			err := BuildPlan(ClaudeCode{}, testBins(), home).Apply(io.Discard, filepath.Join(t.TempDir(), "backups"))
			if err == nil {
				t.Fatalf("Apply succeeded over a malformed permissions block")
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != tt.seed {
				t.Errorf("settings.json was rewritten despite the error:\n%s", after)
			}
		})
	}
}

// TestApplyPermissionsIsIdempotent guards the drift regression class: a second
// pass must not append the rules again or `orch status` reports a wired harness
// as drifted forever.
func TestApplyPermissionsIsIdempotent(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "repeat apply leaves the allow list unchanged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{}
			if _, err := applyPermissions(root); err != nil {
				t.Fatalf("applyPermissions: %v", err)
			}
			first := root["permissions"].(map[string]any)["allow"].([]any)
			if _, err := applyPermissions(root); err != nil {
				t.Fatalf("applyPermissions: %v", err)
			}
			second := root["permissions"].(map[string]any)["allow"].([]any)
			if len(first) != len(second) {
				t.Errorf("allow list grew on re-apply: %v -> %v", first, second)
			}
		})
	}
}

// TestApplyPermissionsKeepsUnrecognizedEntries makes sure a rule shape the
// orchestrator does not understand is carried through rather than dropped —
// settings.json belongs to the user, not to us.
func TestApplyPermissionsKeepsUnrecognizedEntries(t *testing.T) {
	tests := []struct {
		name     string
		existing []any
	}{
		{name: "non-string entry", existing: []any{float64(7)}},
		{name: "nested object entry", existing: []any{map[string]any{"tool": "Bash"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := map[string]any{
				"permissions": map[string]any{"allow": tt.existing},
			}
			if _, err := applyPermissions(root); err != nil {
				t.Fatalf("applyPermissions: %v", err)
			}
			got := root["permissions"].(map[string]any)["allow"].([]any)
			// original entry + one rule per orch MCP server + the exploit-finder
			// sandbox bash rule.
			if len(got) != len(tt.existing)+len(orchMCPServers)+1 {
				t.Fatalf("allow = %v, want the original entry plus one rule per orch server plus the sandbox rule", got)
			}
			if fmt.Sprint(got[0]) != fmt.Sprint(tt.existing[0]) {
				t.Errorf("unrecognized entry %v was rewritten to %v", tt.existing[0], got[0])
			}
		})
	}
}

func TestPlanPrintAndApply(t *testing.T) {
	tests := []struct {
		name  string
		apply bool
	}{
		{name: "dry-run writes nothing", apply: false},
		{name: "apply writes files", apply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			var out bytes.Buffer
			settings := filepath.Join(home, ".claude", "settings.json")

			if tt.apply {
				if err := plan.Apply(&out, filepath.Join(t.TempDir(), "backups")); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				b, err := os.ReadFile(settings)
				if err != nil {
					t.Fatalf("settings not written: %v", err)
				}
				var root map[string]any
				if err := json.Unmarshal(b, &root); err != nil {
					t.Fatalf("settings not valid JSON: %v", err)
				}
				if _, ok := root["hooks"]; !ok {
					t.Errorf("hooks not written")
				}
				// A registered MCP server is unusable from an unattended hook
				// unless its tools are also allowed.
				perms, ok := root["permissions"].(map[string]any)
				if !ok {
					t.Fatalf("permissions not written: %v", root)
				}
				rules, _ := perms["allow"].([]any)
				for _, want := range []string{"mcp__rag__*", "mcp__queue__*"} {
					found := false
					for _, r := range rules {
						if r == want {
							found = true
						}
					}
					if !found {
						t.Errorf("settings.json permissions.allow missing %q, got %v", want, rules)
					}
				}
			} else {
				if err := plan.Print(&out); err != nil {
					t.Fatalf("Print: %v", err)
				}
				if _, err := os.Stat(settings); !os.IsNotExist(err) {
					t.Errorf("dry-run must not write settings.json")
				}
				if !strings.Contains(out.String(), "DRY RUN") {
					t.Errorf("dry-run banner missing")
				}
			}
		})
	}
}

func TestApplyBacksUpExisting(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "existing file is backed up before overwrite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			settings := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(settings, []byte(`{"mcpServers":{"serena":{"command":"serena"}}}`), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			backupDir := filepath.Join(t.TempDir(), "backups")
			var out bytes.Buffer
			if err := BuildPlan(ClaudeCode{}, testBins(), home).Apply(&out, backupDir); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			// The prior settings.json must have been backed up.
			found := false
			_ = filepath.WalkDir(backupDir, func(_ string, d fs.DirEntry, _ error) error {
				if d != nil && !d.IsDir() {
					found = true
				}
				return nil
			})
			if !found {
				t.Errorf("no backup written for the overwritten settings.json")
			}
			// serena must survive the surgical rewrite.
			b, _ := os.ReadFile(settings)
			if !strings.Contains(string(b), "serena") {
				t.Errorf("serena MCP not preserved after apply")
			}
		})
	}
}

// TestBackupNeverWidensMode guards a credential-exposure regression: backups were
// written at a fixed 0644 under 0755 directories, so a 0600 harness config —
// ~/.claude.json carries OAuth state and MCP env blocks — got a world-readable
// copy that is then retained indefinitely.
func TestBackupNeverWidensMode(t *testing.T) {
	tests := []struct {
		name     string
		srcMode  os.FileMode
		wantMode os.FileMode
	}{
		{name: "private source stays private", srcMode: 0o600, wantMode: 0o600},
		{name: "group/world-readable source is tightened", srcMode: 0o644, wantMode: 0o600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(src, []byte(`{"theme":"dark"}`), tt.srcMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(src, tt.srcMode); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "backups", "20260930-000000")
			if err := backup(dest, src, []byte(`{"theme":"dark"}`)); err != nil {
				t.Fatalf("backup: %v", err)
			}

			var found string
			_ = filepath.WalkDir(dest, func(p string, d fs.DirEntry, _ error) error {
				if d != nil && !d.IsDir() {
					found = p
				}
				return nil
			})
			if found == "" {
				t.Fatalf("no backup written under %s", dest)
			}
			fi, err := os.Stat(found)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tt.wantMode {
				t.Errorf("backup mode = %04o, want %04o (source was %04o)", got, tt.wantMode, tt.srcMode)
			}
			if got := fi.Mode().Perm() & 0o077; got != 0 {
				t.Errorf("backup grants group/other %04o — config backups must be owner-only", got)
			}
			// The directory holding it must not be traversable by others either.
			di, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if got := di.Mode().Perm() & 0o077; got != 0 {
				t.Errorf("backup dir grants group/other %04o, want owner-only", got)
			}
		})
	}
}

// TestBackupModeIsUnconditional covers the two ways os.WriteFile's mode argument
// quietly stops applying: it only takes effect when the file is created, and it
// follows symlinks. Either would downgrade the 0600 guarantee to best-effort if
// something already sat at the destination.
func TestBackupModeIsUnconditional(t *testing.T) {
	tests := []struct {
		name  string
		plant func(t *testing.T, dest, rel string) (checkPath string)
	}{
		{
			name: "pre-existing loose file at the destination",
			plant: func(t *testing.T, dest, rel string) string {
				if err := os.MkdirAll(dest, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(rel, []byte("stale"), 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(rel, 0o666); err != nil {
					t.Fatal(err)
				}
				return rel
			},
		},
		{
			name: "symlink planted at the destination",
			plant: func(t *testing.T, dest, rel string) string {
				if err := os.MkdirAll(dest, 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "attacker-readable")
				if err := os.WriteFile(target, []byte("empty"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, rel); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// The symlink target must not have received the secret.
					b, err := os.ReadFile(target)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(b), "SECRET") {
						t.Errorf("backup followed the symlink and wrote config through it")
					}
				})
				return rel
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(src, []byte(`{"token":"SECRET"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "backups", "20260930-000000")
			rel := filepath.Join(dest, filepath.Base(filepath.Dir(src))+"__settings.json")
			check := tt.plant(t, dest, rel)

			if err := backup(dest, src, []byte(`{"token":"SECRET"}`)); err != nil {
				t.Fatalf("backup: %v", err)
			}
			fi, err := os.Lstat(check)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("destination is still a symlink; backup wrote through it")
			}
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("backup mode = %04o, want 0600", got)
			}
		})
	}
}

// TestApplyIsAllOrNothing guards the half-installed state: evaluation reads files
// the installer does not own, so a failure there must abort before any file is
// written rather than part-way through the plan.
func TestApplyIsAllOrNothing(t *testing.T) {
	tests := []struct {
		name string
		seed string
	}{
		{name: "a malformed settings.json aborts before any edit is written", seed: `{"permissions": 7}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			settings := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(settings, []byte(tt.seed), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, home)

			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			if err := plan.Apply(io.Discard, filepath.Join(t.TempDir(), "backups")); err == nil {
				t.Fatalf("Apply succeeded despite a malformed settings.json")
			}
			// Nothing at all may have been written — not the shared policy, not
			// CLAUDE.md, not the edits that would have come after the failure.
			after := snapshot(t, home)
			if len(after) != len(before) {
				t.Errorf("Apply wrote %d file(s) before aborting; plan must be all-or-nothing\nbefore: %v\nafter:  %v",
					len(after)-len(before), keysOf(before), keysOf(after))
			}
			for path, content := range before {
				if after[path] != content {
					t.Errorf("%s was modified despite the aborted plan", path)
				}
			}
			// The dry run must fail the same way rather than printing a partial plan.
			if err := plan.Print(io.Discard); err == nil {
				t.Errorf("Print succeeded despite a malformed settings.json")
			}
			if _, err := plan.Pending(); err == nil {
				t.Errorf("Pending succeeded despite a malformed settings.json")
			}
		})
	}
}

// TestApplyRefusesStaleWrite covers the lost-update window the up-front
// evaluation opened: a harness that rewrites its own settings between evaluation
// and the write must not be silently clobbered, and must not be misrepresented in
// a backup that holds the pre-plan bytes.
func TestApplyRefusesStaleWrite(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "a file rewritten after its own evaluation is not clobbered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			victim := filepath.Join(home, "settings.json")
			const harnessWrote = `{"theme":"light"}`
			if err := os.WriteFile(victim, []byte(`{"theme":"dark"}`), 0o644); err != nil {
				t.Fatal(err)
			}

			// The plan evaluates victim first, then the second edit — whose Compute
			// stands in for the harness rewriting its own config in that window,
			// after the plan already captured the old bytes.
			raced := false
			plan := Plan{Target: "test", Edits: []Edit{
				{
					Path: victim,
					Compute: func(old []byte) ([]byte, string, error) {
						return append(old, []byte("\n// orch\n")...), "victim edit", nil
					},
				},
				{
					Path: filepath.Join(home, "other"),
					Compute: func(old []byte) ([]byte, string, error) {
						if !raced {
							raced = true
							if err := os.WriteFile(victim, []byte(harnessWrote), 0o644); err != nil {
								return nil, "", err
							}
						}
						return []byte("other"), "other edit", nil
					},
				},
			}}

			err := plan.Apply(io.Discard, filepath.Join(t.TempDir(), "backups"))
			if err == nil {
				t.Fatalf("Apply overwrote a file that changed after it was evaluated")
			}
			if !strings.Contains(err.Error(), "changed on disk") {
				t.Errorf("error %q does not explain the staleness", err)
			}
			after, readErr := os.ReadFile(victim)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != harnessWrote {
				t.Errorf("the concurrent write was clobbered:\n%s", after)
			}
			// Re-running now that nothing races must succeed and apply the edit.
			if err := plan.Apply(io.Discard, filepath.Join(t.TempDir(), "backups")); err != nil {
				t.Fatalf("re-run after the refusal failed: %v", err)
			}
			final, readErr := os.ReadFile(victim)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(final), harnessWrote) || !strings.Contains(string(final), "// orch") {
				t.Errorf("re-run did not apply the edit on top of current state:\n%s", final)
			}
		})
	}
}

// TestBackupRefusesHostileDestination pins the fail-safe create: an entry that
// reappears at the destination after the unlink must abort the backup rather than
// be written through.
func TestBackupRefusesHostileDestination(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{name: "symlink racing in after the unlink", kind: "symlink"},
		{name: "regular file racing in after the unlink", kind: "file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(src, []byte(`{"token":"SECRET"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "backups", "20260930-000000")
			if err := os.MkdirAll(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			rel := filepath.Join(dest, filepath.Base(filepath.Dir(src))+"__settings.json")

			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("PRECIOUS"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Stand in for the race by planting the entry and making it survive the
			// unlink: a read-only parent makes os.Remove fail, which must abort.
			switch tt.kind {
			case "symlink":
				if err := os.Symlink(victim, rel); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(rel, []byte("stale"), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(dest, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dest, 0o700) })

			if err := backup(dest, src, []byte(`{"token":"SECRET"}`)); err == nil {
				t.Errorf("backup succeeded against an undeletable destination")
			}
			// Whatever happened, the secret must not have reached the victim.
			b, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "PRECIOUS" {
				t.Errorf("config was written through to the symlink target: %s", b)
			}
		})
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func TestApplyIsIdempotent(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "second apply changes nothing and backs up nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)

			var out1 bytes.Buffer
			if err := plan.Apply(&out1, filepath.Join(t.TempDir(), "b1")); err != nil {
				t.Fatalf("first apply: %v", err)
			}
			before := snapshot(t, home)

			backup2 := filepath.Join(t.TempDir(), "b2")
			var out2 bytes.Buffer
			if err := plan.Apply(&out2, backup2); err != nil {
				t.Fatalf("second apply: %v", err)
			}
			after := snapshot(t, home)

			if len(before) != len(after) {
				t.Fatalf("file count changed: %d -> %d", len(before), len(after))
			}
			for p, c := range before {
				if after[p] != c {
					t.Errorf("second apply changed %s", p)
				}
			}
			if !strings.Contains(out2.String(), "already fully installed") {
				t.Errorf("second apply should report no-op, got:\n%s", out2.String())
			}
			// No backups should have been written on the idempotent second run.
			backups := snapshot(t, backup2)
			if len(backups) != 0 {
				t.Errorf("second apply wrote %d backup file(s), want 0", len(backups))
			}
		})
	}
}

func TestPrintUnchangedAfterInstall(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "dry-run after install reports already installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			if err := plan.Apply(&bytes.Buffer{}, filepath.Join(t.TempDir(), "b")); err != nil {
				t.Fatalf("apply: %v", err)
			}
			var out bytes.Buffer
			if err := plan.Print(&out); err != nil {
				t.Fatalf("print: %v", err)
			}
			if !strings.Contains(out.String(), "Already fully installed") {
				t.Errorf("expected already-installed message, got:\n%s", out.String())
			}
		})
	}
}

func TestAliasEdit(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		wantVerb string
	}{
		{name: "adds block to empty", existing: "", wantVerb: "added"},
		{name: "adds after existing content", existing: "export FOO=1\n", wantVerb: "added"},
		{name: "updates existing block", existing: aliasBegin + "\nalias orch=\"/old\"\n" + aliasEnd + "\n", wantVerb: "updated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := aliasEdit("/opt/orch/orch", t.TempDir())
			nw, summary, err := e.Compute([]byte(tt.existing))
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			s := string(nw)
			if strings.Count(s, aliasBegin) != 1 {
				t.Errorf("expected exactly one managed block, got:\n%s", s)
			}
			if !strings.Contains(s, `alias orch="/opt/orch/orch"`) {
				t.Errorf("alias line missing:\n%s", s)
			}
			if tt.existing != "" && !strings.HasPrefix(tt.existing, aliasBegin) && !strings.Contains(s, "export FOO=1") && strings.Contains(tt.existing, "FOO") {
				t.Errorf("pre-existing content lost")
			}
			if !strings.Contains(summary, tt.wantVerb) {
				t.Errorf("summary %q missing verb %q", summary, tt.wantVerb)
			}
		})
	}
}

func TestPlistEdit(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "contains orchd path and KeepAlive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := plistEdit("/opt/orch/orchd", t.TempDir())
			nw, summary, err := e.Compute(nil)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			s := string(nw)
			if !strings.Contains(s, "/opt/orch/orchd") || !strings.Contains(s, "KeepAlive") {
				t.Errorf("plist missing content:\n%s", s)
			}
			if !strings.Contains(summary, "launchd") {
				t.Errorf("summary missing: %s", summary)
			}
		})
	}
}

func TestPlistEditHomebrewManaged(t *testing.T) {
	tests := []struct {
		name     string
		orchd    string
		wantNoOp bool
	}{
		{name: "cellar path is a no-op (brew services owns it)", orchd: "/opt/homebrew/Cellar/orch/0.2.0/bin/orchd", wantNoOp: true},
		{name: "source-build path creates a plist", orchd: "/Users/me/build/bin/orchd", wantNoOp: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := plistEdit(tt.orchd, t.TempDir())
			existing := []byte("prior-plist")
			nw, summary, err := e.Compute(existing)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if tt.wantNoOp {
				if string(nw) != string(existing) {
					t.Errorf("expected no-op (unchanged bytes), got rewrite:\n%s", nw)
				}
				if !strings.Contains(summary, "skipped") {
					t.Errorf("summary should note skip: %s", summary)
				}
			} else {
				if !strings.Contains(string(nw), tt.orchd) {
					t.Errorf("plist should reference orchd path: %s", nw)
				}
			}
		})
	}
}

func TestPlanPending(t *testing.T) {
	tests := []struct {
		name        string
		applyFirst  bool
		wantPending bool
	}{
		{name: "an unwired harness has edits pending", applyFirst: false, wantPending: true},
		{name: "an installed harness has none", applyFirst: true, wantPending: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			plan := BuildPlan(ClaudeCode{}, testBins(), home)
			if tt.applyFirst {
				if err := plan.Apply(io.Discard, filepath.Join(t.TempDir(), "backups")); err != nil {
					t.Fatalf("Apply: %v", err)
				}
			}
			n, err := plan.Pending()
			if err != nil {
				t.Fatalf("Pending: %v", err)
			}
			if got := n > 0; got != tt.wantPending {
				t.Errorf("Pending() = %d, want pending=%v", n, tt.wantPending)
			}
			// Pending must never write: a status report is read-only.
			if !tt.applyFirst {
				if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
					t.Errorf("Pending must not create settings.json")
				}
			}
		})
	}
}

func TestPlanPendingComputeError(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "a failing edit is reported with its path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Plan{Target: "broken", Edits: []Edit{{
				Path: "/tmp/orch-pending-test",
				Compute: func([]byte) ([]byte, string, error) {
					return nil, "", errors.New("bad edit")
				},
			}}}
			n, err := plan.Pending()
			if err == nil || !strings.Contains(err.Error(), "bad edit") {
				t.Fatalf("got %v, want an error mentioning the failure", err)
			}
			if !strings.Contains(err.Error(), "/tmp/orch-pending-test") {
				t.Errorf("error %q should name the path", err)
			}
			if n != 0 {
				t.Errorf("count = %d, want 0 on error", n)
			}
		})
	}
}

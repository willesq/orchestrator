// Package install wires the orchestrator into a coding-agent harness. It is
// dry-run BY DEFAULT: BuildPlan computes a set of file edits, Plan.Print shows
// exactly what would change (writing nothing), and Plan.Apply performs the edits
// only when explicitly asked — backing up every file it touches first.
//
// Edits are surgical: hook/MCP registration replaces only orchestrator-owned
// entries in place, preserving third-party ones (Serena, Orca) and their order. A
// --home override lets the whole plan run against a throwaway directory for testing.
//
// Drift is measured on meaning, not bytes: harnesses rewrite their own settings in
// their own style, so an edit that would only reorder keys or change escaping is
// reported as already installed and is never written.
package install

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Bins are the absolute paths to the built binaries the install points at.
type Bins struct {
	Orch     string
	OrchHook string
	Orchd    string
}

// Adapter wires the orchestrator into one harness (Claude Code, Codex, Gemini).
type Adapter interface {
	Name() string
	// Edits returns the file transforms for this harness under the given home.
	Edits(bins Bins, home string) []Edit
}

// Edit is one file transform. Compute is a pure function of the current bytes,
// returning the new bytes plus a human-readable summary of the change.
//
// Normalize, when set, re-encodes the current bytes through the same encoder
// Compute writes with. Drift is then judged on meaning rather than on formatting
// the harness owns — key order, string escaping, indentation — so a file that
// already carries the right wiring reads as installed even though Claude Code,
// Codex or Gemini rewrote it in their own style.
type Edit struct {
	Path      string
	Compute   func(old []byte) (newBytes []byte, summary string, err error)
	Normalize func(old []byte) (normalized []byte, err error)
}

// evaluated is an edit measured against the file currently on disk.
type evaluated struct {
	old     []byte
	next    []byte
	summary string
	changed bool
}

// evaluate computes the edit against the file on disk, writing nothing.
func (e Edit) evaluate() (evaluated, error) {
	old, readErr := os.ReadFile(e.Path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return evaluated{}, fmt.Errorf("%s: %w", e.Path, readErr)
	}
	next, summary, err := e.Compute(old)
	if err != nil {
		return evaluated{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	ev := evaluated{old: old, next: next, summary: summary, changed: !bytes.Equal(old, next)}
	if ev.changed && e.Normalize != nil {
		// Reason: an unparseable file can't be compared semantically, so leave it
		// marked as changed and let Compute's own error surface on apply.
		if norm, nerr := e.Normalize(old); nerr == nil && bytes.Equal(norm, next) {
			ev.changed = false
		}
	}
	return ev, nil
}

// Plan is an ordered set of edits for a target harness.
type Plan struct {
	Target string
	Edits  []Edit
}

// BuildPlan computes the plan for an adapter.
func BuildPlan(a Adapter, bins Bins, home string) Plan {
	return Plan{Target: a.Name(), Edits: a.Edits(bins, home)}
}

// AdapterFor returns the harness adapter for a target name.
func AdapterFor(target string) (Adapter, error) {
	switch target {
	case "claude-code":
		return ClaudeCode{}, nil
	case "codex":
		return Codex{}, nil
	case "gemini":
		return Gemini{}, nil
	default:
		return nil, fmt.Errorf("unknown target %q (supported: claude-code, codex, gemini)", target)
	}
}

// AllAdapters returns every supported harness adapter (for --target all).
func AllAdapters() []Adapter {
	return []Adapter{ClaudeCode{}, Codex{}, Gemini{}}
}

// Pending counts the edits that would change a file, writing nothing. It is how
// `orch status` reports harness-wiring drift: 0 means this harness is fully wired
// to the current binaries, anything higher means `orch install --apply` has work.
func (p Plan) Pending() (int, error) {
	pending := 0
	for _, e := range p.Edits {
		ev, err := e.evaluate()
		if err != nil {
			return 0, err
		}
		if ev.changed {
			pending++
		}
	}
	return pending, nil
}

// Print renders the plan without writing anything (the default, safe mode).
// Edits whose target already equals what's on disk are reported as UNCHANGED, so
// re-running against an already-installed harness shows a clean no-op.
func (p Plan) Print(w io.Writer) error {
	fmt.Fprintf(w, "DRY RUN — target %q. No files will be written.\n", p.Target)
	changed := 0
	for _, e := range p.Edits {
		ev, err := e.evaluate()
		if err != nil {
			return err
		}
		if !ev.changed {
			fmt.Fprintf(w, "\n=== UNCHANGED %s ===\n(already installed)\n", e.Path)
			continue
		}
		changed++
		action := "MODIFY"
		if len(ev.old) == 0 {
			action = "CREATE"
		}
		fmt.Fprintf(w, "\n=== %s %s ===\n%s\n", action, e.Path, ev.summary)
	}
	if changed == 0 {
		fmt.Fprintf(w, "\nAlready fully installed for %q — nothing to do.\n", p.Target)
	} else {
		fmt.Fprintf(w, "\n%d file(s) would change. Re-run with --apply to write (originals backed up first).\n", changed)
	}
	return nil
}

// Apply performs only the edits that actually change a file, backing up each
// modified file first. Idempotent: an already-installed harness writes nothing.
func (p Plan) Apply(w io.Writer, backupDir string) error {
	stamp := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(backupDir, stamp)
	changed := 0
	for _, e := range p.Edits {
		ev, err := e.evaluate()
		if err != nil {
			return err
		}
		if !ev.changed {
			fmt.Fprintf(w, "unchanged %s\n", e.Path)
			continue
		}
		changed++
		if len(ev.old) > 0 {
			if err := backup(dest, e.Path, ev.old); err != nil {
				return fmt.Errorf("backup %s: %w", e.Path, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(e.Path, ev.next, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(w, "wrote %s — %s\n", e.Path, firstLine(ev.summary))
	}
	if changed == 0 {
		fmt.Fprintf(w, "already fully installed for %q — nothing changed.\n", p.Target)
	} else {
		fmt.Fprintf(w, "%d file(s) changed; backups in %s\n", changed, dest)
	}
	return nil
}

func backup(destRoot, srcPath string, content []byte) error {
	// Mirror the source path under destRoot to avoid basename collisions.
	rel := filepath.Join(destRoot, filepath.Base(filepath.Dir(srcPath))+"__"+filepath.Base(srcPath))
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	return os.WriteFile(rel, content, 0o644)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

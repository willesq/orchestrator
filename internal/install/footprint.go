package install

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Footprint is the instruction text a harness loads into every session: its
// entry file and everything that file pulls in.
type Footprint struct {
	Harness string
	Files   []string // in load order, each counted once
	Bytes   int
}

// Tokens estimates the footprint in model tokens, at about four bytes each.
func (f Footprint) Tokens() int { return (f.Bytes + 3) / 4 }

// maxImportDepth matches how many hops Claude Code follows through @imports.
const maxImportDepth = 5

// importRef matches an @import of a Markdown file: "@path.md" at the start of a
// line or after whitespace, where the path may contain backslash-escaped spaces.
var importRef = regexp.MustCompile(`(?m)(?:^|\s)@((?:[^\s\\]|\\ )+\.md)\b`)

// InstructionFootprints measures each harness whose entry file exists under
// home. Claude Code and Gemini expand @imports; Codex does not, but its entry
// tells it to read the shared policy, so that file is counted for it too.
func InstructionFootprints(home string) []Footprint {
	shared := filepath.Join(home, ".config", "orch", "AGENTS.md")
	var out []Footprint
	for _, h := range []struct {
		name, entry string
		imports     bool
	}{
		{"claude-code", filepath.Join(home, ".claude", "CLAUDE.md"), true},
		{"codex", codexEntry(home), false},
		{"gemini", filepath.Join(home, ".gemini", "GEMINI.md"), true},
	} {
		if _, err := os.Stat(h.entry); err != nil {
			continue
		}
		f := Footprint{Harness: h.name}
		seen := map[string]bool{}
		if h.imports {
			f.load(h.entry, home, 0, seen)
		} else {
			f.load(h.entry, home, maxImportDepth, seen) // the entry alone
			if body, err := os.ReadFile(h.entry); err == nil && strings.Contains(string(body), shared) {
				f.load(shared, home, maxImportDepth, seen)
			}
		}
		out = append(out, f)
	}
	return out
}

// codexEntry is the file Codex actually loads: a nonempty override wins.
func codexEntry(home string) string {
	override := filepath.Join(home, ".codex", "AGENTS.override.md")
	if info, err := os.Stat(override); err == nil && info.Size() > 0 {
		return override
	}
	return filepath.Join(home, ".codex", "AGENTS.md")
}

// load counts path and, below the depth limit, the files it imports.
func (f *Footprint) load(path, home string, depth int, seen map[string]bool) {
	if seen[path] {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	seen[path] = true
	f.Files = append(f.Files, path)
	f.Bytes += len(body)
	if depth >= maxImportDepth {
		return
	}
	for _, m := range importRef.FindAllStringSubmatch(string(body), -1) {
		f.load(resolveImport(m[1], path, home), home, depth+1, seen)
	}
}

// resolveImport turns an @import into a path: ~ is the home, and a relative
// path is relative to the importing file.
func resolveImport(ref, from, home string) string {
	ref = strings.ReplaceAll(ref, `\ `, " ")
	switch {
	case strings.HasPrefix(ref, "~/"):
		return filepath.Join(home, ref[2:])
	case filepath.IsAbs(ref):
		return filepath.Clean(ref)
	default:
		return filepath.Join(filepath.Dir(from), ref)
	}
}

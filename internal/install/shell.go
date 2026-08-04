package install

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	aliasBegin = "# >>> orch alias (managed by orch install) >>>"
	aliasEnd   = "# <<< orch alias (managed by orch install) <<<"
)

// aliasEdit inserts or replaces a managed alias block in ~/.zprofile pointing at
// the BUILT orch binary (never `go run`), so the running service is a stable
// compiled artifact even while the code is iterated.
func aliasEdit(orchBin, home string) Edit {
	path := filepath.Join(home, ".zprofile")
	block := aliasBegin + "\nalias orch=\"" + orchBin + "\"\n" + aliasEnd + "\n"
	return Edit{
		Path: path,
		Compute: func(old []byte) ([]byte, string, error) {
			existing := string(old)
			replaced := strings.Contains(existing, aliasBegin)
			next := replaceManagedBlock(existing, block)
			verb := "added"
			if replaced {
				verb = "updated"
			}
			summary := fmt.Sprintf("alias: %s managed block -> alias orch=\"%s\"", verb, orchBin)
			return []byte(next), summary, nil
		},
	}
}

// replaceManagedBlock swaps an existing managed block for the new one, or appends
// it if absent, leaving all other lines untouched.
func replaceManagedBlock(existing, block string) string {
	start := strings.Index(existing, aliasBegin)
	if start == -1 {
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		return existing + block
	}
	end := strings.Index(existing[start:], aliasEnd)
	if end == -1 {
		// Malformed (begin without end) — replace from begin to end of file.
		return existing[:start] + block
	}
	end = start + end + len(aliasEnd)
	// Consume a trailing newline after the end marker if present.
	if end < len(existing) && existing[end] == '\n' {
		end++
	}
	return existing[:start] + block + existing[end:]
}

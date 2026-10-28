package handoff

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitConfig is a git config file's values, keyed section.key or
// section.subsection.key: section and key lowercased, a quoted subsection
// as written (git compares it case-sensitively). A key set twice keeps its
// last value.
type gitConfig map[string]string

// readGitConfig parses <gitDir>/config. It reads files only: include and
// includeIf are not followed, so a value set only through one is not seen.
func readGitConfig(gitDir string) gitConfig {
	cfg := gitConfig{}
	b, err := readSmall(filepath.Join(gitDir, "config"), maxGitConfig)
	if err != nil {
		return cfg
	}
	section := ""
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			end := headerEnd(line)
			if end < 0 {
				section = ""
				continue
			}
			section = parseHeader(line[1:end])
			// A key may follow the header on the same line.
			line = strings.TrimSpace(line[end+1:])
		}
		if line == "" || line[0] == '#' || line[0] == ';' || section == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || strings.ContainsAny(k, " \t\"[].") {
			continue
		}
		if !ok {
			cfg[section+"."+k] = "true" // a bare key is a true boolean
			continue
		}
		cfg[section+"."+k] = configScalar(v)
	}
	return cfg
}

// headerEnd is the index of the ']' closing a section header, outside quotes.
func headerEnd(line string) int {
	quoted := false
	for i := 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '"':
			quoted = !quoted
		case ']':
			if !quoted {
				return i
			}
		}
	}
	return -1
}

// parseHeader turns `remote "origin"` (or the old `remote.origin`) into
// remote.origin.
func parseHeader(h string) string {
	h = strings.TrimSpace(h)
	if name, sub, ok := strings.Cut(h, `"`); ok {
		sub = strings.TrimSuffix(sub, `"`)
		sub = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(sub)
		return strings.ToLower(strings.TrimSpace(name)) + "." + sub
	}
	return strings.ToLower(h)
}

// configValue reads one key from a git directory's config (no subsection).
func configValue(gitDir, section, key string) string {
	return readGitConfig(gitDir)[strings.ToLower(section)+"."+strings.ToLower(key)]
}

// originURL reads remote.origin.url from a git directory's config.
func originURL(gitDir string) string {
	return readGitConfig(gitDir)["remote.origin.url"]
}

// configScalar reads a git config value: an inline comment (# or ;) outside
// quotes ends it, and surrounding quotes are dropped.
func configScalar(v string) string {
	quoted := false
	for i, r := range v {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && (r == '#' || r == ';'):
			return strings.Trim(strings.TrimSpace(v[:i]), `"`)
		}
	}
	return strings.Trim(strings.TrimSpace(v), `"`)
}

// upstreamWhy says why branch b, named like the default branch, is not taken
// as the origin's default branch, or "" when it is. A repository without an
// origin is local and its own default. With one, b must not track another
// branch, and must be at origin/b as last fetched: the tracking config alone
// survives `git checkout -B main FETCH_HEAD` or a reset onto a PR, so only
// the commit shows the files are origin's. A default branch behind or ahead
// of origin/b (not yet pulled or pushed) is skipped until it is level again;
// the refresh leaves its docs as last indexed meanwhile.
func upstreamWhy(common, b string) string {
	cfg := readGitConfig(common)
	if cfg["remote.origin.url"] == "" {
		return ""
	}
	remote, merge := cfg["branch."+b+".remote"], cfg["branch."+b+".merge"]
	if (remote != "" || merge != "") && (remote != "origin" || merge != "refs/heads/"+b) {
		return fmt.Sprintf("on branch %s, which tracks %s %s, not origin's %s", b, remote, strings.TrimPrefix(merge, "refs/heads/"), b)
	}
	at := refOID(common, "refs/remotes/origin/"+b)
	if at == "" {
		return fmt.Sprintf("on branch %s, but origin/%s is not fetched, so nothing shows it is origin's default (run: git fetch origin)", b, b)
	}
	if refOID(common, "refs/heads/"+b) != at {
		return fmt.Sprintf("on branch %s, which is not at origin/%s as last fetched (pull or push to index it)", b, b)
	}
	return ""
}

// refOID is the commit a ref names, loose or packed, "" when unknown.
func refOID(common, ref string) string {
	if b, err := readSmall(filepath.Join(common, filepath.FromSlash(ref)), maxGitPointer); err == nil {
		if oid := strings.TrimSpace(string(b)); isOID(oid) {
			return oid
		}
		return ""
	} else if !os.IsNotExist(err) {
		return ""
	}
	b, err := readSmall(filepath.Join(common, "packed-refs"), maxGitConfig)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == ref && isOID(f[0]) {
			return f[0]
		}
	}
	return ""
}

// isOID reports whether s is a full SHA-1 or SHA-256 object id in hex.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

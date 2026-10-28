package sources

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/handoff"
)

// CanonicalFunc names the repo:// URIs a file would be indexed under by the
// docs or sources sync, best first; none when it is in no known repo.
type CanonicalFunc func(ctx context.Context, path string) []string

// Canonical builds the mapping from the sources registry and the file's own
// git checkout; roots are sources.known_roots (a leading ~ is expanded).
// A file maps to repo://<owner>/<repo>/<path> when it sits in a registered
// repo source, under a registered owner source's directory, or in a git
// checkout (a linked worktree included) whose origin is a GitHub repo, which
// is how the docs sync names that repo's files. A checkout that is ~/.claude
// itself maps to nothing: that is the user's live configuration, not a copy
// of a repo, even when it is kept in git.
func Canonical(store *sql.DB, roots []string) CanonicalFunc {
	home, _ := os.UserHomeDir()
	claudeDir := filepath.Join(home, ".claude")
	roots = append([]string(nil), roots...)
	for i, r := range roots {
		if home != "" && (r == "~" || strings.HasPrefix(r, "~/")) {
			roots[i] = filepath.Join(home, strings.TrimPrefix(r, "~"))
		}
	}
	var reg *Registry
	if store != nil {
		reg = NewRegistry(store)
	}
	return func(ctx context.Context, path string) []string {
		var out []string
		add := func(owner, repo, rel string) {
			if repo != "" && rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
				out = append(out, "repo://"+ownerLabel(owner)+"/"+repo+"/"+filepath.ToSlash(rel))
			}
		}
		if reg != nil {
			if srcs, err := reg.List(ctx); err == nil {
				for _, s := range srcs {
					switch s.Kind {
					case KindRepo:
						if within(path, s.URI) {
							rel, _ := filepath.Rel(filepath.Clean(s.URI), path)
							add(s.Owner, filepath.Base(filepath.Clean(s.URI)), rel)
						}
					case KindOwner:
						for _, r := range roots {
							base := filepath.Join(r, s.URI)
							if !within(path, base) {
								continue
							}
							rest, _ := filepath.Rel(base, path)
							if repo, rel, ok := strings.Cut(filepath.ToSlash(rest), "/"); ok {
								add(s.URI, repo, rel)
							}
						}
					}
				}
			}
		}
		co, ok := handoff.FindCheckout(path)
		if !ok || co.Owner == "" || (home != "" && filepath.Clean(co.Root) == claudeDir) {
			return out
		}
		if rel, err := filepath.Rel(co.Root, path); err == nil {
			add(co.Owner, co.Repo, rel)
		}
		return out
	}
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

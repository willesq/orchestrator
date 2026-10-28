package sources

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/handoff"
)

// CanonicalFunc names the URIs a file would be indexed under by the docs or
// sources sync; none when no registered source covers it.
type CanonicalFunc func(ctx context.Context, path string) []string

// Canonical builds the mapping from the sources and docs registries; roots
// are sources.known_roots (a leading ~ is expanded). The path is cleaned
// first, so it names the file it reaches, and a file maps:
//
//   - to its checkout:// URI when it sits in a registered repo source, or in
//     a repo directory under a registered owner source;
//   - to repo://<owner>/<repo>/<path> when it sits in a git checkout (a
//     linked worktree included) whose origin names a repo the docs sync
//     tracks. The origin comes from the checkout's own config, so it is
//     believed only for a repo already registered; the caller must still find
//     that copy stored by the docs sync, with the same content, before
//     treating the file as a duplicate.
//
// A checkout that is ~/.claude itself maps to nothing: that is the user's
// live configuration, not a copy of a repo, even when it is kept in git.
func Canonical(store *sql.DB, roots []string) CanonicalFunc {
	home, _ := os.UserHomeDir()
	claudeDirs := map[string]bool{}
	if home != "" {
		claudeDir := filepath.Join(home, ".claude")
		claudeDirs[claudeDir] = true
		// Paths reach here resolved, so ~/.claude is matched resolved too.
		if real, err := filepath.EvalSymlinks(claudeDir); err == nil {
			claudeDirs[real] = true
		}
	}
	roots = append([]string(nil), roots...)
	for i, r := range roots {
		if home != "" && (r == "~" || strings.HasPrefix(r, "~/")) {
			roots[i] = filepath.Join(home, strings.TrimPrefix(r, "~"))
		}
		roots[i] = filepath.Clean(roots[i])
	}
	var reg *Registry
	var dreg *docs.Registry
	if store != nil {
		reg, dreg = NewRegistry(store), docs.NewRegistry(store)
	}
	return func(ctx context.Context, path string) []string {
		if !filepath.IsAbs(path) {
			return nil
		}
		path = filepath.Clean(path)
		var out []string
		if reg != nil {
			if srcs, err := reg.List(ctx); err == nil {
				for _, s := range srcs {
					if s.Kind == KindRepo {
						if dir := resolve(s.URI); within(path, dir) && path != dir {
							out = append(out, CheckoutURI(path))
						}
					}
					if s.Kind == KindOwner {
						for _, r := range roots {
							if repo, ok := under(path, resolve(filepath.Join(r, s.URI))); ok && repo != "" {
								out = append(out, CheckoutURI(path))
								break
							}
						}
					}
				}
			}
		}
		if dreg == nil {
			return out
		}
		co, ok := handoff.FindCheckout(path)
		if !ok || co.Owner == "" || claudeDirs[filepath.Clean(co.Root)] {
			return out
		}
		rel, err := filepath.Rel(co.Root, path)
		if err != nil || rel == "." || hasDotSegment(rel) {
			return out
		}
		if owner, repo, ok := trackedRepo(ctx, dreg, co.Owner, co.Repo); ok {
			out = append(out, "repo://"+owner+"/"+repo+"/"+filepath.ToSlash(rel))
		}
		return out
	}
}

// trackedRepo finds a repo the docs sync tracks, by name (GitHub names are
// case-insensitive), returning the registry's own spelling.
func trackedRepo(ctx context.Context, dreg *docs.Registry, owner, repo string) (string, string, bool) {
	owners, err := dreg.ListOwners(ctx)
	if err != nil {
		return "", "", false
	}
	for _, o := range owners {
		if !strings.EqualFold(o.Owner, owner) {
			continue
		}
		repos, err := dreg.ListRepos(ctx, o.Owner)
		if err != nil {
			return "", "", false
		}
		for _, r := range repos {
			if strings.EqualFold(r.Repo, repo) {
				return o.Owner, r.Repo, true
			}
		}
	}
	return "", "", false
}

// under reports whether path is inside base, as base/<repo>/<more>, and
// returns repo.
func under(path, base string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(base), path)
	if err != nil || hasDotSegment(rel) {
		return "", false
	}
	repo, rest, ok := strings.Cut(filepath.ToSlash(rel), "/")
	return repo, ok && rest != ""
}

// hasDotSegment reports whether a relative path climbs out (or is "." itself).
func hasDotSegment(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." || seg == "." || seg == "" {
			return true
		}
	}
	return false
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// resolve is dir with symlinks resolved, or dir cleaned when it cannot be:
// the sources refresh stores files under their resolved paths.
func resolve(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return filepath.Clean(dir)
}

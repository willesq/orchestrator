package install

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Querier reads orch.db: an *sql.DB the caller opened read-only.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// maxPolicyRevisions bounds the saved spaces policies whose roots are read.
const maxPolicyRevisions = 500

// RecordedGitHookPlaces lists the repositories and folders orch has a record
// of, read without orchd: repositories registered with `orch repo`, the
// known roots, trusted test repositories and the docs clone cache (from
// orch.db's settings, or their defaults when q is nil), every root a saved
// spaces policy named, and the checkouts orch's spaces sync wrote into. A
// leading ~ is the home; relative paths are dropped.
func RecordedGitHookPlaces(ctx context.Context, q Querier, home, dataDir string) GitHookPlaces {
	var p GitHookPlaces
	v := config.Defaults()
	if q != nil {
		if stored, err := config.LoadStore(ctx, q); err == nil {
			v = stored
		}
		// 'repo' is sources.KindRepo: a path `orch repo add` registered.
		p.Repos = append(p.Repos, queryStrings(ctx, q, `SELECT uri FROM sources WHERE kind = 'repo'`)...)
		for _, doc := range queryStrings(ctx, q, `SELECT doc FROM spaces_revisions ORDER BY revision DESC LIMIT ?`, maxPolicyRevisions) {
			var pol spaces.Policy
			if json.Unmarshal([]byte(doc), &pol) != nil {
				continue
			}
			for _, s := range pol.Spaces {
				p.Roots = append(p.Roots, s.Roots...)
			}
		}
	}
	p.Roots = append(p.Roots, v.GetStringSlice("sources.known_roots")...)
	cache := v.GetString("docs.repo_cache")
	if cache == "" {
		cache = filepath.Join(dataDir, "repos")
	}
	p.Roots = append(p.Roots, cache)
	p.Repos = append(p.Repos, v.GetStringSlice("queue.worker.verify.repos")...)
	p.Repos = append(p.Repos, recordedCheckoutPaths(spacesettings.Layout{Dir: filepath.Join(dataDir, "spaces")}.StateFile())...)
	p.Repos = absPaths(p.Repos, home)
	p.Roots = absPaths(p.Roots, home)
	return p
}

// queryStrings returns one string column of a query; nothing when the
// table is missing (an older orch.db) or the query fails.
func queryStrings(ctx context.Context, q Querier, query string, args ...any) []string {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// recordedCheckoutPaths lists the checkouts orch's spaces sync recorded.
func recordedCheckoutPaths(stateFile string) []string {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return nil
	}
	var st struct {
		Checkouts map[string]json.RawMessage `json:"checkouts"`
	}
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	out := make([]string, 0, len(st.Checkouts))
	for c := range st.Checkouts {
		out = append(out, c)
	}
	return out
}

// absPaths expands a leading ~ against home, drops relative paths and
// repeats, and keeps the order.
func absPaths(in []string, home string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		if !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

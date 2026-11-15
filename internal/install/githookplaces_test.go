package install

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite" // the pure-Go driver orch.db uses
)

// placesDB is a temporary orch.db holding the tables the search reads.
func placesDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, stmt := range []string{
		`CREATE TABLE sources (id TEXT PRIMARY KEY, kind TEXT NOT NULL, uri TEXT NOT NULL)`,
		`CREATE TABLE config_values (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL, updated_by TEXT NOT NULL)`,
		`CREATE TABLE spaces_revisions (revision INTEGER PRIMARY KEY AUTOINCREMENT, doc TEXT NOT NULL)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func TestRecordedGitHookPlaces(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "orch")
	ctx := context.Background()

	// With no orch.db: the default known root and clone cache only.
	p := RecordedGitHookPlaces(ctx, nil, home, data)
	if !slices.Equal(p.Roots, []string{filepath.Join(home, "code"), filepath.Join(data, "repos")}) || len(p.Repos) != 0 {
		t.Errorf("defaults = %+v", p)
	}

	conn := placesDB(t)
	for _, stmt := range []string{
		`INSERT INTO sources VALUES ('repo|/w/registered', 'repo', '/w/registered'), ('web|https://x', 'web', 'https://x'),
			('repo|relative', 'repo', 'relative')`,
		`INSERT INTO config_values VALUES ('sources.known_roots', '["~/src", "/w/roots"]', 't', 'x'),
			('queue.worker.verify.repos', '["/w/trusted"]', 't', 'x'), ('docs.repo_cache', '"/w/cache"', 't', 'x')`,
		`INSERT INTO spaces_revisions (doc) VALUES ('{"spaces":[{"name":"a","kind":"private","roots":["/w/space-a"]}]}'),
			('not json'), ('{"spaces":[{"name":"b","kind":"private","roots":["/w/space-b", "/w/roots"]}]}')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(data, "spaces", ".orch", "checkouts.json"), `{"checkouts":{"/w/checkout":{}}}`)
	p = RecordedGitHookPlaces(ctx, conn, home, data)
	wantRepos := []string{"/w/registered", "/w/trusted", "/w/checkout"}
	wantRoots := []string{"/w/space-b", "/w/roots", "/w/space-a", filepath.Join(home, "src"), "/w/cache"}
	if !slices.Equal(p.Repos, wantRepos) {
		t.Errorf("repos = %v, want %v", p.Repos, wantRepos)
	}
	if !slices.Equal(p.Roots, wantRoots) {
		t.Errorf("roots = %v, want %v", p.Roots, wantRoots)
	}

	// An older orch.db without these tables reads as the defaults.
	empty, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if p := RecordedGitHookPlaces(ctx, empty, home, data); len(p.Roots) != 2 || len(p.Repos) != 1 {
		t.Errorf("older orch.db = %+v", p)
	}
	if got := recordedCheckoutPaths(filepath.Join(home, "missing")); got != nil {
		t.Errorf("missing record = %v", got)
	}
	writeFile(t, filepath.Join(home, "bad.json"), "{")
	if got := recordedCheckoutPaths(filepath.Join(home, "bad.json")); got != nil {
		t.Errorf("bad record = %v", got)
	}
}

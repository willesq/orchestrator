package db

import (
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestDocsLegacyErrorsMigration(t *testing.T) {
	const legacy = "failed before failure reasons were recorded; the next sync retries it"
	conn, err := openAt(filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Step back to before 0013, seed rows as an older orchd left them, re-apply.
	if err := goose.DownTo(conn, "migrations", 12); err != nil {
		t.Fatalf("DownTo 12: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO docs_owners (owner, status, error) VALUES ('blank', 'error', ''), ('kept', 'error', 'clone failed (network)'), ('fine', 'ok', '')`,
		`INSERT INTO docs_repos (owner, repo, status, error) VALUES ('o', 'blank', 'error', ''), ('o', 'kept', 'error', 'clone failed (stalled)'), ('o', 'fine', 'ok', '')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tests := []struct {
		table, keyCol, key, want string
	}{
		{"docs_repos", "repo", "blank", legacy},
		{"docs_repos", "repo", "kept", "clone failed (stalled)"},
		{"docs_repos", "repo", "fine", ""},
		{"docs_owners", "owner", "blank", legacy},
		{"docs_owners", "owner", "kept", "clone failed (network)"},
		{"docs_owners", "owner", "fine", ""},
	}
	for _, tt := range tests {
		t.Run(tt.table+"/"+tt.key, func(t *testing.T) {
			var got string
			if err := conn.QueryRow("SELECT error FROM "+tt.table+" WHERE "+tt.keyCol+" = ?", tt.key).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}

	if err := goose.DownTo(conn, "migrations", 12); err != nil {
		t.Fatalf("down: %v", err)
	}
	var got string
	if err := conn.QueryRow("SELECT error FROM docs_repos WHERE repo = 'blank'").Scan(&got); err != nil || got != "" {
		t.Errorf("after down: error = %q (%v), want empty", got, err)
	}
}

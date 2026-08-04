package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpen(t *testing.T) {
	tests := []struct {
		name      string
		wantTable string
	}{
		{name: "opens db and applies migrations", wantTable: "meta"},
		{name: "goose tracking table present", wantTable: "goose_db_version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			conn, err := Open()
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = conn.Close() }()

			var name string
			err = conn.QueryRow(
				"SELECT name FROM sqlite_master WHERE type='table' AND name=?",
				tt.wantTable,
			).Scan(&name)
			if err != nil {
				t.Fatalf("query for %q: %v", tt.wantTable, err)
			}
			if name != tt.wantTable {
				t.Errorf("table = %q, want %q", name, tt.wantTable)
			}
		})
	}
}

func TestOpenAtErrors(t *testing.T) {
	tests := []struct {
		name    string
		dbPath  string
		wantErr bool
	}{
		{name: "valid temp path succeeds", dbPath: filepath.Join(t.TempDir(), "ok.db"), wantErr: false},
		{name: "unwritable path fails to ping", dbPath: "/nonexistent-dir-xyz/orch.db", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := openAt(tt.dbPath)
			if conn != nil {
				defer func() { _ = conn.Close() }()
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("openAt(%q) err = %v, wantErr %v", tt.dbPath, err, tt.wantErr)
			}
		})
	}
}

func TestOpenAtMigrateError(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "corrupt goose_db_version table fails migration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrupt.db")
			conn, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			// A goose_db_version missing the columns goose expects makes goose.Up error.
			if _, err := conn.Exec("CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY)"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_ = conn.Close()

			if _, err := openAt(path); err == nil {
				t.Errorf("expected migrate error on corrupt goose table")
			}
		})
	}
}

func TestOpenEnsureError(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "data dir under a file cannot be created"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a regular file, then point the data dir beneath it so
			// MkdirAll (inside paths.Ensure) fails.
			f := filepath.Join(t.TempDir(), "afile")
			if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			t.Setenv("ORCH_DATA_DIR", filepath.Join(f, "nested"))
			t.Setenv("ORCH_CONFIG_DIR", filepath.Join(f, "nested-cfg"))
			if _, err := Open(); err == nil {
				t.Errorf("expected Open to fail when data dir is unbuildable")
			}
		})
	}
}

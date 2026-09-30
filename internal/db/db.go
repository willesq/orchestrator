// Package db opens the orch.db SQLite store (pure-Go, no cgo) and applies the
// embedded migrations. This store backs settings-adjacent registries (sources,
// projects, source types) in later phases; go-workflows uses its own workflows.db.
package db

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go)

	"github.com/willesq/orchestrator/internal/paths"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens orch.db with WAL + busy_timeout and applies pending migrations.
func Open() (*sql.DB, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	return openAt(paths.DBFile())
}

// openAt opens (and migrates) a SQLite database at an explicit path. Split out
// from Open so error paths are testable without the paths package.
func openAt(dbPath string) (*sql.DB, error) {
	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := migrate(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return conn, nil
}

func migrate(conn *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(conn, "migrations")
}

// OpenReader opens an existing orch.db for reading without applying migrations.
// Read-only callers (notably `orch status`) must not migrate a store they do not
// own, and must not print goose's migration chatter into a report.
func OpenReader() (*sql.DB, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	dsn := "file:" + paths.DBFile() +
		"?mode=ro&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

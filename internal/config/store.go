package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/spf13/viper"
)

// querier is what reading the store needs: an *sql.DB, or an *sql.Tx so a
// write can plan against the same snapshot it changes.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// LoadStore resolves the config from the compiled defaults and the values
// stored in orch.db's config_values. A row that names no field, or no longer
// passes its field's checks, is skipped and logged by key, never by value: a
// value can be a path or a permission rule.
func LoadStore(ctx context.Context, q querier) (*viper.Viper, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM config_values`)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer func() { _ = rows.Close() }()
	v := Defaults()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		f, ok := FieldFor(key)
		if !ok {
			log.Printf("config: ignoring stored setting %q: no such setting", key)
			continue
		}
		val, err := ParseJSON(f, raw)
		if err != nil {
			log.Printf("config: ignoring stored setting %q: invalid value", key)
			continue
		}
		v.Set(key, val)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return v, nil
}

// ApplyStore validates values as Apply does and stores them in one
// transaction: all of them or none. A value equal to its default deletes the
// key's row instead, so the key follows the default from then on. by records
// who made the change ("app", "orchd:migrate", ...).
func ApplyStore(ctx context.Context, db *sql.DB, values map[string]any, confirmSensitive bool, by string) error {
	parsed := make(map[string]any, len(values))
	for key, val := range values {
		f, ok := FieldFor(key)
		if !ok || f.Hidden {
			return fmt.Errorf("unknown setting %q", key)
		}
		// Parsed values carry the Go types planWrites' queue checks expect,
		// whatever type the caller passed.
		enc, err := encodeValue(f, val)
		if err != nil {
			return err
		}
		if parsed[key], err = ParseJSON(f, enc); err != nil {
			return err
		}
	}
	values = parsed
	tx, err := beginWrite(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	v, err := LoadStore(ctx, tx)
	if err != nil {
		return err
	}
	writes, err := planWrites(v, values, confirmSensitive)
	if err != nil {
		return err
	}
	if err := storeValues(ctx, tx, writes, by); err != nil {
		return err
	}
	return tx.Commit()
}

// beginWrite opens a transaction that holds the write lock from its first
// statement. A transaction that reads first and writes later cannot wait for
// another writer: SQLite fails its upgrade at once (SQLITE_BUSY, or
// SQLITE_BUSY_SNAPSHOT in WAL mode) instead of honouring busy_timeout. Taking
// the lock with a write that changes nothing makes a second writer wait.
func beginWrite(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM config_values WHERE 0`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// ResetStore puts keys back to their defaults, with the same checks as
// ApplyStore: a sensitive key needs confirmSensitive, since its default can
// widen what agents may run.
func ResetStore(ctx context.Context, db *sql.DB, keys []string, confirmSensitive bool, by string) error {
	defaults := Defaults()
	values := make(map[string]any, len(keys))
	for _, key := range keys {
		f, ok := FieldFor(key)
		if !ok {
			return fmt.Errorf("unknown setting %q", key)
		}
		if f.KeepDefault {
			return fmt.Errorf("%s has no default to reset to; %s", key, keepDefaultHint(key))
		}
		val, err := canonicalValue(defaults, f)
		if err != nil {
			return err
		}
		values[key] = val
	}
	return ApplyStore(ctx, db, values, confirmSensitive, by)
}

// ResetAllStore puts every setting back to its default and returns the keys
// it reset, sorted, so the caller can say what changed.
func ResetAllStore(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `DELETE FROM config_values RETURNING key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, rows.Err()
}

// storeValues upserts each value, or deletes its row when it equals the
// default. Every key must name a field.
func storeValues(ctx context.Context, tx *sql.Tx, values map[string]any, by string) error {
	defaults := Defaults()
	now := time.Now().UTC().Format(time.RFC3339)
	for key, val := range values {
		f, ok := FieldFor(key)
		if !ok {
			return fmt.Errorf("unknown setting %q", key)
		}
		enc, err := encodeValue(f, val)
		if err != nil {
			return err
		}
		def, err := canonicalValue(defaults, f)
		if err != nil {
			return err
		}
		defEnc, err := encodeValue(f, def)
		if err != nil {
			return err
		}
		if enc == defEnc && !f.KeepDefault {
			if _, err := tx.ExecContext(ctx, `DELETE FROM config_values WHERE key = ?`, key); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO config_values (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value,
				updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
			key, enc, now, by); err != nil {
			return err
		}
	}
	return nil
}

// encodeValue is val's stored JSON. It goes through ParseJSON so equal values
// encode identically whatever Go type they arrived as (an int or a float,
// []string or []any), which is what lets a default delete its row.
func encodeValue(f Field, val any) (string, error) {
	b, err := json.Marshal(val)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.Key, err)
	}
	parsed, err := ParseJSON(f, string(b))
	if err != nil {
		return "", err
	}
	b, err = json.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.Key, err)
	}
	return string(b), nil
}

// canonicalValue is f's value in v as stored, without the derived view
// FieldValue gives the queue's fields: the raw max_concurrent, not 0 when the
// worker is off.
func canonicalValue(v *viper.Viper, f Field) (any, error) {
	var raw any
	switch f.Type {
	case TypeList:
		raw = listValue(v.Get(f.Key))
	case TypeMap:
		raw = mapValue(v, f.Key)
	default:
		raw = v.Get(f.Key)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Key, err)
	}
	return ParseJSON(f, string(b))
}

// keepDefaultHint says how a KeepDefault setting is changed instead.
func keepDefaultHint(key string) string {
	if key == KeySpacesEnabled {
		return "turn memory spaces on or off with 'orch spaces on|off' or in Orch.app"
	}
	return "change it in Orch.app"
}

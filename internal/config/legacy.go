package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// ImportReport says what ImportLegacy did, by key only.
type ImportReport struct {
	// Ran is true when this call imported the file; false when there was no
	// file or an earlier import already ran.
	Ran bool
	// Imported lists the keys stored because they differ from the default.
	Imported []string
	// Invalid lists keys whose value no longer passes the field's checks;
	// they keep their default.
	Invalid []string
	// Unknown lists keys in the file that name no setting.
	Unknown []string
}

// ImportLegacy copies a config.yaml's settings into config_values, once. It
// imports every value that differs from today's default exactly as it is, even
// one an older orch wrote only because it saved every key: dropping it would
// silently change what runs, such as turning on a worker that was off. The
// file is left in place, unread from then on.
func ImportLegacy(ctx context.Context, db *sql.DB, path string) (ImportReport, error) {
	var rep ImportReport
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer func() { _ = tx.Rollback() }()
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM config_import`).Scan(&done); err != nil {
		return rep, err
	}
	if done > 0 {
		return rep, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Closing the window on a first start without a file means a
		// config.yaml written later (by an older orch, say) is never imported
		// over settings confirmed in the app.
		if err := recordImport(ctx, tx, path, ""); err != nil {
			return rep, err
		}
		return rep, tx.Commit()
	}
	if err != nil {
		return rep, err
	}

	file := viper.New()
	file.SetConfigType("yaml")
	if err := file.ReadConfig(bytes.NewReader(data)); err != nil {
		return rep, fmt.Errorf("read %s: %w", path, err)
	}
	defaults := Defaults()
	ran := legacyQueue(file, defaults)
	writes := map[string]any{}
	for _, f := range Fields {
		if !file.IsSet(f.Key) {
			continue
		}
		var val any
		switch f.Key {
		case keyMaxActive, keyMaxConcurrent:
			n, ok := ran[f.Key]
			if !ok {
				rep.Invalid = append(rep.Invalid, f.Key)
				continue
			}
			val = n
		default:
			if !textItems(f, file.Get(f.Key)) {
				rep.Invalid = append(rep.Invalid, f.Key)
				continue
			}
			val, err = canonicalValue(file, f)
			if err != nil {
				rep.Invalid = append(rep.Invalid, f.Key)
				continue
			}
		}
		enc, err := encodeValue(f, val)
		if err != nil {
			rep.Invalid = append(rep.Invalid, f.Key)
			continue
		}
		def, err := canonicalValue(defaults, f)
		if err != nil {
			return rep, err
		}
		if defEnc, err := encodeValue(f, def); err != nil || defEnc == enc {
			continue
		}
		writes[f.Key] = val
		rep.Imported = append(rep.Imported, f.Key)
	}
	for _, key := range file.AllKeys() {
		if !coveredKey(key) {
			rep.Unknown = append(rep.Unknown, key)
		}
	}

	if err := storeValues(ctx, tx, writes, "import"); err != nil {
		return rep, err
	}
	sum := sha256.Sum256(data)
	if err := recordImport(ctx, tx, path, hex.EncodeToString(sum[:])); err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	sort.Strings(rep.Imported)
	sort.Strings(rep.Invalid)
	sort.Strings(rep.Unknown)
	rep.Ran = true
	return rep, nil
}

// recordImport marks the import done; sum is empty when there was no file.
func recordImport(ctx context.Context, tx *sql.Tx, path, sum string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO config_import (path, sha256, imported_at) VALUES (?, ?, ?)`,
		path, sum, time.Now().UTC().Format(time.RFC3339))
	return err
}

// textItems reports whether a list field's items, or a map-of-lists field's
// lists' items, are all text. listValue would turn a number or a nested map
// into text, which must not slip into a list such as allowed_tools.
func textItems(f Field, raw any) bool {
	listOK := func(raw any) bool {
		items, ok := raw.([]any)
		if !ok {
			return true // a comma-separated string, or nothing
		}
		for _, it := range items {
			if _, ok := it.(string); !ok {
				return false
			}
		}
		return true
	}
	switch {
	case f.Type == TypeList:
		return listOK(raw)
	case f.Type == TypeMap && f.MapValues == TypeList:
		m, ok := raw.(map[string]any)
		if !ok {
			return true
		}
		for _, v := range m {
			if !listOK(v) {
				return false
			}
		}
	}
	return true
}

// LegacyImported reports whether the one-time import has run, so a
// config.yaml still on disk may be removed without losing anything.
func LegacyImported(ctx context.Context, q querier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT 1 FROM config_import LIMIT 1`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	return rows.Next(), rows.Err()
}

// legacyQueue is the queue's two limits as the old loader ran them, clamped by
// normalizeQueue, so a value it corrected (max_active: 0 meant 50) keeps its
// effect. A limit that is not a whole number is left out, and reported invalid.
func legacyQueue(file, defaults *viper.Viper) map[string]int {
	read := func(key string) (int, bool) {
		if !file.IsSet(key) {
			return defaults.GetInt(key), true
		}
		n, err := strconv.Atoi(fmt.Sprint(file.Get(key)))
		return n, err == nil
	}
	active, activeOK := read(keyMaxActive)
	parallel, parallelOK := read(keyMaxConcurrent)
	q := QueueSettings{MaxActive: active, Worker: QueueWorkerSettings{MaxConcurrent: parallel}}
	normalizeQueue(&q)
	out := map[string]int{}
	if activeOK {
		out[keyMaxActive] = q.MaxActive
	}
	if parallelOK {
		out[keyMaxConcurrent] = q.Worker.MaxConcurrent
	}
	return out
}

// coveredKey reports whether a flattened file key belongs to a field: the
// field itself, or one entry of a map field.
func coveredKey(key string) bool {
	if _, ok := FieldFor(key); ok {
		return true
	}
	for _, f := range Fields {
		if f.Type == TypeMap && strings.HasPrefix(key, f.Key+".") {
			return true
		}
	}
	return false
}

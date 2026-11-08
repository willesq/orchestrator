package config

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
)

func openStore(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// storedRows maps each stored key to its JSON value.
func storedRows(t *testing.T, conn *sql.DB) map[string]string {
	t.Helper()
	rows, err := conn.Query(`SELECT key, value FROM config_values`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

func storedValue(t *testing.T, conn *sql.DB, key string) string {
	t.Helper()
	v, err := LoadStore(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	val, err := FieldValue(v, mustField(t, key))
	if err != nil {
		t.Fatal(err)
	}
	return val
}

func TestEveryDefaultEncodes(t *testing.T) {
	defaults := Defaults()
	for _, f := range Fields {
		val, err := canonicalValue(defaults, f)
		if err != nil {
			t.Errorf("default of %s does not pass its own checks: %v", f.Key, err)
			continue
		}
		if _, err := encodeValue(f, val); err != nil {
			t.Errorf("default of %s does not encode: %v", f.Key, err)
		}
	}
}

func TestApplyStoreRoundTrip(t *testing.T) {
	tests := []struct {
		key, raw string
	}{
		{"hooks.auto_rag.top_k", `7`},
		{"rag.search.keyword_weight", `1.5`},
		{"hooks.auto_rag.format", `"full"`},
		{"docs.default_refresh", `"12h"`},
		{"sources.known_roots", `["/src","/work"]`},
		{"rag.search.source_weights", `{"decision":2,"web":0.5}`},
		{"routing.kinds", `{"code":"fast"}`},
		{"rag.qdrant.mode", `"external"`},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			conn := openStore(t)
			f := mustField(t, tt.key)
			val, err := ParseJSON(f, tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := ApplyStore(context.Background(), conn, map[string]any{tt.key: val}, true, "app"); err != nil {
				t.Fatal(err)
			}
			if _, ok := storedRows(t, conn)[tt.key]; !ok {
				t.Fatalf("no row for %s", tt.key)
			}
			want, err := encodeValue(f, val)
			if err != nil {
				t.Fatal(err)
			}
			got := storedValue(t, conn, tt.key)
			if f.Type == TypeMap {
				// Map entries the user did not set keep their defaults.
				if !strings.Contains(got, strings.Trim(want, "{}")) {
					t.Errorf("%s = %s, want it to contain %s", tt.key, got, want)
				}
				return
			}
			if got != want {
				t.Errorf("%s = %s, want %s", tt.key, got, want)
			}
		})
	}
}

func TestApplyStoreDefaultDeletesRow(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	if err := ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.top_k": 9}, false, "app"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.top_k": 3}, false, "app"); err != nil {
		t.Fatal(err)
	}
	// A default arriving as another Go type is still the default.
	if err := ApplyStore(ctx, conn, map[string]any{"rag.search.keyword_weight": 0.5, "sources.known_roots": []any{"~/code"}}, true, "app"); err != nil {
		t.Fatal(err)
	}
	if rows := storedRows(t, conn); len(rows) != 0 {
		t.Errorf("rows = %v, want none: default values are not stored", rows)
	}
}

func TestApplyStoreRecordsWho(t *testing.T) {
	conn := openStore(t)
	if err := ApplyStore(context.Background(), conn, map[string]any{"hooks.auto_rag.top_k": 9}, false, "orchd:migrate"); err != nil {
		t.Fatal(err)
	}
	var by, at string
	if err := conn.QueryRow(`SELECT updated_by, updated_at FROM config_values WHERE key = 'hooks.auto_rag.top_k'`).Scan(&by, &at); err != nil {
		t.Fatal(err)
	}
	if by != "orchd:migrate" || at == "" {
		t.Errorf("updated_by = %q, updated_at = %q", by, at)
	}
}

func TestApplyStoreRefuses(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]any
		confirm bool
		wantErr error
		wantMsg string
	}{
		{name: "sensitive without confirmation", values: map[string]any{"rag.qdrant.mode": "external"}, wantErr: ErrConfirmSensitive},
		{name: "unknown key", values: map[string]any{"slack.channels": "x"}, wantMsg: "unknown setting"},
		{name: "hidden key", values: map[string]any{keyWorkerEnabled: false}, wantMsg: "unknown setting"},
		{name: "invalid value", values: map[string]any{"queue.worker.permission_mode": "bypassPermissions"}, confirm: true, wantMsg: "must be one of"},
		{name: "queue limit below parallel tasks, all or nothing", values: map[string]any{
			"hooks.auto_rag.top_k": 9, keyMaxConcurrent: 10, keyMaxActive: 5,
		}, wantMsg: "cannot be lower"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := openStore(t)
			err := ApplyStore(context.Background(), conn, tt.values, tt.confirm, "app")
			if err == nil {
				t.Fatal("want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
			if rows := storedRows(t, conn); len(rows) != 0 {
				t.Errorf("rows = %v, want none after a refused change", rows)
			}
		})
	}
}

func TestApplyStoreRaisesQueueLimit(t *testing.T) {
	conn := openStore(t)
	// A float, as a JSON decoder without UseNumber gives, still drives the queue rules.
	if err := ApplyStore(context.Background(), conn, map[string]any{keyMaxConcurrent: float64(30)}, false, "app"); err != nil {
		t.Fatal(err)
	}
	rows := storedRows(t, conn)
	if rows[keyMaxConcurrent] != "30" || rows[keyMaxActive] != "30" {
		t.Errorf("rows = %v, want parallel 30 and the queue limit raised to 30", rows)
	}
	if _, ok := rows[keyWorkerEnabled]; ok {
		t.Errorf("worker enabled stored though it equals its default")
	}
}

func TestResetStore(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	if err := ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.top_k": 9, "rag.qdrant.mode": "external"}, true, "app"); err != nil {
		t.Fatal(err)
	}
	if err := ResetStore(ctx, conn, []string{"rag.qdrant.mode"}, false, "app"); !errors.Is(err, ErrConfirmSensitive) {
		t.Errorf("reset of a sensitive key without confirmation: err = %v", err)
	}
	if err := ResetStore(ctx, conn, []string{"nope"}, true, "app"); err == nil {
		t.Error("reset of an unknown key: want an error")
	}
	if err := ResetStore(ctx, conn, []string{"hooks.auto_rag.top_k", "rag.qdrant.mode"}, true, "app"); err != nil {
		t.Fatal(err)
	}
	if rows := storedRows(t, conn); len(rows) != 0 {
		t.Errorf("rows = %v, want none after reset", rows)
	}
	if got := storedValue(t, conn, "hooks.auto_rag.top_k"); got != "3" {
		t.Errorf("top_k = %s after reset, want the default 3", got)
	}
}

func TestLoadStoreSkipsBadRows(t *testing.T) {
	conn := openStore(t)
	for _, row := range [][2]string{
		{"slack.channels", `["general"]`},  // names no setting
		{"hooks.auto_rag.top_k", `"many"`}, // no longer valid
		{"rag.search.keyword_weight", `2`}, // fine
	} {
		if _, err := conn.Exec(`INSERT INTO config_values (key, value, updated_at, updated_by) VALUES (?, ?, 'now', 'test')`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got := storedValue(t, conn, "hooks.auto_rag.top_k"); got != "3" {
		t.Errorf("top_k = %s, want the default for an invalid row", got)
	}
	if got := storedValue(t, conn, "rag.search.keyword_weight"); got != "2" {
		t.Errorf("keyword_weight = %s, want 2", got)
	}
}

func writeLegacy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// legacyYAML is what an older orch wrote: every key, defaults included.
const legacyYAML = `
hooks:
  auto_rag:
    top_k: 3
    format: full
    min_score: "high"
queue:
  max_active: 0
  worker:
    enabled: false
    max_concurrent: lots
    allowed_tools:
      claude: ["Bash(ls:*)", {nested: map}]
    verify:
      repos: [/src/app]
routing:
  kinds:
    code: fast
    docs: balanced
slack:
  channels: [general]
`

func TestImportLegacy(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	path := writeLegacy(t, legacyYAML)

	rep, err := ImportLegacy(ctx, conn, path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ran {
		t.Fatal("Ran = false on the first import")
	}
	wantImported := []string{"hooks.auto_rag.format", keyMaxActive, keyWorkerEnabled, "queue.worker.verify.repos", "routing.kinds"}
	if !slices.Equal(rep.Imported, wantImported) {
		t.Errorf("Imported = %v, want %v", rep.Imported, wantImported)
	}
	if !slices.Equal(rep.Invalid, []string{"hooks.auto_rag.min_score", "queue.worker.allowed_tools", keyMaxConcurrent}) {
		t.Errorf("Invalid = %v", rep.Invalid)
	}
	if !slices.Equal(rep.Unknown, []string{"slack.channels"}) {
		t.Errorf("Unknown = %v", rep.Unknown)
	}

	rows := storedRows(t, conn)
	// The old loader ran max_active: 0 as the 50-task cap; the import keeps that.
	if rows[keyMaxActive] != "50" {
		t.Errorf("max_active = %s, want 50", rows[keyMaxActive])
	}
	// A worker that was off stays off.
	if rows[keyWorkerEnabled] != "false" {
		t.Errorf("worker enabled = %s, want false", rows[keyWorkerEnabled])
	}
	if _, ok := rows["hooks.auto_rag.top_k"]; ok {
		t.Error("top_k stored though the file held its default")
	}
	var by string
	if err := conn.QueryRow(`SELECT updated_by FROM config_values WHERE key = 'routing.kinds'`).Scan(&by); err != nil || by != "import" {
		t.Errorf("updated_by = %q, %v", by, err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file should be left in place: %v", err)
	}
	if ok, err := LegacyImported(ctx, conn); err != nil || !ok {
		t.Errorf("LegacyImported = %v, %v", ok, err)
	}

	// A change made since the import survives a second start.
	if err := ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.format": "pointers"}, false, "app"); err != nil {
		t.Fatal(err)
	}
	again, err := ImportLegacy(ctx, conn, path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ran {
		t.Error("the import ran twice")
	}
	if _, ok := storedRows(t, conn)["hooks.auto_rag.format"]; ok {
		t.Error("a second import overwrote a change made in the app")
	}
}

func TestImportLegacyNeverAfterAStartWithoutAFile(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := ImportLegacy(ctx, conn, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(legacyYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := ImportLegacy(ctx, conn, path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ran || len(storedRows(t, conn)) != 0 {
		t.Errorf("a file written after the first start was imported: %+v", rep)
	}
}

func TestImportLegacyNothingToDo(t *testing.T) {
	tests := []struct {
		name     string
		path     func(t *testing.T) string
		wantErr  bool
		wantDone bool
	}{
		{name: "no file closes the import", path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "config.yaml") }, wantDone: true},
		{name: "malformed file", path: func(t *testing.T) string { return writeLegacy(t, "hooks: [unclosed") }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := openStore(t)
			ctx := context.Background()
			rep, err := ImportLegacy(ctx, conn, tt.path(t))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if rep.Ran {
				t.Error("Ran = true")
			}
			if ok, err := LegacyImported(ctx, conn); err != nil || ok != tt.wantDone {
				t.Errorf("LegacyImported = %v, %v; want %v", ok, err, tt.wantDone)
			}
		})
	}
}

// Two processes writing settings at once (the app through orchd, a maintenance
// command) must each wait their turn, not fail on the other's commit.
func TestApplyStoreConcurrentWriters(t *testing.T) {
	first := openStore(t)
	second, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	ctx := context.Background()
	errs := make(chan error, 40)
	for i := range 20 {
		for _, conn := range []*sql.DB{first, second} {
			go func() {
				errs <- ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.top_k": 4 + i%10}, false, "test")
			}()
		}
	}
	for range 40 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
}

func TestResetAllStore(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	if err := ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.top_k": 9, "rag.qdrant.mode": "external"}, true, "app"); err != nil {
		t.Fatal(err)
	}
	keys, err := ResetAllStore(ctx, conn)
	if err != nil || !slices.Equal(keys, []string{"hooks.auto_rag.top_k", "rag.qdrant.mode"}) {
		t.Fatalf("reset = %v, %v", keys, err)
	}
	if keys, err := ResetAllStore(ctx, conn); err != nil || len(keys) != 0 {
		t.Errorf("second reset = %v, %v", keys, err)
	}
}

// A closed or unmigrated store fails each write cleanly, writing nothing.
func TestStoreWritesFailOnAnUnusableStore(t *testing.T) {
	conn := openStore(t)
	_ = conn.Close()
	ctx := context.Background()
	if err := ApplyStore(ctx, conn, map[string]any{"hooks.auto_rag.top_k": 9}, false, "app"); err == nil {
		t.Error("ApplyStore on a closed store succeeded")
	}
	if _, err := ResetAllStore(ctx, conn); err == nil {
		t.Error("ResetAllStore on a closed store succeeded")
	}
	if _, err := ImportLegacy(ctx, conn, writeLegacy(t, legacyYAML)); err == nil {
		t.Error("ImportLegacy on a closed store succeeded")
	}

	bare, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bare.Close() })
	if err := ApplyStore(ctx, bare, map[string]any{"hooks.auto_rag.top_k": 9}, false, "app"); err == nil {
		t.Error("ApplyStore without the settings table succeeded")
	}
	if _, err := LoadStore(ctx, bare); err == nil {
		t.Error("LoadStore without the settings table succeeded")
	}
	if _, err := LegacyImported(ctx, bare); err == nil {
		t.Error("LegacyImported without the import table succeeded")
	}
}

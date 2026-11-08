package config

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/willesq/orchestrator/internal/db"
)

// loadTemp points orch at a temporary data directory and loads the config,
// which is the defaults until a test stores something.
func loadTemp(t *testing.T) *viper.Viper {
	t.Helper()
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	v, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// applyTemp stores values in loadTemp's orch.db, as a client's write does.
func applyTemp(t *testing.T, values map[string]any, confirm bool) error {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	return ApplyStore(context.Background(), conn, values, confirm, "test")
}

// reload loads the config loadTemp's orch.db now holds.
func reload(t *testing.T) *viper.Viper {
	t.Helper()
	v, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustField(t *testing.T, key string) Field {
	t.Helper()
	f, ok := FieldFor(key)
	if !ok {
		t.Fatalf("no field %s", key)
	}
	return f
}

// flatten lists a nested settings map's leaf keys, stopping at map fields.
func flatten(prefix string, m map[string]any, out *[]string) {
	for k, val := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if f, ok := FieldFor(key); ok && f.Type == TypeMap {
			*out = append(*out, key)
			continue
		}
		if nested, ok := val.(map[string]any); ok {
			flatten(key, nested, out)
			continue
		}
		*out = append(*out, key)
	}
}

func TestEveryDefaultHasAField(t *testing.T) {
	var keys []string
	flatten("", Defaults().AllSettings(), &keys)
	if len(keys) == 0 {
		t.Fatal("no defaults found")
	}
	for _, k := range keys {
		if _, ok := FieldFor(k); !ok {
			t.Errorf("default %s has no field in Fields, so clients cannot see it", k)
		}
	}
}

func TestFieldsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Fields {
		if seen[f.Key] {
			t.Errorf("%s listed twice", f.Key)
		}
		seen[f.Key] = true
		if f.Label == "" || f.Section == "" {
			t.Errorf("%s needs a label and a section", f.Key)
		}
		if f.Type == TypeEnum && len(f.Options) == 0 {
			t.Errorf("%s is an enum without options", f.Key)
		}
		if f.Type == TypeMap && f.MapValues == "" {
			t.Errorf("%s is a map without MapValues", f.Key)
		}
	}
}

func TestParseJSON(t *testing.T) {
	tests := []struct {
		key, raw string
		want     any
		wantErr  string
	}{
		{key: keyMaxConcurrent, raw: "0", want: 0},
		{key: keyMaxConcurrent, raw: "1", want: 1},
		{key: keyMaxConcurrent, raw: "50", want: 50},
		{key: keyMaxConcurrent, raw: "51", wantErr: "from 0 to 50"},
		{key: keyMaxConcurrent, raw: "-1", wantErr: "from 0 to 50"},
		{key: keyMaxConcurrent, raw: "1.5", wantErr: "whole number"},
		{key: keyMaxConcurrent, raw: `"5"`, wantErr: "whole number"},
		{key: keyMaxConcurrent, raw: "5 6", wantErr: "more than one"},
		{key: keyMaxConcurrent, raw: "99999999999999999999", wantErr: "whole number"},
		{key: keyMaxActive, raw: "0", wantErr: "from 1 to 50"},
		{key: keyMaxActive, raw: "51", wantErr: "from 1 to 50"},
		{key: "pr_babysitter.enabled", raw: "false", want: false},
		{key: "pr_babysitter.enabled", raw: "1", wantErr: "true or false"},
		{key: "hooks.auto_rag.min_score", raw: "0.5", want: 0.5},
		{key: "hooks.auto_rag.min_score", raw: "1.5", wantErr: "from 0 to 1"},
		{key: "hooks.auto_rag.min_score", raw: "true", wantErr: "a number"},
		{key: "queue.worker.timeout", raw: `"5m"`, want: "5m"},
		{key: "queue.worker.timeout", raw: `"0s"`, wantErr: "positive duration"},
		{key: "queue.worker.timeout", raw: `"soon"`, wantErr: "positive duration"},
		{key: "queue.worker.permission_mode", raw: `"plan"`, want: "plan"},
		{key: "queue.worker.permission_mode", raw: `"bypassPermissions"`, wantErr: "must be one of"},
		{key: "queue.worker.codex.sandbox", raw: `"danger-full-access"`, wantErr: "must be one of"},
		{key: "queue.worker.claude_bin", raw: `""`, want: ""},
		{key: "queue.worker.claude_bin", raw: `"/opt/claude"`, want: "/opt/claude"},
		{key: "queue.worker.claude_bin", raw: `"claude"`, wantErr: "absolute path"},
		{key: "rag.qdrant.url", raw: `"https://q.example"`, want: "https://q.example"},
		{key: "rag.qdrant.url", raw: `"file:///etc/passwd"`, wantErr: "http or https"},
		{key: "rag.qdrant.url", raw: `"https://u:p@q.example"`, wantErr: "username or password"},
		{key: "rag.qdrant.version", raw: `"v1.2.3"`, want: "v1.2.3"},
		{key: "rag.qdrant.version", raw: `"../../evil"`, wantErr: "release"},
		{key: "sessions.summarize.model", raw: `"a\nb"`, wantErr: "one line"},
		{key: "sessions.summarize.model", raw: "3", wantErr: "must be text"},
		{key: "sources.known_roots", raw: `["~/code"]`, want: []string{"~/code"}},
		{key: "sources.known_roots", raw: `null`, want: []string{}},
		{key: "sources.known_roots", raw: `[""]`, wantErr: "non-empty"},
		{key: "sources.known_roots", raw: `"~/code"`, wantErr: "list of text"},
		{key: "rag.search.source_weights", raw: `{"web":11}`, wantErr: "up to 10"},
		{key: "rag.search.source_weights", raw: `{"web":"x"}`, wantErr: "a number"},
		{key: "rag.search.source_weights", raw: `{"a.b":1}`, wantErr: "invalid key"},
		{key: "routing.kinds", raw: `[]`, wantErr: "JSON object"},
		{key: "routing.kinds", raw: `{"code":3}`, wantErr: "one line"},
		{key: "queue.worker.allowed_tools", raw: `{"claude":"Bash"}`, wantErr: "list of text"},
		{key: "embedder.chunk_tokens", raw: "127", wantErr: "from 1 to 126"},
		{key: keyMaxConcurrent, raw: "{", wantErr: "not valid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.raw, func(t *testing.T) {
			got, err := ParseJSON(mustField(t, tt.key), tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(tt.want)
			if string(a) != string(b) {
				t.Errorf("got %s, want %s", a, b)
			}
		})
	}
}

func TestParseText(t *testing.T) {
	tests := []struct {
		key, text, want, wantErr string
	}{
		{key: keyMaxConcurrent, text: "5", want: "5"},
		{key: keyMaxConcurrent, text: "51", wantErr: "from 0 to 50"},
		{key: "pr_babysitter.enabled", text: "true", want: "true"},
		{key: "sources.known_roots", text: "a, b,,", want: `["a","b"]`},
		{key: "routing.kinds", text: `{"code":"fast"}`, want: `{"code":"fast"}`},
		{key: "queue.worker.timeout", text: "1h", want: `"1h"`},
		{key: "queue.worker.timeout", text: "x", wantErr: "positive duration"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := ParseText(mustField(t, tt.key), tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := json.Marshal(got); string(b) != tt.want {
				t.Errorf("got %s, want %s", b, tt.want)
			}
		})
	}
}

func TestUnmarshalHoldsTheQueueToItsLimits(t *testing.T) {
	tests := []struct {
		name                     string
		parallel, active         int
		wantParallel, wantActive int
	}{
		{name: "in range", parallel: 5, active: 20, wantParallel: 5, wantActive: 20},
		{name: "parallel past the limit", parallel: 500, active: 50, wantParallel: 50, wantActive: 50},
		{name: "negative parallel is off", parallel: -3, active: 20, wantParallel: 0, wantActive: 20},
		{name: "zero parallel stays off", parallel: 0, active: 20, wantParallel: 0, wantActive: 20},
		{name: "no cap is gone", parallel: 1, active: 0, wantParallel: 1, wantActive: 50},
		{name: "queue past the limit", parallel: 1, active: 900, wantParallel: 1, wantActive: 50},
		{name: "queue never below parallel", parallel: 30, active: 20, wantParallel: 30, wantActive: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := loadTemp(t)
			v.Set(keyMaxConcurrent, tt.parallel)
			v.Set(keyMaxActive, tt.active)
			s, err := Unmarshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if s.Queue.Worker.MaxConcurrent != tt.wantParallel || s.Queue.MaxActive != tt.wantActive {
				t.Errorf("parallel %d active %d", s.Queue.Worker.MaxConcurrent, s.Queue.MaxActive)
			}
		})
	}
}

func TestRuns(t *testing.T) {
	for _, tt := range []struct {
		w    QueueWorkerSettings
		want bool
	}{
		{QueueWorkerSettings{Enabled: true, MaxConcurrent: 1}, true},
		{QueueWorkerSettings{Enabled: true}, false},
		{QueueWorkerSettings{MaxConcurrent: 5}, false},
	} {
		if got := tt.w.Runs(); got != tt.want {
			t.Errorf("%+v: Runs = %v", tt.w, got)
		}
	}
}

func TestApply(t *testing.T) {
	queue := func(t *testing.T) QueueSettings {
		t.Helper()
		s, err := Unmarshal(reload(t))
		if err != nil {
			t.Fatal(err)
		}
		return s.Queue
	}

	t.Run("raising parallel raises the queue with it", func(t *testing.T) {
		loadTemp(t)
		if err := applyTemp(t, map[string]any{keyMaxConcurrent: 30}, false); err != nil {
			t.Fatal(err)
		}
		if q := queue(t); q.Worker.MaxConcurrent != 30 || q.MaxActive != 30 {
			t.Errorf("queue = %+v", q)
		}
	})
	t.Run("zero turns the worker off and any slots turn it back on", func(t *testing.T) {
		loadTemp(t)
		if err := applyTemp(t, map[string]any{keyMaxConcurrent: 0}, false); err != nil {
			t.Fatal(err)
		}
		if q := queue(t); q.Worker.Runs() {
			t.Errorf("worker still runs: %+v", q)
		}
		if got, _ := FieldValue(reload(t), mustField(t, keyMaxConcurrent)); got != "0" {
			t.Errorf("shown as %s", got)
		}
		seedTemp(t, keyWorkerEnabled, "false")
		if err := applyTemp(t, map[string]any{keyMaxConcurrent: 2}, false); err != nil {
			t.Fatal(err)
		}
		if q := queue(t); !q.Worker.Runs() || q.Worker.MaxConcurrent != 2 {
			t.Errorf("worker off after turning it on: %+v", q)
		}
	})
	t.Run("a worker switched off elsewhere shows as zero", func(t *testing.T) {
		v := loadTemp(t)
		v.Set(keyWorkerEnabled, false)
		if got, _ := FieldValue(v, mustField(t, keyMaxConcurrent)); got != "0" {
			t.Errorf("shown as %s", got)
		}
	})
	t.Run("the queue cannot go below parallel", func(t *testing.T) {
		loadTemp(t)
		if err := applyTemp(t, map[string]any{keyMaxConcurrent: 10}, false); err != nil {
			t.Fatal(err)
		}
		if err := applyTemp(t, map[string]any{keyMaxActive: 5}, false); err == nil {
			t.Fatal("a queue below parallel was accepted")
		}
		if err := applyTemp(t, map[string]any{keyMaxConcurrent: 20, keyMaxActive: 10}, false); err == nil {
			t.Fatal("a batch with the queue below parallel was accepted")
		}
		if err := applyTemp(t, map[string]any{keyMaxConcurrent: 4, keyMaxActive: 5}, false); err != nil {
			t.Fatal(err)
		}
		if q := queue(t); q.Worker.MaxConcurrent != 4 || q.MaxActive != 5 {
			t.Errorf("queue = %+v", q)
		}
	})
	t.Run("sensitive needs confirmation and writes nothing without it", func(t *testing.T) {
		loadTemp(t)
		err := applyTemp(t, map[string]any{"queue.worker.timeout": "5m", "queue.worker.permission_mode": "plan"}, false)
		if !errors.Is(err, ErrConfirmSensitive) {
			t.Fatalf("err = %v", err)
		}
		if reload(t).GetString("queue.worker.timeout") != "60m" {
			t.Errorf("part of a refused batch was written")
		}
		if err := applyTemp(t, map[string]any{"queue.worker.permission_mode": "plan"}, true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the environment no longer overrides a setting", func(t *testing.T) {
		loadTemp(t)
		t.Setenv("ORCH_QUEUE_WORKER_TIMEOUT", "5m")
		if got := reload(t).GetString("queue.worker.timeout"); got != "60m" {
			t.Errorf("timeout = %s, want the default", got)
		}
	})
	t.Run("a whole map replaces the previous one", func(t *testing.T) {
		loadTemp(t)
		f := mustField(t, "rag.search.source_weights")
		val, err := ParseJSON(f, `{"web":2}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyTemp(t, map[string]any{f.Key: val}, false); err != nil {
			t.Fatal(err)
		}
		s, _ := Unmarshal(reload(t))
		if s.RAG.Search.SourceWeights["web"] != 2 {
			t.Errorf("weights = %v", s.RAG.Search.SourceWeights)
		}
	})
}

// seedTemp stores a raw JSON value in loadTemp's orch.db, past the checks.
func seedTemp(t *testing.T, key, raw string) {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(`INSERT OR REPLACE INTO config_values (key, value, updated_at, updated_by) VALUES (?, ?, 'now', 'test')`, key, raw); err != nil {
		t.Fatal(err)
	}
}

func TestEntries(t *testing.T) {
	loadTemp(t)
	if err := applyTemp(t, map[string]any{"sources.known_roots": []string{"a", "b"}}, true); err != nil {
		t.Fatal(err)
	}
	entries, err := Entries(reload(t), Defaults())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Entry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	if _, shown := byKey[keyWorkerEnabled]; shown {
		t.Error("queue.worker.enabled is offered alongside max_concurrent's 0")
	}
	if got := byKey["sources.known_roots"]; got.Value != `["a","b"]` || got.Default != `["~/code"]` {
		t.Errorf("known_roots = %+v", got)
	}
	if !byKey["sources.known_roots"].Overridden() || byKey["docs.scan_interval"].Overridden() {
		t.Error("overridden not reported per key")
	}
	if got := byKey["rag.search.source_weights"]; !strings.Contains(got.Value, `"web":0.9`) || got.Value != got.Default {
		t.Errorf("source_weights = %+v", got)
	}
	if got := byKey["routing.efforts"]; got.Value != "{}" {
		t.Errorf("an unset map = %s", got.Value)
	}
	if got := byKey["queue.worker.allowed_tools"]; !strings.Contains(got.Value, "git status") {
		t.Errorf("allowed_tools = %s", got.Value)
	}
	if got := byKey[keyMaxConcurrent]; got.Value != "1" || got.Max != MaxParallelTasks {
		t.Errorf("max_concurrent = %+v", got)
	}
	for _, e := range entries {
		f := e.Field
		if _, err := ParseJSON(f, e.Default); err != nil {
			t.Errorf("%s: its own default does not validate: %v", f.Key, err)
		}
	}
}

func TestSnapshotAndPendingRestart(t *testing.T) {
	v := loadTemp(t)
	started, err := Snapshot(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := started["hooks.auto_rag.top_k"]; has {
		t.Error("a live setting is in the snapshot")
	}
	if err := applyTemp(t, map[string]any{keyMaxConcurrent: 3, "hooks.auto_rag.top_k": 9}, false); err != nil {
		t.Fatal(err)
	}
	now, err := Snapshot(reload(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := PendingRestart(started, now); !slices.Equal(got, []string{keyMaxConcurrent}) {
		t.Errorf("pending = %v", got)
	}
}

// A write must not read as a change to other settings: a false "restart
// needed" would follow every edit.
func TestAWriteChangesNothingElse(t *testing.T) {
	v := loadTemp(t)
	before, err := Snapshot(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyTemp(t, map[string]any{"hooks.auto_rag.top_k": 9}, false); err != nil {
		t.Fatal(err)
	}
	after, err := Snapshot(reload(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range PendingRestart(before, after) {
		t.Errorf("%s: %s became %s", k, before[k], after[k])
	}
}

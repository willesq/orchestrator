package status

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/check"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/supervisor"
)

func TestSubsystemSectionDetails(t *testing.T) {
	tests := []struct {
		name      string
		sub       *orchdv1.SubsystemStatus
		wantLevel string
		wantHas   []string
		wantNotIn []string
	}{
		{
			name: "running with a beat, restarts and detail",
			sub: &orchdv1.SubsystemStatus{
				Name: "qdrant", State: supervisor.StateRunning, Detail: "native engine on :6333",
				LastBeat: testNow.Add(-90 * time.Second).Unix(), Restarts: 2,
			},
			wantLevel: LevelOK,
			wantHas:   []string{"native engine on :6333", "last tick 1m ago", "2 restart(s)"},
		},
		{
			name:      "failed carries the error text",
			sub:       &orchdv1.SubsystemStatus{Name: "qdrant", State: supervisor.StateFailed, Error: "port in use"},
			wantLevel: LevelBad,
			wantHas:   []string{"error: port in use"},
		},
		{
			name:      "never-ticked unit omits the tick clause",
			sub:       &orchdv1.SubsystemStatus{Name: "qdrant", State: supervisor.StateRunning},
			wantLevel: LevelOK,
			wantNotIn: []string{"last tick", "restart(s)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
				return &orchdv1.StatusResponse{Subsystems: []*orchdv1.SubsystemStatus{tt.sub}}, nil
			}
			got := line(t, section(t, Collect(context.Background(), d), "subsystems"), tt.sub.GetName())
			if got.Level != tt.wantLevel {
				t.Errorf("level = %q, want %q", got.Level, tt.wantLevel)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q missing %q", got.Detail, want)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(got.Detail, unwanted) {
					t.Errorf("detail %q should not contain %q", got.Detail, unwanted)
				}
			}
		})
	}
}

func TestSubsystemSectionNotes(t *testing.T) {
	tests := []struct {
		name  string
		orchd func(context.Context) (*orchdv1.StatusResponse, error)
		want  string
	}{
		{
			name:  "daemon down",
			orchd: func(context.Context) (*orchdv1.StatusResponse, error) { return nil, errors.New("nope") },
			want:  "orchd is not running",
		},
		{
			name: "daemon up but supervising nothing",
			orchd: func(context.Context) (*orchdv1.StatusResponse, error) {
				return &orchdv1.StatusResponse{}, nil
			},
			want: "no supervised subsystems",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Orchd = tt.orchd
			sec := section(t, Collect(context.Background(), d), "subsystems")
			if len(sec.Lines) != 0 {
				t.Errorf("expected no lines, got %v", sec.Lines)
			}
			if !strings.Contains(sec.Note, tt.want) {
				t.Errorf("note %q missing %q", sec.Note, tt.want)
			}
		})
	}
}

func TestVectorSection(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		ready       bool
		wantValue   string
		wantLevel   string
		wantStorage bool
		wantDetail  string
	}{
		{
			name: "binary mode reports orchd ownership and storage",
			mode: "binary", ready: true,
			wantValue: "ready", wantLevel: LevelOK, wantStorage: true,
			wantDetail: "orchd-managed",
		},
		{
			name: "external mode has no storage line",
			mode: "external", ready: true,
			wantValue: "ready", wantLevel: LevelOK, wantStorage: false,
			wantDetail: "externally managed",
		},
		{
			name: "an unreachable engine is a problem",
			mode: "binary", ready: false,
			wantValue: "not reachable", wantLevel: LevelBad, wantStorage: true,
			wantDetail: "orchd-managed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Settings.RAG.Qdrant.Mode = tt.mode
			d.Settings.RAG.Qdrant.URL = "http://example:6333"
			d.QdrantReady = func(context.Context) bool { return tt.ready }

			sec := section(t, Collect(context.Background(), d), "vector store")
			engine := line(t, sec, "engine")
			if engine.Value != tt.wantValue || engine.Level != tt.wantLevel {
				t.Errorf("engine = %+v, want %s/%s", engine, tt.wantValue, tt.wantLevel)
			}
			if !strings.Contains(engine.Detail, tt.wantDetail) {
				t.Errorf("detail %q missing %q", engine.Detail, tt.wantDetail)
			}
			hasStorage := false
			for _, l := range sec.Lines {
				if l.Name == "storage" {
					hasStorage = true
				}
			}
			if hasStorage != tt.wantStorage {
				t.Errorf("storage line present = %v, want %v", hasStorage, tt.wantStorage)
			}
		})
	}
}

func TestVectorSectionUnsetConfig(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "empty mode and collection render as (unset)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Settings = config.Settings{}
			sec := section(t, Collect(context.Background(), d), "vector store")
			if got := line(t, sec, "collection"); got.Value != "(unset)" {
				t.Errorf("collection = %q, want (unset)", got.Value)
			}
			if got := line(t, sec, "engine"); !strings.Contains(got.Detail, "mode=(unset)") {
				t.Errorf("engine detail = %q, want mode=(unset)", got.Detail)
			}
		})
	}
}

func TestDataSectionWithoutStore(t *testing.T) {
	tests := []struct {
		name      string
		store     func() (*sql.DB, error)
		createDB  bool
		wantValue string
		wantLevel string
	}{
		{
			name:      "absent store is reported, not created",
			createDB:  false,
			store:     func() (*sql.DB, error) { t.Fatal("must not open a store that does not exist"); return nil, nil },
			wantValue: "not created yet",
		},
		{
			name:      "nil opener",
			createDB:  true,
			store:     nil,
			wantValue: "not inspected",
			wantLevel: LevelWarn,
		},
		{
			name:      "failing opener is a warning",
			createDB:  true,
			store:     func() (*sql.DB, error) { return nil, errors.New("locked") },
			wantValue: "unreadable",
			wantLevel: LevelWarn,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Store = tt.store
			if tt.createDB {
				conn, err := db.Open()
				if err != nil {
					t.Fatalf("db.Open: %v", err)
				}
				_ = conn.Close()
			}
			got := line(t, section(t, Collect(context.Background(), d), "data"), "registries")
			if got.Value != tt.wantValue || got.Level != tt.wantLevel {
				t.Errorf("registries = %+v, want %s/%s", got, tt.wantValue, tt.wantLevel)
			}
		})
	}
}

func TestDataSectionCounts(t *testing.T) {
	tests := []struct {
		name      string
		wantTasks string
		wantSrc   string
	}{
		{name: "counts rows from a live store", wantTasks: "2 recorded", wantSrc: "1 registered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			conn, err := db.Open()
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			ctx := context.Background()
			for _, row := range []struct{ id, st string }{{"t1", "complete"}, {"t2", "running"}} {
				if _, err := conn.ExecContext(ctx,
					`INSERT INTO tasks (id, agent, body, status, updated_at) VALUES (?,?,?,?,datetime('now'))`,
					row.id, "dev", "body", row.st); err != nil {
					t.Fatalf("seed task: %v", err)
				}
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO sources (id, kind, uri, refresh_secs, last_fetched, status) VALUES (?,?,?,?,?,?)`,
				"web|x", "web", "http://x", 60, testNow.Unix()-600, "stale"); err != nil {
				t.Fatalf("seed source: %v", err)
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO docs_owners (owner, refresh_secs, last_synced, status) VALUES (?,?,?,?)`,
				"willesq", 3600, testNow.Unix()-1800, "ok"); err != nil {
				t.Fatalf("seed owner: %v", err)
			}
			_ = conn.Close()
			d.Store = db.OpenReader

			sec := section(t, Collect(context.Background(), d), "data")
			if got := line(t, sec, "tasks"); got.Value != tt.wantTasks {
				t.Errorf("tasks = %q, want %q", got.Value, tt.wantTasks)
			}
			src := line(t, sec, "sources")
			if src.Value != tt.wantSrc || src.Level != LevelWarn {
				t.Errorf("sources = %+v, want %q/warn", src, tt.wantSrc)
			}
			if !strings.Contains(src.Detail, "1 due") || !strings.Contains(src.Detail, "1 stale") {
				t.Errorf("sources detail = %q, want due and stale counts", src.Detail)
			}
			if got := line(t, sec, "docs owners"); !strings.Contains(got.Detail, "last sync 30m ago") {
				t.Errorf("docs owners detail = %q, want a last-sync age", got.Detail)
			}
		})
	}
}

func TestDataSectionQueryErrors(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{name: "a closed handle warns per registry", lines: []string{"tasks", "sources", "docs owners"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			conn, err := db.Open()
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			_ = conn.Close()
			d.Store = func() (*sql.DB, error) { return conn, nil } // already closed

			sec := section(t, Collect(context.Background(), d), "data")
			for _, name := range tt.lines {
				if got := line(t, sec, name); got.Value != "unreadable" || got.Level != LevelWarn {
					t.Errorf("%s = %+v, want unreadable/warn", name, got)
				}
			}
		})
	}
}

func TestPRSLine(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		wantValue  string
		wantDetail []string
	}{
		{
			name:      "no cache file yet",
			wantValue: "0 PRs", wantDetail: []string{"no cache file yet"},
		},
		{
			name: "counts blocked and ready PRs",
			state: `{"prs":{"a":{"url":"u1","ci":"FAILURE","review":"APPROVED"},` +
				`"b":{"url":"u2","ci":"SUCCESS","review":"APPROVED"}}}`,
			wantValue: "2 PRs", wantDetail: []string{"updated "},
		},
		{
			name:      "corrupt cache reads as empty",
			state:     `not json`,
			wantValue: "0 PRs", wantDetail: []string{"updated "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			if tt.state != "" {
				if err := os.MkdirAll(filepath.Dir(prsPath()), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(prsPath(), []byte(tt.state), 0o644); err != nil {
					t.Fatalf("write state: %v", err)
				}
			}
			got := line(t, section(t, Collect(context.Background(), d), "data"), "pr cache")
			if got.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", got.Value, tt.wantValue)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q missing %q", got.Detail, want)
				}
			}
		})
	}
}

func TestHarnessSection(t *testing.T) {
	tests := []struct {
		name      string
		info      HarnessInfo
		wantValue string
		wantLevel string
		wantHas   string
	}{
		{name: "fully wired", info: HarnessInfo{Name: "claude-code"}, wantValue: "wired", wantLevel: LevelOK},
		{
			name: "drifted names the fix", info: HarnessInfo{Name: "codex", Pending: 2},
			wantValue: "drifted", wantLevel: LevelWarn, wantHas: "orch install --target codex --apply",
		},
		{
			name: "error is surfaced", info: HarnessInfo{Name: "gemini", Err: "bad plan"},
			wantValue: "unknown", wantLevel: LevelWarn, wantHas: "bad plan",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Harness = func() []HarnessInfo { return []HarnessInfo{tt.info} }
			got := line(t, section(t, Collect(context.Background(), d), "harness wiring"), tt.info.Name)
			if got.Value != tt.wantValue || got.Level != tt.wantLevel {
				t.Errorf("line = %+v, want %s/%s", got, tt.wantValue, tt.wantLevel)
			}
			if tt.wantHas != "" && !strings.Contains(got.Detail, tt.wantHas) {
				t.Errorf("detail %q missing %q", got.Detail, tt.wantHas)
			}
		})
	}
}

func TestPreconditionSection(t *testing.T) {
	tests := []struct {
		name       string
		result     check.Result
		wantValue  string
		wantLevel  string
		wantDetail string
	}{
		{
			name:      "passing check with a redundant detail drops it",
			result:    check.Result{Name: "gh auth", Detail: "ok", OK: true},
			wantValue: "ok", wantLevel: LevelOK, wantDetail: "",
		},
		{
			name:      "passing check keeps a useful detail",
			result:    check.Result{Name: "embedding model", Detail: "/models", OK: true},
			wantValue: "ok", wantLevel: LevelOK, wantDetail: "/models",
		},
		{
			name:      "failing check is a problem",
			result:    check.Result{Name: "embedding model", Detail: "missing", OK: false},
			wantValue: "failing", wantLevel: LevelBad, wantDetail: "missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Checks = []check.Check{func() check.Result { return tt.result }}
			got := line(t, section(t, Collect(context.Background(), d), "preconditions"), tt.result.Name)
			if got.Value != tt.wantValue || got.Level != tt.wantLevel || got.Detail != tt.wantDetail {
				t.Errorf("line = %+v, want %s/%s/%q", got, tt.wantValue, tt.wantLevel, tt.wantDetail)
			}
		})
	}
}

func TestServiceSectionManagerDetailFallback(t *testing.T) {
	tests := []struct {
		name string
		info ServiceInfo
		want string
	}{
		{
			name: "uninstalled with no detail gets install advice",
			info: ServiceInfo{Manager: "launchd"},
			want: "orch install --apply",
		},
		{
			name: "uninstalled with a detail keeps it",
			info: ServiceInfo{Manager: "launchd", Detail: "plist absent: /x"},
			want: "plist absent: /x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Service = func() ServiceInfo { return tt.info }
			got := line(t, section(t, Collect(context.Background(), d), "service"), "service manager")
			if got.Level != LevelWarn || !strings.Contains(got.Detail, tt.want) {
				t.Errorf("line = %+v, want warn with %q", got, tt.want)
			}
		})
	}
}

func TestVectorSectionRedactsURLCredentials(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantHas   string
		wantNotIn string
	}{
		{
			name: "userinfo is redacted",
			url:  "http://admin:hunter2@qdrant.internal:6333",
			// Reports get pasted into issues, so the password must not survive.
			wantHas: "admin:xxxxx@qdrant.internal:6333", wantNotIn: "hunter2",
		},
		{
			name:    "a credential-free url is untouched",
			url:     "http://qdrant.internal:6333",
			wantHas: "http://qdrant.internal:6333",
		},
		{
			name:    "an unparseable url is passed through as-is",
			url:     "://not-a-url",
			wantHas: "://not-a-url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			d.Settings.RAG.Qdrant.Mode = "external"
			d.Settings.RAG.Qdrant.URL = tt.url

			got := line(t, section(t, Collect(context.Background(), d), "vector store"), "engine")
			if !strings.Contains(got.Detail, tt.wantHas) {
				t.Errorf("detail %q missing %q", got.Detail, tt.wantHas)
			}
			if tt.wantNotIn != "" && strings.Contains(got.Detail, tt.wantNotIn) {
				t.Errorf("detail %q leaked %q", got.Detail, tt.wantNotIn)
			}
		})
	}
}

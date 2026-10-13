package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/check"
	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/supervisor"
)

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// healthyDeps is a fully-wired Deps with nothing wrong, for tests that vary one
// collector at a time.
func healthyDeps(t *testing.T) Deps {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))

	s := config.Settings{}
	s.RAG.Qdrant.Mode = "binary"
	s.RAG.Qdrant.HTTPPort = 6333
	s.RAG.Qdrant.Collection = "orchestrator"

	return Deps{
		Settings: s,
		Now:      func() time.Time { return testNow },
		Orchd: func(context.Context) (*orchdv1.StatusResponse, error) {
			return &orchdv1.StatusResponse{
				Version:       "1.2.3",
				UptimeSeconds: 3600,
				Subsystems: []*orchdv1.SubsystemStatus{{
					Name:      "sources-scheduler",
					State:     supervisor.StateRunning,
					Detail:    "freshness",
					StartedAt: testNow.Add(-time.Hour).Unix(),
					LastBeat:  testNow.Add(-3 * time.Minute).Unix(),
					Restarts:  1,
				}},
			}, nil
		},
		QdrantReady: func(context.Context) bool { return true },
		Service: func() ServiceInfo {
			return ServiceInfo{Manager: "launchd", Detail: "/plist", Installed: true}
		},
		SocketPresent: func() bool { return true },
		Checks:        []check.Check{func() check.Result { return check.Result{Name: "gh auth", Detail: "ok", OK: true} }},
		Harness:       func() []HarnessInfo { return []HarnessInfo{{Name: "claude-code"}} },
		DirSize:       func(string) int64 { return 1024 },
	}
}

// section finds a section by title.
func section(t *testing.T, r Report, title string) Section {
	t.Helper()
	for _, s := range r.Sections {
		if s.Title == title {
			return s
		}
	}
	t.Fatalf("no %q section in report", title)
	return Section{}
}

// line finds a line by name within a section.
func line(t *testing.T, sec Section, name string) Line {
	t.Helper()
	for _, l := range sec.Lines {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no %q line in section %q", name, sec.Title)
	return Line{}
}

func TestCollectSectionOrder(t *testing.T) {
	tests := []struct {
		name  string
		want  []string
		fatal bool
	}{
		{
			name: "fixed section order",
			want: []string{"service", "subsystems", "vector store", "data", "harness wiring", "preconditions"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Collect(context.Background(), healthyDeps(t))
			var got []string
			for _, s := range r.Sections {
				got = append(got, s.Title)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("sections = %v, want %v", got, tt.want)
			}
			if r.Version == "" || !r.At.Equal(testNow) {
				t.Errorf("report header = %q/%v, want a version and the injected clock", r.Version, r.At)
			}
		})
	}
}

func TestCollectHealthyHasNoProblems(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "everything up, nothing flagged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Collect(context.Background(), healthyDeps(t))
			if r.Problems != 0 {
				t.Errorf("problems = %d, want 0 (report: %+v)", r.Problems, r.Sections)
			}
			if got := line(t, section(t, r, "service"), "orchd"); got.Value != "running" || got.Level != LevelOK {
				t.Errorf("orchd line = %+v, want running/ok", got)
			}
		})
	}
}

func TestCollectCountsOnlyBadAsProblems(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(d *Deps)
		wantProblems int
	}{
		{
			name: "orchd down is a problem",
			mutate: func(d *Deps) {
				d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
					return nil, errors.New("connection refused")
				}
			},
			wantProblems: 1,
		},
		{
			name:         "unreachable qdrant is a problem",
			mutate:       func(d *Deps) { d.QdrantReady = func(context.Context) bool { return false } },
			wantProblems: 1,
		},
		{
			name: "a failing precondition is a problem",
			mutate: func(d *Deps) {
				d.Checks = []check.Check{func() check.Result {
					return check.Result{Name: "gh auth", Detail: "not logged in", OK: false}
				}}
			},
			wantProblems: 1,
		},
		{
			name: "a stopped subsystem is a problem",
			mutate: func(d *Deps) {
				d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
					return &orchdv1.StatusResponse{Subsystems: []*orchdv1.SubsystemStatus{
						{Name: "docs-scheduler", State: supervisor.StateStopped},
					}}, nil
				}
			},
			wantProblems: 1,
		},
		{
			name:         "drifted harness wiring is only a warning",
			mutate:       func(d *Deps) { d.Harness = func() []HarnessInfo { return []HarnessInfo{{Name: "codex", Pending: 3}} } },
			wantProblems: 0,
		},
		{
			name:         "an uninstalled service manager is only a warning",
			mutate:       func(d *Deps) { d.Service = func() ServiceInfo { return ServiceInfo{Manager: "launchd"} } },
			wantProblems: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthyDeps(t)
			tt.mutate(&d)
			if got := Collect(context.Background(), d).Problems; got != tt.wantProblems {
				t.Errorf("problems = %d, want %d", got, tt.wantProblems)
			}
		})
	}
}

func TestZeroDepsIsSafe(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "a zero Deps still produces a report"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			r := Collect(context.Background(), Deps{})
			if len(r.Sections) != 6 {
				t.Fatalf("got %d sections, want 6", len(r.Sections))
			}
			// Unknown service manager and no harness info degrade to notes/warnings.
			if got := line(t, section(t, r, "service"), "service manager"); got.Value != "unknown" {
				t.Errorf("service manager = %q, want unknown", got.Value)
			}
			if got := section(t, r, "harness wiring"); got.Note != "not inspected" {
				t.Errorf("harness note = %q, want 'not inspected'", got.Note)
			}
			if got := section(t, r, "preconditions"); got.Note == "" {
				t.Errorf("expected a note when no checks are configured")
			}
		})
	}
}

func TestFileLineAndDirSize(t *testing.T) {
	tests := []struct {
		name      string
		write     int
		wantValue string
	}{
		{name: "absent file", write: -1, wantValue: "absent"},
		{name: "sized file", write: 2048, wantValue: "2.0 KB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.db")
			if tt.write >= 0 {
				if err := os.WriteFile(path, make([]byte, tt.write), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if got := fileLine("f.db", path); got.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", got.Value, tt.wantValue)
			}
			if tt.write >= 0 {
				if got := DirSize(dir); got != int64(tt.write) {
					t.Errorf("DirSize = %d, want %d", got, tt.write)
				}
			}
		})
	}
}

func TestDirSizeMissingDir(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "absent directory sums to zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DirSize(filepath.Join(t.TempDir(), "nope")); got != 0 {
				t.Errorf("DirSize = %d, want 0", got)
			}
		})
	}
}

// prsPath mirrors where the babysitter cache lives, for seeding it in tests.
func prsPath() string { return filepath.Join(os.Getenv("ORCH_DATA_DIR"), "pr_babysitter_state.json") }

func TestInstructionLoadSection(t *testing.T) {
	r := Collect(context.Background(), Deps{InstructionLoad: func() []Line {
		return []Line{{Name: "claude-code", Value: "≈9.2k tokens", Level: LevelOK}}
	}})
	var sec *Section
	for i := range r.Sections {
		if r.Sections[i].Title == "always-loaded instructions" {
			sec = &r.Sections[i]
		}
	}
	if sec == nil || len(sec.Lines) != 1 || sec.Lines[0].Value != "≈9.2k tokens" {
		t.Fatalf("section = %+v", sec)
	}
	empty := Collect(context.Background(), Deps{InstructionLoad: func() []Line { return nil }})
	for _, s := range empty.Sections {
		if s.Title == "always-loaded instructions" && s.Note == "" {
			t.Errorf("an empty section must say why")
		}
	}
}

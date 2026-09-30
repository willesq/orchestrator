package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/selfupdate"
)

func TestQdrantUnit(t *testing.T) {
	// A stub already serving /readyz lets binary mode reuse it (no download/spawn).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	tests := []struct {
		name    string
		mode    string
		wantOK  bool
		wantErr bool
	}{
		{name: "binary mode yields a startable unit", mode: "binary", wantOK: true},
		{name: "external mode has no unit to supervise", mode: "external", wantOK: false},
		{name: "docker mode has no unit to supervise", mode: "docker", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.RAG.Qdrant.Mode = tt.mode
			s.RAG.Qdrant.HTTPPort = port
			s.RAG.Qdrant.Version = "v1.17.1"

			u, ok := qdrantUnit(s, "")
			if ok != tt.wantOK {
				t.Fatalf("qdrantUnit ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if u.Name != UnitQdrant || !strings.Contains(u.Detail, portStr) {
				t.Errorf("unit = %+v, want name %q and the port in Detail", u, UnitQdrant)
			}
			var beats int
			if err := u.Run(context.Background(), func() { beats++ }); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if beats != 1 {
				t.Errorf("beats = %d, want 1", beats)
			}
			if err := u.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
			}
		})
	}
}

func TestQdrantUnitRunError(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "an unreachable engine surfaces a start error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.RAG.Qdrant.Mode = "binary"
			s.RAG.Qdrant.HTTPPort = 1 // nothing serves here, and the download is bogus
			s.RAG.Qdrant.Version = "v0.0.0-does-not-exist"

			u, ok := qdrantUnit(s, "")
			if !ok {
				t.Fatal("expected a unit in binary mode")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := u.Run(ctx, func() {})
			if err == nil || !strings.Contains(err.Error(), "start qdrant") {
				t.Fatalf("got %v, want a wrapped start qdrant error", err)
			}
		})
	}
}

func TestSubsystems(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		prs      bool
		watch    bool
		wantUnit []string
	}{
		{
			name: "binary mode with the babysitter on",
			mode: "binary", prs: true,
			wantUnit: []string{UnitQdrant, UnitWorker, UnitSources, UnitDocs, UnitPRs},
		},
		{
			name: "external qdrant and the babysitter off",
			mode: "external", prs: false,
			wantUnit: []string{UnitWorker, UnitSources, UnitDocs},
		},
		{
			// A watcher orchd could not build (an unreadable executable path)
			// must cost it the unit, not the daemon.
			name: "a watched binary adds the self-update unit last",
			mode: "external", prs: false, watch: true,
			wantUnit: []string{UnitWorker, UnitSources, UnitDocs, UnitSelfUpdate},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.RAG.Qdrant.Mode = tt.mode
			s.RAG.Qdrant.HTTPPort = 6333
			s.PRBabysitter.Enabled = tt.prs

			d := deps{settings: s}
			if tt.watch {
				bin := filepath.Join(t.TempDir(), "orchd")
				if err := os.WriteFile(bin, []byte("build"), 0o755); err != nil {
					t.Fatalf("write: %v", err)
				}
				w, err := selfupdate.NewWatcher(bin)
				if err != nil {
					t.Fatalf("NewWatcher: %v", err)
				}
				d.selfUpdate, d.restart = w, func(string) {}
			}

			var got []string
			for _, u := range subsystems(d) {
				// Every unit must be well-formed for the supervisor: exactly one
				// of Run (service) or Tick (periodic), and a periodic one needs a
				// positive cadence.
				switch {
				case (u.Run == nil) == (u.Tick == nil):
					t.Errorf("unit %q must set exactly one of Run or Tick", u.Name)
				case u.Tick != nil && u.Interval <= 0:
					t.Errorf("periodic unit %q has interval %v, want > 0", u.Name, u.Interval)
				}
				got = append(got, u.Name)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantUnit, ",") {
				t.Errorf("subsystems = %v, want %v", got, tt.wantUnit)
			}
		})
	}
}

func TestInterval(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		fallback time.Duration
		want     time.Duration
	}{
		{name: "parses a valid duration", spec: "30s", fallback: time.Minute, want: 30 * time.Second},
		{name: "empty falls back", spec: "", fallback: time.Minute, want: time.Minute},
		{name: "unparseable falls back", spec: "banana", fallback: time.Minute, want: time.Minute},
		{name: "zero falls back", spec: "0s", fallback: time.Minute, want: time.Minute},
		{name: "negative falls back", spec: "-5s", fallback: time.Minute, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interval(tt.spec, tt.fallback); got != tt.want {
				t.Errorf("interval(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

// TestSelfUpdateUnit covers the decision the unit makes on each tick: exit so
// the service manager can bring the new build up, or — when nothing would bring
// orchd back — report the drift and keep serving the old one.
func TestSelfUpdateUnit(t *testing.T) {
	tests := []struct {
		name        string
		upgrade     bool
		supervised  bool
		wantRestart bool
	}{
		{name: "an installed build restarts a supervised orchd", upgrade: true, supervised: true, wantRestart: true},
		{name: "an unsupervised orchd keeps serving", upgrade: true, supervised: false, wantRestart: false},
		{name: "an unchanged binary is left alone", upgrade: false, supervised: true, wantRestart: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "orchd")
			if err := os.WriteFile(bin, []byte("old build"), 0o755); err != nil {
				t.Fatalf("write: %v", err)
			}
			w, err := selfupdate.NewWatcher(bin)
			if err != nil {
				t.Fatalf("NewWatcher: %v", err)
			}

			var reasons []string
			u := selfUpdateUnit(w, func(reason string) { reasons = append(reasons, reason) }, tt.supervised)
			if u.Name != UnitSelfUpdate || u.Tick == nil || u.Interval <= 0 {
				t.Fatalf("unit = %+v, want a periodic unit named %q", u, UnitSelfUpdate)
			}

			if tt.upgrade {
				if err := os.WriteFile(bin, []byte("a visibly longer new build"), 0o755); err != nil {
					t.Fatalf("rewrite: %v", err)
				}
			}
			u.Tick(context.Background())
			// A second tick only happens on the paths that keep serving — the
			// supervised one cancels the daemon's context and ends the loop.
			if !tt.wantRestart {
				u.Tick(context.Background())
			}

			if got := len(reasons) > 0; got != tt.wantRestart {
				t.Fatalf("restart requested = %v (%v), want %v", got, reasons, tt.wantRestart)
			}
			if tt.wantRestart && !strings.Contains(reasons[0], bin) {
				t.Errorf("reason = %q, want it to name the launch path %q", reasons[0], bin)
			}
			if !tt.supervised && tt.upgrade {
				if _, changed := w.Changed(); changed {
					t.Error("an unsupervised tick left the drift unadopted, so it will re-report forever")
				}
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		def  string
		want string
	}{
		{name: "empty uses default", in: "", def: "d", want: "d"},
		{name: "value wins", in: "v", def: "d", want: "v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orDefault(tt.in, tt.def); got != tt.want {
				t.Errorf("orDefault(%q,%q) = %q, want %q", tt.in, tt.def, got, tt.want)
			}
		})
	}
	// resolveOnnxLibDir returns a non-empty path (brew or fallback).
	if resolveOnnxLibDir() == "" {
		t.Errorf("resolveOnnxLibDir returned empty")
	}
}

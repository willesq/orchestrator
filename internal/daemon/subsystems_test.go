package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/config"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.RAG.Qdrant.Mode = tt.mode
			s.RAG.Qdrant.HTTPPort = 6333
			s.PRBabysitter.Enabled = tt.prs

			var got []string
			for _, u := range subsystems(deps{settings: s}) {
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

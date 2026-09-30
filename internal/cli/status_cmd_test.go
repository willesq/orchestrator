package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/status"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// isolate points config, data and socket paths at throwaway dirs so a test never
// reads the developer's real orchestrator state.
func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("ORCH_SOCKET", filepath.Join(dir, "orchd.sock"))
	t.Setenv("HOME", dir)
}

func TestStatusCmd(t *testing.T) {
	tests := []struct {
		name      string
		client    *fakeClient
		dialErr   error
		wantHas   []string
		wantNotIn []string
	}{
		{
			name: "reports a running daemon and its subsystems",
			client: &fakeClient{status: &orchdv1.StatusResponse{
				Version: "9.9.9", UptimeSeconds: 120,
				Subsystems: []*orchdv1.SubsystemStatus{
					{Name: "qdrant", State: supervisor.StateRunning, Detail: "native engine on :6333"},
					{Name: "docs-scheduler", State: supervisor.StateStopped},
				},
			}},
			// The problem count is deliberately not asserted: the precondition
			// checks run for real here, so it varies by machine.
			wantHas: []string{
				"service", "orchd", "running", "9.9.9, up 2m",
				"subsystems", "qdrant", "native engine on :6333",
				" ! docs-scheduler", "vector store", "data", "harness wiring", "preconditions",
			},
		},
		{
			name:    "still reports with the daemon down",
			dialErr: errNoDaemon,
			wantHas: []string{" ! orchd", "not running", "unavailable — orchd is not running"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			client := tt.client
			if client == nil {
				client = &fakeClient{}
			}
			out, err := run(factory(client, tt.dialErr), "", "status")
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(out, unwanted) {
					t.Errorf("output should not contain %q:\n%s", unwanted, out)
				}
			}
		})
	}
}

func TestStatusCmdJSON(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "--json emits a parseable report"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			out, err := run(factory(&fakeClient{status: &orchdv1.StatusResponse{Version: "1.0.0"}}, nil),
				"", "status", "--json")
			if err != nil {
				t.Fatalf("status --json: %v", err)
			}
			var report status.Report
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			if len(report.Sections) == 0 {
				t.Errorf("no sections in JSON report")
			}
		})
	}
}

func TestQdrantReady(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ready.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sick.Close()
	// A readiness check must answer for the host we configured, not for wherever
	// that host chooses to redirect us.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ready.URL+"/readyz", http.StatusFound)
	}))
	defer redirector.Close()

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "healthy engine", url: ready.URL, want: true},
		{name: "unhealthy engine", url: sick.URL, want: false},
		{name: "nothing listening", url: "http://127.0.0.1:1", want: false},
		{name: "empty url", url: "", want: false},
		{name: "malformed url", url: "http://[::1]:namedport", want: false},
		{name: "non-http scheme is refused", url: "file:///etc/passwd", want: false},
		{name: "scheme without a host is refused", url: "http://", want: false},
		{name: "trailing slash still resolves /readyz", url: ready.URL + "/", want: true},
		{name: "a redirect is not followed", url: redirector.URL, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qdrantReady(context.Background(), tt.url); got != tt.want {
				t.Errorf("qdrantReady(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestServiceInfo(t *testing.T) {
	tests := []struct {
		name          string
		homebrew      bool
		writePlist    bool
		wantManager   string
		wantInstalled bool
		wantDetailHas string
	}{
		{
			name: "homebrew-managed", homebrew: true,
			wantManager: "brew services", wantInstalled: true, wantDetailHas: "orch formula",
		},
		{
			name: "launchd with the plist installed", writePlist: true,
			wantManager: "launchd", wantInstalled: true, wantDetailHas: "com.orchestrator.orchd.plist",
		},
		{
			name: "launchd without a plist", writePlist: false,
			wantManager: "launchd", wantInstalled: false, wantDetailHas: "plist absent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			saved := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return tt.homebrew }
			t.Cleanup(func() { daemonIsHomebrew = saved })

			if tt.writePlist {
				plist := orchdPlistPath()
				if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
					t.Fatalf("write plist: %v", err)
				}
			}

			got := serviceInfo()
			if got.Manager != tt.wantManager || got.Installed != tt.wantInstalled {
				t.Errorf("info = %+v, want %s/installed=%v", got, tt.wantManager, tt.wantInstalled)
			}
			if !strings.Contains(got.Detail, tt.wantDetailHas) {
				t.Errorf("detail %q missing %q", got.Detail, tt.wantDetailHas)
			}
		})
	}
}

func TestHarnessInfo(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "reports pending edits for every adapter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t) // an empty HOME means every harness needs wiring
			infos := harnessInfo()
			if len(infos) != 3 {
				t.Fatalf("got %d harnesses, want 3: %+v", len(infos), infos)
			}
			for _, info := range infos {
				if info.Err != "" {
					t.Errorf("%s reported an error: %s", info.Name, info.Err)
				}
				if info.Pending == 0 {
					t.Errorf("%s reported no pending edits against an empty HOME", info.Name)
				}
			}
		})
	}
}

func TestInstallBins(t *testing.T) {
	tests := []struct {
		name   string
		binDir string
	}{
		{name: "explicit dir", binDir: "/opt/orch/bin"},
		{name: "defaults to the running executable's dir", binDir: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bins, err := installBins(tt.binDir)
			if err != nil {
				t.Fatalf("installBins: %v", err)
			}
			if tt.binDir != "" {
				if bins.Orch != filepath.Join(tt.binDir, "orch") ||
					bins.OrchHook != filepath.Join(tt.binDir, "orch-hook") ||
					bins.Orchd != filepath.Join(tt.binDir, "orchd") {
					t.Errorf("bins = %+v, want them under %q", bins, tt.binDir)
				}
				return
			}
			if !filepath.IsAbs(bins.Orch) || filepath.Base(bins.Orch) != "orch" {
				t.Errorf("bins.Orch = %q, want an absolute path ending in orch", bins.Orch)
			}
		})
	}
}

func TestSettingsLoad(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "loads defaults from an empty config dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			s, err := settings()
			if err != nil {
				t.Fatalf("settings: %v", err)
			}
			if s.RAG.Qdrant.Collection == "" {
				t.Errorf("expected the default collection to be set")
			}
		})
	}
}

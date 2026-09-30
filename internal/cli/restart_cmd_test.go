package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// errNoDaemon is the dial failure a stopped orchd produces.
var errNoDaemon = errors.New("dial unix: connect: no such file or directory")

func TestRestartSubsystemCmd(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		restart   *orchdv1.RestartResponse
		wantName  string
		wantOutIn []string
	}{
		{
			name:      "named subsystem restarts in place",
			args:      []string{"restart", "sources-scheduler"},
			restart:   &orchdv1.RestartResponse{Restarted: []string{"sources-scheduler"}},
			wantName:  "sources-scheduler",
			wantOutIn: []string{`restarted subsystem "sources-scheduler" in place`, "no process bounce"},
		},
		{
			name:      "subsystems restarts them all without a bounce",
			args:      []string{"restart", "subsystems"},
			restart:   &orchdv1.RestartResponse{Restarted: []string{"qdrant", "docs-scheduler"}},
			wantName:  "", // empty name means "every subsystem"
			wantOutIn: []string{"restarted 2 subsystem(s) in place", "qdrant, docs-scheduler"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			client := &fakeClient{restart: tt.restart}
			out, err := run(factory(client, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			if len(client.restartNames) != 1 || client.restartNames[0] != tt.wantName {
				t.Errorf("requested names = %v, want [%q]", client.restartNames, tt.wantName)
			}
			for _, want := range tt.wantOutIn {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestRestartSubsystemErrors(t *testing.T) {
	tests := []struct {
		name       string
		restartErr error
		dialErr    error
		wantErrHas string
	}{
		{
			name:       "unreachable daemon points at the whole-system restart",
			restartErr: grpcstatus.Error(codes.Unavailable, "connection refused"),
			wantErrHas: "orchd is not reachable",
		},
		{
			name:       "unknown subsystem keeps the daemon's list",
			restartErr: grpcstatus.Error(codes.NotFound, `unknown subsystem: "nope" (known subsystems: qdrant)`),
			wantErrHas: `unknown subsystem: "nope" (known subsystems: qdrant)`,
		},
		{
			name:       "a wedged subsystem reports the failure",
			restartErr: grpcstatus.Error(codes.Internal, "stop qdrant: still running"),
			wantErrHas: "restart failed: stop qdrant: still running",
		},
		{
			name:       "a non-status error passes through",
			restartErr: errors.New("plain failure"),
			wantErrHas: "plain failure",
		},
		{
			name:       "a dial failure surfaces",
			dialErr:    errNoDaemon,
			wantErrHas: "no such file or directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			client := &fakeClient{restartErr: tt.restartErr}
			_, err := run(factory(client, tt.dialErr), "", "restart", "qdrant")
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Errorf("error %q missing %q", err, tt.wantErrHas)
			}
		})
	}
}

func TestRestartProcessCmd(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		status     *orchdv1.StatusResponse
		statusErr  error
		wantOutIn  []string
		wantErrHas string
	}{
		{
			name: "bare restart bounces the process and waits for it",
			args: []string{"restart"},
			status: &orchdv1.StatusResponse{Version: "1.0.0", Subsystems: []*orchdv1.SubsystemStatus{
				{Name: "qdrant", State: supervisor.StateRunning},
				{Name: "docs-scheduler", State: supervisor.StateRunning},
			}},
			wantOutIn: []string{"restarted", "orchd 1.0.0 is back up", "2 subsystem(s) restarted with it"},
		},
		{
			name:      "explicit orchd component does the same",
			args:      []string{"restart", "orchd"},
			status:    &orchdv1.StatusResponse{Version: "1.0.0"},
			wantOutIn: []string{"orchd 1.0.0 is back up", "0 subsystem(s) restarted with it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			// Exercise the launchd path with a plist present and launchctl stubbed.
			plist := orchdPlistPath()
			if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
				t.Fatalf("write plist: %v", err)
			}
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return false }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })
			savedRun := runCommand
			runCommand = func(string, ...string) error { return nil }
			t.Cleanup(func() { runCommand = savedRun })

			client := &fakeClient{status: tt.status, statusErr: tt.statusErr}
			out, err := run(factory(client, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			for _, want := range tt.wantOutIn {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			if len(client.restartNames) != 0 {
				t.Errorf("a process restart must not call the Restart RPC, got %v", client.restartNames)
			}
		})
	}
}

func TestRestartProcessServiceManagerFailure(t *testing.T) {
	tests := []struct {
		name       string
		wantErrHas string
	}{
		{name: "a missing plist aborts before waiting", wantErrHas: "plist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t) // empty HOME → no plist
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return false }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })

			_, err := run(factory(&fakeClient{}, nil), "", "restart")
			if err == nil || !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Fatalf("got %v, want an error mentioning %q", err, tt.wantErrHas)
			}
		})
	}
}

func TestRestartListCmd(t *testing.T) {
	tests := []struct {
		name      string
		client    *fakeClient
		dialErr   error
		wantHas   []string
		wantNotIn []string
	}{
		{
			name: "lists the pseudo-components plus the live subsystems",
			client: &fakeClient{status: &orchdv1.StatusResponse{Subsystems: []*orchdv1.SubsystemStatus{
				{Name: "workflow-worker", Detail: "durable agent-task workflows", State: supervisor.StateRunning},
			}}},
			wantHas: []string{
				"orchd", "the daemon process (default)",
				"subsystems", "every subsystem, in place",
				"workflow-worker", "durable agent-task workflows (running)",
			},
		},
		{
			name:      "says why the subsystem list is missing when orchd is down",
			client:    &fakeClient{statusErr: errNoDaemon},
			wantHas:   []string{"orchd", "subsystems", "orchd is not running — start it to list its subsystems"},
			wantNotIn: []string{"workflow-worker"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			out, err := run(factory(tt.client, tt.dialErr), "", "restart", "--list")
			if err != nil {
				t.Fatalf("restart --list: %v", err)
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

func TestRestartListDialFailure(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "a dial failure is reported, not swallowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			if _, err := run(factory(&fakeClient{}, errNoDaemon), "", "restart", "--list"); err == nil {
				t.Fatal("expected a dial error")
			}
		})
	}
}

func TestRestartTooManyArgs(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "only one component is accepted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			if _, err := run(factory(&fakeClient{}, nil), "", "restart", "a", "b"); err == nil {
				t.Fatal("expected an argument-count error")
			}
		})
	}
}

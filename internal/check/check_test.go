package check

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAll(t *testing.T) {
	tests := []struct {
		name      string
		checks    []Check
		wantAllOK bool
	}{
		{
			name:      "all pass",
			checks:    []Check{ok("a"), ok("b")},
			wantAllOK: true,
		},
		{
			name:      "one fails",
			checks:    []Check{ok("a"), fail("b")},
			wantAllOK: false,
		},
		{
			name:      "empty passes",
			checks:    nil,
			wantAllOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, allOK := RunAll(tt.checks)
			if allOK != tt.wantAllOK {
				t.Errorf("allOK = %v, want %v", allOK, tt.wantAllOK)
			}
			if len(results) != len(tt.checks) {
				t.Errorf("results = %d, want %d", len(results), len(tt.checks))
			}
		})
	}
}

func TestPrint(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		wantSub string
	}{
		{name: "ok marker", results: []Result{{Name: "x", Detail: "d", OK: true}}, wantSub: "[OK  ] x"},
		{name: "fail marker", results: []Result{{Name: "y", Detail: "d", OK: false}}, wantSub: "[FAIL] y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			Print(&out, tt.results)
			if !strings.Contains(out.String(), tt.wantSub) {
				t.Errorf("output %q missing %q", out.String(), tt.wantSub)
			}
		})
	}
}

func TestFileCheck(t *testing.T) {
	tests := []struct {
		name    string
		present bool
		wantOK  bool
	}{
		{name: "present", present: true, wantOK: true},
		{name: "missing", present: false, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f")
			if tt.present {
				_ = os.WriteFile(path, []byte("x"), 0o644)
			}
			if got := FileCheck("f", path)(); got.OK != tt.wantOK {
				t.Errorf("FileCheck OK = %v, want %v", got.OK, tt.wantOK)
			}
		})
	}
}

func TestCommandCheck(t *testing.T) {
	tests := []struct {
		name   string
		argv   []string
		wantOK bool
	}{
		{name: "true exits zero", argv: []string{"true"}, wantOK: true},
		{name: "false exits nonzero", argv: []string{"false"}, wantOK: false},
		{name: "no command", argv: nil, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CommandCheck("c", tt.argv...)(); got.OK != tt.wantOK {
				t.Errorf("CommandCheck OK = %v, want %v", got.OK, tt.wantOK)
			}
		})
	}
}

func TestSocketCheckNotRunning(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "orchd down reports not OK"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_SOCKET", filepath.Join(t.TempDir(), "nope.sock"))
			if got := SocketCheck("orchd")(); got.OK {
				t.Errorf("expected not OK when orchd is down")
			}
		})
	}
}

func TestDefaultAssembles(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "returns the standard checks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			t.Setenv("ORCH_DATA_DIR", t.TempDir())
			if len(Default()) == 0 {
				t.Errorf("Default returned no checks")
			}
		})
	}
}

func ok(name string) Check   { return func() Result { return Result{Name: name, OK: true} } }
func fail(name string) Check { return func() Result { return Result{Name: name, OK: false} } }

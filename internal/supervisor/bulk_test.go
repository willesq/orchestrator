package supervisor

// Whole-fleet operations: StartAll, RestartAll and StopAll, including the
// ordering and error-aggregation guarantees each one makes.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestStartAllStopsAtFirstFailure(t *testing.T) {
	tests := []struct {
		name     string
		failOn   string
		wantRuns []string
		wantErr  string
	}{
		{name: "aborts after b fails", failOn: "b", wantRuns: []string{"a", "b"}, wantErr: "start b: bad"},
		{name: "all start when none fail", failOn: "", wantRuns: []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var ran []string
			s := New(context.Background())
			for _, name := range []string{"a", "b", "c"} {
				_ = s.Add(Unit{Name: name, Run: func(context.Context, func()) error {
					mu.Lock()
					ran = append(ran, name)
					mu.Unlock()
					if name == tt.failOn {
						return errors.New("bad")
					}
					return nil
				}})
			}
			err := s.StartAll()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("StartAll: %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("got %v, want %q", err, tt.wantErr)
			}
			if strings.Join(ran, ",") != strings.Join(tt.wantRuns, ",") {
				t.Errorf("ran %v, want %v", ran, tt.wantRuns)
			}
		})
	}
}

func TestRestartAllContinuesPastFailures(t *testing.T) {
	tests := []struct {
		name     string
		failOn   string
		wantRuns []string
		wantErr  string
	}{
		{name: "b fails, c still restarts", failOn: "b", wantRuns: []string{"a", "b", "c"}, wantErr: "start b: bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var ran []string
			s := New(context.Background())
			for _, name := range []string{"a", "b", "c"} {
				_ = s.Add(Unit{Name: name, Run: func(context.Context, func()) error {
					mu.Lock()
					ran = append(ran, name)
					mu.Unlock()
					if name == tt.failOn {
						return errors.New("bad")
					}
					return nil
				}})
			}
			err := s.RestartAll()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
			if strings.Join(ran, ",") != strings.Join(tt.wantRuns, ",") {
				t.Errorf("ran %v, want %v", ran, tt.wantRuns)
			}
			// The healthy units are running again; the broken one is marked failed.
			for _, st := range s.Status() {
				wantState := StateRunning
				if st.Name == tt.failOn {
					wantState = StateFailed
				}
				if st.State != wantState {
					t.Errorf("%s state = %q, want %q", st.Name, st.State, wantState)
				}
			}
		})
	}
}

func TestStopAllReverseOrder(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{name: "stops in reverse registration order", want: []string{"c", "b", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var stopped []string
			s := New(context.Background())
			for _, name := range []string{"a", "b", "c"} {
				_ = s.Add(Unit{Name: name, Run: noopRun, Stop: func() error {
					mu.Lock()
					stopped = append(stopped, name)
					mu.Unlock()
					return nil
				}})
			}
			if err := s.StartAll(); err != nil {
				t.Fatalf("StartAll: %v", err)
			}
			if err := s.StopAll(); err != nil {
				t.Fatalf("StopAll: %v", err)
			}
			if strings.Join(stopped, ",") != strings.Join(tt.want, ",") {
				t.Errorf("stopped %v, want %v", stopped, tt.want)
			}
		})
	}
}

func TestStopAllJoinsErrors(t *testing.T) {
	tests := []struct {
		name  string
		wants []string
	}{
		{name: "both failures reported", wants: []string{"stop a: a-bad", "stop b: b-bad"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			for _, name := range []string{"a", "b"} {
				_ = s.Add(Unit{Name: name, Run: noopRun, Stop: func() error { return errors.New(name + "-bad") }})
			}
			_ = s.StartAll()
			err := s.StopAll()
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tt.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

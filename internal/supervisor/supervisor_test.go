package supervisor

// Per-unit lifecycle: registration, starting, stopping, restarting, and the
// status snapshot one unit produces.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noopRun is a service unit that starts instantly and does nothing.
func noopRun(context.Context, func()) error { return nil }

// fixedClock returns a deterministic now func.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestAddValidation(t *testing.T) {
	tests := []struct {
		name    string
		units   []Unit
		wantErr string
	}{
		{
			name:    "empty name rejected",
			units:   []Unit{{Run: noopRun}},
			wantErr: "name is required",
		},
		{
			name:    "neither Run nor Tick rejected",
			units:   []Unit{{Name: "a"}},
			wantErr: "needs either Run or Tick",
		},
		{
			name: "both Run and Tick rejected",
			units: []Unit{{
				Name: "a", Run: noopRun,
				Tick: func(context.Context) {}, Interval: time.Second,
			}},
			wantErr: "sets both Run and Tick",
		},
		{
			name:    "periodic unit without an interval rejected",
			units:   []Unit{{Name: "a", Tick: func(context.Context) {}}},
			wantErr: "needs a positive Interval",
		},
		{
			name:    "periodic unit with a negative interval rejected",
			units:   []Unit{{Name: "a", Tick: func(context.Context) {}, Interval: -time.Second}},
			wantErr: "needs a positive Interval",
		},
		{
			name:    "duplicate name rejected",
			units:   []Unit{{Name: "a", Run: noopRun}, {Name: "a", Run: noopRun}},
			wantErr: "duplicate unit",
		},
		{
			name:  "distinct names accepted",
			units: []Unit{{Name: "a", Run: noopRun}, {Name: "b", Run: noopRun}},
		},
		{
			name:  "a periodic unit with an interval is accepted",
			units: []Unit{{Name: "p", Tick: func(context.Context) {}, Interval: time.Second}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			var err error
			for _, u := range tt.units {
				if e := s.Add(u); e != nil {
					err = e
				}
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Add: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestNamesPreservesRegistrationOrder(t *testing.T) {
	tests := []struct {
		name string
		add  []string
		want []string
	}{
		{name: "order preserved", add: []string{"qdrant", "worker", "sources"}, want: []string{"qdrant", "worker", "sources"}},
		{name: "empty supervisor", add: nil, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			for _, n := range tt.add {
				if err := s.Add(Unit{Name: n, Run: noopRun}); err != nil {
					t.Fatalf("Add %s: %v", n, err)
				}
			}
			got := s.Names()
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Names() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnknownUnitOperations(t *testing.T) {
	tests := []struct {
		name string
		op   func(s *Supervisor) error
	}{
		{name: "start", op: func(s *Supervisor) error { return s.Start("nope") }},
		{name: "stop", op: func(s *Supervisor) error { return s.Stop("nope") }},
		{name: "restart", op: func(s *Supervisor) error { return s.Restart("nope") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			err := tt.op(s)
			if !errors.Is(err, ErrUnknownUnit) {
				t.Fatalf("got %v, want ErrUnknownUnit", err)
			}
			if !strings.Contains(err.Error(), `"nope"`) {
				t.Errorf("error %q should name the unit", err)
			}
		})
	}
}

func TestStartIsIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		calls int
		want  int32
	}{
		{name: "second start is a no-op", calls: 3, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int32
			s := New(context.Background())
			if err := s.Add(Unit{Name: "a", Run: func(context.Context, func()) error {
				runs.Add(1)
				return nil
			}}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			for range tt.calls {
				if err := s.Start("a"); err != nil {
					t.Fatalf("Start: %v", err)
				}
			}
			if got := runs.Load(); got != tt.want {
				t.Errorf("Run called %d times, want %d", got, tt.want)
			}
			if st := s.Status()[0]; st.State != StateRunning {
				t.Errorf("state = %q, want running", st.State)
			}
		})
	}
}

func TestStartFailureMarksUnitFailed(t *testing.T) {
	tests := []struct {
		name    string
		runErr  error
		wantErr string
	}{
		{name: "run error surfaces", runErr: errors.New("boom"), wantErr: "start a: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			_ = s.Add(Unit{Name: "a", Run: func(context.Context, func()) error { return tt.runErr }})
			err := s.Start("a")
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("got %v, want %q", err, tt.wantErr)
			}
			st := s.Status()[0]
			if st.State != StateFailed || st.Err != "boom" {
				t.Errorf("status = %+v, want failed/boom", st)
			}
		})
	}
}

func TestStopCancelsContextAndRunsHook(t *testing.T) {
	tests := []struct {
		name     string
		stopErr  error
		wantErr  string
		wantStop int32
	}{
		{name: "clean stop", wantStop: 1},
		{name: "stop hook error", stopErr: errors.New("nope"), wantErr: "stop a: nope", wantStop: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stops atomic.Int32
			ctxDone := make(chan struct{})
			s := New(context.Background())
			_ = s.Add(Unit{
				Name: "a",
				Run: func(ctx context.Context, _ func()) error {
					go func() { <-ctx.Done(); close(ctxDone) }()
					return nil
				},
				Stop: func() error {
					stops.Add(1)
					return tt.stopErr
				},
			})
			if err := s.Start("a"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			err := s.Stop("a")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("got %v, want %q", err, tt.wantErr)
			}
			select {
			case <-ctxDone:
			case <-time.After(waitLimit):
				t.Fatal("unit context was not canceled")
			}
			// Stopping again must not re-run the hook.
			_ = s.Stop("a")
			if got := stops.Load(); got != tt.wantStop {
				t.Errorf("Stop hook ran %d times, want %d", got, tt.wantStop)
			}
		})
	}
}

func TestRestartCountsAndRestartsWork(t *testing.T) {
	tests := []struct {
		name         string
		restarts     int
		wantRuns     int32
		wantRestarts int
	}{
		{name: "two restarts", restarts: 2, wantRuns: 3, wantRestarts: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int32
			s := New(context.Background())
			_ = s.Add(Unit{Name: "a", Run: func(context.Context, func()) error {
				runs.Add(1)
				return nil
			}})
			if err := s.Start("a"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			for range tt.restarts {
				if err := s.Restart("a"); err != nil {
					t.Fatalf("Restart: %v", err)
				}
			}
			if got := runs.Load(); got != tt.wantRuns {
				t.Errorf("Run called %d times, want %d", got, tt.wantRuns)
			}
			st := s.Status()[0]
			if st.Restarts != tt.wantRestarts {
				t.Errorf("restarts = %d, want %d", st.Restarts, tt.wantRestarts)
			}
			if st.State != StateRunning {
				t.Errorf("state = %q, want running", st.State)
			}
		})
	}
}

func TestRestartStartsAStoppedUnit(t *testing.T) {
	tests := []struct {
		name string
		want int32
	}{
		{name: "restart of a never-started unit just starts it", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int32
			s := New(context.Background())
			_ = s.Add(Unit{Name: "a", Run: func(context.Context, func()) error {
				runs.Add(1)
				return nil
			}})
			if err := s.Restart("a"); err != nil {
				t.Fatalf("Restart: %v", err)
			}
			if got := runs.Load(); got != tt.want {
				t.Errorf("Run called %d times, want %d", got, tt.want)
			}
		})
	}
}

func TestStatusReportsTimestampsAndDetail(t *testing.T) {
	tests := []struct {
		name   string
		detail string
	}{
		{name: "detail, start time and heartbeat", detail: ":6333"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			s := New(context.Background())
			s.now = fixedClock(now)
			_ = s.Add(Unit{Name: "a", Detail: tt.detail, Run: func(_ context.Context, beat func()) error {
				beat()
				return nil
			}})
			if err := s.Start("a"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			st := s.Status()[0]
			if st.Detail != tt.detail {
				t.Errorf("detail = %q, want %q", st.Detail, tt.detail)
			}
			if !st.StartedAt.Equal(now) {
				t.Errorf("startedAt = %v, want %v", st.StartedAt, now)
			}
			if !st.LastBeat.Equal(now) {
				t.Errorf("lastBeat = %v, want %v", st.LastBeat, now)
			}
		})
	}
}

func TestStatusOfUnstartedUnit(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "registered but not started"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			_ = s.Add(Unit{Name: "a", Run: noopRun})
			st := s.Status()[0]
			if st.State != StateStopped || !st.StartedAt.IsZero() || !st.LastBeat.IsZero() || st.Restarts != 0 {
				t.Errorf("status = %+v, want a pristine stopped unit", st)
			}
		})
	}
}

func TestBeatOnUnknownUnitIsSafe(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "beat for a name that was never registered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			s.beat("never-registered") // must not panic
		})
	}
}

func TestStartAllContinuesPastADegradedUnit(t *testing.T) {
	tests := []struct {
		name      string
		firstErr  error
		wantErr   bool
		wantState string // of the unit registered after the failing one
	}{
		{name: "a degraded unit leaves the rest running", firstErr: Degraded(errors.New("boom")), wantState: StateRunning},
		{name: "a plain failure still stops StartAll", firstErr: errors.New("boom"), wantErr: true, wantState: StateStopped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(context.Background())
			_ = s.Add(Unit{Name: "a", Run: func(context.Context, func()) error { return tt.firstErr }})
			_ = s.Add(Unit{Name: "b", Run: noopRun})
			err := s.StartAll()
			if (err != nil) != tt.wantErr {
				t.Fatalf("StartAll err = %v, want error %v", err, tt.wantErr)
			}
			st := s.Status()
			if st[0].State != StateFailed || st[0].Err != "boom" {
				t.Errorf("a = %+v, want failed/boom", st[0])
			}
			if st[1].State != tt.wantState {
				t.Errorf("b state = %q, want %q", st[1].State, tt.wantState)
			}
		})
	}
}

func TestDegraded(t *testing.T) {
	if Degraded(nil) != nil {
		t.Errorf("Degraded(nil) is not nil")
	}
	base := errors.New("boom")
	err := Degraded(base)
	if !errors.Is(err, base) || err.Error() != "boom" {
		t.Errorf("Degraded(boom) = %v, want it to wrap boom with the same text", err)
	}
	if !IsDegraded(fmt.Errorf("start a: %w", err)) || IsDegraded(base) {
		t.Errorf("IsDegraded does not see through wrapping or misreports a plain error")
	}
}

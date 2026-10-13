package supervisor

// Periodic units: the supervisor-owned ticker loop, the join-on-stop guarantee,
// and context propagation from the supervisor's root.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeriodicTicksImmediatelyAndBeats(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "first tick is immediate (catch-up)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ticked := make(chan struct{}, 1)
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			s := New(context.Background())
			s.now = fixedClock(now)
			if err := s.Add(Unit{
				Name:     "p",
				Interval: time.Hour,
				Tick:     func(context.Context) { ticked <- struct{}{} },
			}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if err := s.Start("p"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			select {
			case <-ticked:
			case <-time.After(waitLimit):
				t.Fatal("periodic unit did not tick immediately")
			}
			// The supervisor records the heartbeat itself, per tick.
			deadline := time.Now().Add(waitLimit)
			for time.Now().Before(deadline) {
				if st := s.Status()[0]; st.LastBeat.Equal(now) {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Errorf("no heartbeat recorded, status = %+v", s.Status()[0])
		})
	}
}

func TestPeriodicStopsOnContextCancel(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "canceled context ends the loop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ticks atomic.Int32
			s := New(context.Background())
			done := make(chan struct{})
			_ = s.Add(Unit{Name: "p", Interval: time.Millisecond, Tick: func(context.Context) {
				if ticks.Add(1) == 1 {
					close(done)
				}
			}})
			if err := s.Start("p"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-done
			if err := s.Stop("p"); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			// Stop joins the loop, so the count is frozen the moment it returns.
			settled := ticks.Load()
			time.Sleep(50 * time.Millisecond)
			if got := ticks.Load(); got != settled {
				t.Errorf("ticks advanced from %d to %d after Stop returned", settled, got)
			}
		})
	}
}

// TestRestartJoinsTheLoopBeforeStartingAnother is the regression test for the
// overlap bug: without a join, restarting mid-tick left two loops running, and
// the schedulers are not safe to run concurrently with themselves.
func TestRestartJoinsTheLoopBeforeStartingAnother(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "a restart never overlaps two ticks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var concurrent, maxConcurrent, ticks atomic.Int32
			inTick := make(chan struct{}, 1)
			release := make(chan struct{})

			s := New(context.Background())
			_ = s.Add(Unit{Name: "p", Interval: time.Millisecond, Tick: func(ctx context.Context) {
				n := concurrent.Add(1)
				defer concurrent.Add(-1)
				for {
					old := maxConcurrent.Load()
					if n <= old || maxConcurrent.CompareAndSwap(old, n) {
						break
					}
				}
				if ticks.Add(1) == 1 {
					inTick <- struct{}{} // the first tick blocks until released
					<-release
				}
			}})
			if err := s.Start("p"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-inTick // a tick is now in flight

			restarted := make(chan error, 1)
			go func() { restarted <- s.Restart("p") }()

			// The restart must still be blocked: the first tick holds the loop.
			select {
			case err := <-restarted:
				t.Fatalf("Restart returned while a tick was in flight: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			close(release)
			select {
			case err := <-restarted:
				if err != nil {
					t.Fatalf("Restart: %v", err)
				}
			case <-time.After(waitLimit):
				t.Fatal("Restart never completed after the tick finished")
			}

			time.Sleep(20 * time.Millisecond)
			_ = s.Stop("p")
			if got := maxConcurrent.Load(); got > 1 {
				t.Errorf("observed %d concurrent ticks, want at most 1", got)
			}
		})
	}
}

func TestStopTimesOutOnAWedgedTick(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string
	}{
		{name: "a tick that ignores cancellation fails the unit", wantErr: "still running after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) }) // let the wedged tick exit
			entered := make(chan struct{})

			s := New(context.Background())
			s.stopTimeout = 50 * time.Millisecond
			_ = s.Add(Unit{Name: "wedged", Interval: time.Millisecond, Tick: func(context.Context) {
				close(entered)
				<-release // deliberately ignores ctx
			}})
			if err := s.Start("wedged"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-entered // the tick must be in flight before we try to stop it

			err := s.Stop("wedged")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
			}
			if st := s.Status()[0]; st.State != StateFailed {
				t.Errorf("state = %q, want failed", st.State)
			}
		})
	}
}

func TestRootCancelStopsUnitWork(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "canceling the root context cancels unit contexts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			s := New(ctx)
			done := make(chan struct{})
			_ = s.Add(Unit{Name: "a", Run: func(uctx context.Context, _ func()) error {
				go func() { <-uctx.Done(); close(done) }()
				return nil
			}})
			if err := s.Start("a"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(waitLimit):
				t.Fatal("unit context was not canceled with the root")
			}
		})
	}
}

func TestRootCancelStopsPeriodicLoop(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "canceling the root ends periodic loops too"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var ticks atomic.Int32
			s := New(ctx)
			_ = s.Add(Unit{Name: "p", Interval: time.Millisecond, Tick: func(context.Context) {
				ticks.Add(1)
			}})
			if err := s.Start("p"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			s.mu.Lock()
			done := s.units["p"].done
			s.mu.Unlock()
			cancel()
			// Wait for the loop itself to exit rather than for a fixed time, so a
			// slow machine cannot fail this; then confirm nothing ticks after it.
			select {
			case <-done:
			case <-time.After(waitLimit):
				t.Fatal("periodic loop kept running after the root was canceled")
			}
			settled := ticks.Load()
			time.Sleep(50 * time.Millisecond)
			if got := ticks.Load(); got != settled {
				t.Errorf("ticks advanced from %d to %d after the loop exited", settled, got)
			}
		})
	}
}

// waitLimit bounds how long a test waits for something it expects to happen.
// The waits return as soon as the condition holds, so the limit costs nothing
// on a healthy run and is deliberately generous: only a real hang reaches it,
// never a slow or loaded machine.
const waitLimit = 30 * time.Second

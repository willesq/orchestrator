// Package supervisor runs orchd's long-lived subsystems as independently
// restartable units. Each unit gets its own child context, so `orch restart
// <name>` can stop and start one subsystem without bouncing the orchd process
// (and without dropping the socket, the warm embedder, or the other units).
//
// Units come in two shapes. A service unit (Run) starts something that keeps
// running until its context is canceled — the qdrant engine, the workflow
// worker. A periodic unit (Tick + Interval) is a ticker loop the supervisor
// itself owns, which is what lets it record the heartbeat `orch status` reports
// as "last tick" and join the loop before a restart starts a second copy.
//
// Every lifecycle operation is idempotent: starting a running unit, stopping a
// stopped one, and restarting either are all safe to repeat. Stopping waits for
// the unit to actually be down, so a restart never overlaps two copies of work
// that is unsafe to run concurrently with itself.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Unit lifecycle states, as reported by Status and printed by `orch status`.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateFailed  = "failed"
)

// ErrUnknownUnit is returned for a name that was never registered. Callers use
// errors.Is to map it onto a NOT_FOUND response.
var ErrUnknownUnit = errors.New("unknown subsystem")

// Unit is one independently restartable subsystem. It is either a *service*
// unit (set Run) or a *periodic* unit (set Tick + Interval), never both.
type Unit struct {
	// Name is the stable identifier `orch restart <name>` accepts.
	Name string
	// Detail is a static human-readable descriptor, e.g. the qdrant port.
	Detail string
	// Run starts a service unit's work and must return promptly: anything
	// long-lived belongs in a goroutine that exits when ctx is canceled. beat
	// records a heartbeat, surfaced as the unit's last tick.
	Run func(ctx context.Context, beat func()) error
	// Tick is a periodic unit's body. The supervisor owns the ticker loop, so it
	// records the heartbeat itself and — crucially — can join the loop before a
	// restart brings a second copy up. Tick should honour ctx; a restart waits
	// for an in-flight Tick to return.
	Tick func(ctx context.Context)
	// Interval is Tick's cadence. Required with Tick, must be positive.
	Interval time.Duration
	// Stop releases what the unit acquired. Optional; called after the unit's
	// context is canceled and after its loop has been joined, and must tolerate
	// being called on an already-stopped unit.
	Stop func() error
}

// Status is a point-in-time view of one unit.
type Status struct {
	Name      string
	Detail    string
	State     string
	Err       string
	StartedAt time.Time
	LastBeat  time.Time
	Restarts  int
}

type unit struct {
	spec      Unit
	cancel    context.CancelFunc
	done      chan struct{} // closed when a periodic unit's loop has exited
	state     string
	err       string
	startedAt time.Time
	lastBeat  time.Time
	restarts  int
}

// DefaultStopTimeout bounds how long stopping a unit waits for its loop to
// exit. A tick that outlives it leaves the unit failed rather than letting a
// restart stack a second copy on top of the first.
const DefaultStopTimeout = 15 * time.Second

// Supervisor owns a set of named units under a root context. Canceling the root
// context stops every unit's work; StopAll additionally runs their Stop hooks.
type Supervisor struct {
	root context.Context

	// lifecycle serializes start/stop/restart so two concurrent restarts of the
	// same unit cannot interleave. It is never held while mu is contended by a
	// heartbeat, and user code (Run/Stop) is never called while holding mu.
	lifecycle sync.Mutex

	mu    sync.Mutex
	order []string
	units map[string]*unit
	now   func() time.Time

	// stopTimeout bounds the wait for a unit's loop to exit; see DefaultStopTimeout.
	stopTimeout time.Duration
}

// New builds a supervisor whose units all derive from root.
func New(root context.Context) *Supervisor {
	return &Supervisor{
		root:        root,
		units:       make(map[string]*unit),
		now:         time.Now,
		stopTimeout: DefaultStopTimeout,
	}
}

// Add registers a unit without starting it.
func (s *Supervisor) Add(spec Unit) error {
	if spec.Name == "" {
		return errors.New("supervisor: unit name is required")
	}
	switch {
	case spec.Run == nil && spec.Tick == nil:
		return fmt.Errorf("supervisor: unit %q needs either Run or Tick", spec.Name)
	case spec.Run != nil && spec.Tick != nil:
		return fmt.Errorf("supervisor: unit %q sets both Run and Tick", spec.Name)
	case spec.Tick != nil && spec.Interval <= 0:
		return fmt.Errorf("supervisor: periodic unit %q needs a positive Interval", spec.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.units[spec.Name]; dup {
		return fmt.Errorf("supervisor: duplicate unit %q", spec.Name)
	}
	s.units[spec.Name] = &unit{spec: spec, state: StateStopped}
	s.order = append(s.order, spec.Name)
	return nil
}

// Names lists registered units in registration (start) order.
func (s *Supervisor) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// Status snapshots every unit in registration order.
func (s *Supervisor) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.order))
	for _, name := range s.order {
		u := s.units[name]
		out = append(out, Status{
			Name:      name,
			Detail:    u.spec.Detail,
			State:     u.state,
			Err:       u.err,
			StartedAt: u.startedAt,
			LastBeat:  u.lastBeat,
			Restarts:  u.restarts,
		})
	}
	return out
}

// Start starts one unit. Starting an already-running unit is a no-op.
func (s *Supervisor) Start(name string) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	return s.start(name)
}

// StartAll starts every unit in registration order, stopping at the first
// failure so a broken dependency does not bring up half a system silently. A
// unit whose Run returns a Degraded error is the exception: it is left failed,
// visible in Status, and the remaining units start.
func (s *Supervisor) StartAll() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	for _, name := range s.Names() {
		if err := s.start(name); err != nil && !IsDegraded(err) {
			return err
		}
	}
	return nil
}

// degradedError marks a Run failure the rest of the system can run without.
type degradedError struct{ err error }

func (d degradedError) Error() string { return d.err.Error() }
func (d degradedError) Unwrap() error { return d.err }

// Degraded wraps a Run error so StartAll leaves that unit failed and starts the
// others, for a unit whose loss disables one feature rather than the whole
// system. It keeps err's text and chain; Degraded(nil) is nil. Never use it for
// a unit that enforces security, such as authorization: its failure would then
// leave the system running without it.
func Degraded(err error) error {
	if err == nil {
		return nil
	}
	return degradedError{err: err}
}

// IsDegraded reports whether err, or anything it wraps, came from Degraded.
func IsDegraded(err error) bool {
	var d degradedError
	return errors.As(err, &d)
}

// Stop cancels one unit and runs its Stop hook. Stopping a stopped unit is a no-op.
func (s *Supervisor) Stop(name string) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	return s.stop(name)
}

// StopAll stops every unit in reverse registration order (shutdown path: errors
// are reported but never abort the sweep).
func (s *Supervisor) StopAll() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	names := s.Names()
	var errs []error
	for i := len(names) - 1; i >= 0; i-- {
		if err := s.stop(names[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Restart stops then starts one unit, counting a completed restart.
func (s *Supervisor) Restart(name string) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	return s.restart(name)
}

// RestartAll restarts every unit in registration order. Unlike StartAll it does
// not stop at the first failure: one wedged subsystem should not prevent the
// others from coming back.
func (s *Supervisor) RestartAll() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	var errs []error
	for _, name := range s.Names() {
		if err := s.restart(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// restart assumes s.lifecycle is held.
func (s *Supervisor) restart(name string) error {
	if err := s.stop(name); err != nil {
		return err
	}
	if err := s.start(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.units[name].restarts++
	return nil
}

// start assumes s.lifecycle is held.
func (s *Supervisor) start(name string) error {
	u, err := s.lookup(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	running := u.state == StateRunning
	s.mu.Unlock()
	if running {
		return nil
	}

	ctx, cancel := context.WithCancel(s.root)
	var done chan struct{}
	if u.spec.Tick != nil {
		// The supervisor owns periodic loops so that stop can join them.
		done = make(chan struct{})
		go s.loop(ctx, done, name, u.spec)
	} else if err := u.spec.Run(ctx, func() { s.beat(name) }); err != nil {
		cancel()
		s.mu.Lock()
		u.state, u.err = StateFailed, err.Error()
		s.mu.Unlock()
		return fmt.Errorf("start %s: %w", name, err)
	}
	s.mu.Lock()
	u.cancel, u.done, u.state, u.err, u.startedAt = cancel, done, StateRunning, "", s.now()
	s.mu.Unlock()
	return nil
}

// loop drives a periodic unit: tick immediately (catch-up after a restart or a
// laptop sleep), then every Interval until the unit's context is canceled.
// Closing done on exit is what lets stop join the loop before a restart brings
// a second copy up.
func (s *Supervisor) loop(ctx context.Context, done chan struct{}, name string, spec Unit) {
	defer close(done)
	t := time.NewTicker(spec.Interval)
	defer t.Stop()
	for ctx.Err() == nil {
		s.beat(name)
		spec.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// stop assumes s.lifecycle is held.
func (s *Supervisor) stop(name string) error {
	u, err := s.lookup(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cancel, done, running := u.cancel, u.done, u.state == StateRunning
	u.cancel, u.done = nil, nil
	if running {
		u.state = StateStopped
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !running {
		return nil
	}
	// Join the unit's loop before returning: a restart that skipped this would
	// start a second copy alongside an in-flight tick, and the schedulers are
	// not safe to run concurrently with themselves (shared repo cache, same
	// registry rows, same chunks re-ingested).
	if done != nil {
		timer := time.NewTimer(s.stopTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			err := fmt.Errorf("stop %s: still running after %s", name, s.stopTimeout)
			s.mu.Lock()
			u.state, u.err = StateFailed, err.Error()
			s.mu.Unlock()
			return err
		}
	}
	if u.spec.Stop == nil {
		return nil
	}
	if err := u.spec.Stop(); err != nil {
		s.mu.Lock()
		u.state, u.err = StateFailed, err.Error()
		s.mu.Unlock()
		return fmt.Errorf("stop %s: %w", name, err)
	}
	return nil
}

func (s *Supervisor) lookup(name string) (*unit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.units[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownUnit, name)
	}
	return u, nil
}

func (s *Supervisor) beat(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.units[name]; ok {
		u.lastBeat = s.now()
	}
}

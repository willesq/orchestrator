package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Session pins (SP-010) fix the identity every later decision about a
// session uses: the harness process, the first directory and its space. A
// pin is written once at SessionStart and only ended_at changes after that
// (INV-05). See docs/internals/daemon.md "Session pins".

// sessionIDPattern is the session IDs orchd accepts. Both harnesses use
// UUIDs, and the bound keeps an ID safe in log lines and file names.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// pinPlaceTimeout bounds placing a new pin, which can ask GitHub about the
// checkout's repo.
const pinPlaceTimeout = 10 * time.Second

// sessionPin is one row of session_pins.
type sessionPin struct {
	SessionID     string
	Harness       string
	Process       procstat.Identity
	FirstCwd      string
	MainCheckout  string
	Space         string
	Revision      int64
	PinnedAt      time.Time
	EndedAt       time.Time // zero while not ended
	InheritedFrom string
}

func (p sessionPin) proto() *orchdv1.SessionPin {
	out := &orchdv1.SessionPin{
		SessionId: p.SessionID, Harness: p.Harness,
		HarnessPid: int64(p.Process.PID), HarnessStarted: p.Process.Started.UnixNano(),
		FirstCwd: p.FirstCwd, MainCheckout: p.MainCheckout, Space: p.Space,
		PolicyRevision: p.Revision, PinnedAt: p.PinnedAt.Unix(), InheritedFrom: p.InheritedFrom,
	}
	if !p.EndedAt.IsZero() {
		out.EndedAt = p.EndedAt.Unix()
	}
	return out
}

// pinRegistry stores session pins in orch.db.
type pinRegistry struct {
	db  *sql.DB
	now func() time.Time
	// started and cwd read a process's start time and working directory
	// from the kernel; tests replace them with a fake process table.
	started func(pid int) (time.Time, error)
	cwd     func(pid int) (string, error)
	// mu makes each pin decision and its write one step, so two SessionStart
	// hooks of one harness process cannot both create a pin.
	mu sync.Mutex
}

func newPinRegistry(db *sql.DB) *pinRegistry {
	return &pinRegistry{db: db, now: time.Now, started: procstat.StartTime, cwd: procstat.Cwd}
}

// placeFunc decides the space and policy revision of a new pin's directory.
type placeFunc func(ctx context.Context, cwd string) (space string, revision int64, err error)

var (
	errPinnedElsewhere = errors.New("the session is pinned to another harness process")
	errParentElsewhere = errors.New("the parent session is pinned to another harness process")
	errParentUnpinned  = errors.New("the parent session has no pin")
	errCwdMismatch     = errors.New("the directory is not the harness process's working directory")
	errNoPin           = errors.New("the session has no pin")
	errHarnessGone     = errors.New("the harness process has exited")
	errOuterUnpinned   = errors.New("the harness runs inside another session that has no pin")
	errPinTooLate      = errors.New("the harness process started too long ago to take a new pin")
	errOrphanHarness   = errors.New("the harness process was detached from the process that started it")
)

// newPinWindow is how long after its harness process starts a session may
// take a new pin. SessionStart fires as the harness starts; later, an agent
// might have moved the harness's working directory (see verifyCwd).
const newPinWindow = 5 * time.Minute

// pin returns id's pin, creating it when there is none. The rules, in order:
// an existing pin is returned unchanged; a subagent copies its parent's pin;
// a new session in a harness process that already holds a pin (Claude Code's
// /clear) copies that one; a harness started inside another session copies
// the nearest outer harness's pin, and gets none when that session has none;
// otherwise cwd, which must be the harness process's own working directory,
// is pinned while the harness is new. A pin is only ever returned to the
// harness process it names.
func (r *pinRegistry) pin(ctx context.Context, id, parent string, c harnessCaller, cwd string, place placeFunc) (sessionPin, error) {
	harness, proc := c.name, c.proc
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok, err := r.get(ctx, id); err != nil || ok {
		if err == nil && !existing.Process.Same(proc) {
			err = errPinnedElsewhere
		}
		return existing, err
	}
	if parent != "" {
		pp, ok, err := r.get(ctx, parent)
		switch {
		case err != nil:
			return sessionPin{}, err
		case !ok:
			return sessionPin{}, errParentUnpinned
		case !pp.Process.Same(proc):
			return sessionPin{}, errParentElsewhere
		}
		return r.insertCopy(ctx, id, pp)
	}
	if earlier, ok, err := r.firstOf(ctx, proc); err != nil || ok {
		if err != nil {
			return sessionPin{}, err
		}
		return r.insertCopy(ctx, id, earlier)
	}
	// Reason: a harness an agent starts in another directory, or any
	// program it names claude, is nested in the agent's own session. It
	// keeps that session's space rather than choosing one by its cwd.
	for _, outer := range c.outer {
		if p, ok, err := r.firstOf(ctx, outer); err != nil || ok {
			if err != nil {
				return sessionPin{}, err
			}
			p.Harness, p.Process = harness, proc
			return r.insertCopy(ctx, id, p)
		}
	}
	if len(c.outer) > 0 {
		return sessionPin{}, errOuterUnpinned
	}
	// Reason: a process an agent detaches from its session (a backgrounded
	// subshell that exits) is reparented to launchd and has no harness above
	// it. A real session's harness runs under a shell, terminal or editor.
	// Accepted risk (2026-11-08): a detached shell kept alive above the fake
	// harness still passes; see docs/internals/daemon.md, Session pins.
	if c.orphan {
		return sessionPin{}, errOrphanHarness
	}
	if r.now().Sub(proc.Started) > newPinWindow {
		return sessionPin{}, errPinTooLate
	}
	// Reason: the hook's cwd is only a report. Checking it against the
	// kernel's view of the harness process keeps a process inside a harness
	// that started without a pin from choosing its own space.
	first, err := r.verifyCwd(proc.PID, cwd)
	if err != nil {
		return sessionPin{}, err
	}
	space, rev, err := place(ctx, first)
	if err != nil {
		return sessionPin{}, err
	}
	p := sessionPin{
		SessionID: id, Harness: harness, Process: proc, FirstCwd: first,
		MainCheckout: mainCheckout(first), Space: space, Revision: rev, PinnedAt: r.now().UTC(),
	}
	return p, r.insert(ctx, p)
}

// verifyCwd returns cwd when it is pid's working directory, comparing the
// two with symlinks resolved.
func (r *pinRegistry) verifyCwd(pid int, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", errCwdMismatch
	}
	want, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return "", errCwdMismatch
	}
	got, err := r.cwd(pid)
	if err != nil {
		return "", fmt.Errorf("working directory of harness process %d: %w", pid, err)
	}
	if real, err := filepath.EvalSymlinks(got); err == nil {
		got = real
	}
	if got != want {
		return "", errCwdMismatch
	}
	return want, nil
}

// mainCheckout is the main checkout of the repository holding dir, "" when
// there is none.
func mainCheckout(dir string) string {
	c, ok := handoff.CheckoutAt(dir)
	if !ok {
		c, _ = handoff.FindCheckout(dir)
	}
	return c.Main
}

// end records that id ended, keeping the first end recorded.
func (r *pinRegistry) end(ctx context.Context, id string, proc procstat.Identity) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok, err := r.get(ctx, id)
	switch {
	case err != nil:
		return time.Time{}, err
	case !ok:
		return time.Time{}, errNoPin
	case !p.Process.Same(proc):
		return time.Time{}, errPinnedElsewhere
	case !p.EndedAt.IsZero():
		return p.EndedAt, nil
	}
	now := r.now().UTC()
	if _, err := r.db.ExecContext(ctx,
		`UPDATE session_pins SET ended_at = ? WHERE session_id = ? AND ended_at IS NULL`,
		now.Format(time.RFC3339Nano), id); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

const pinColumns = `session_id, harness, harness_pid, harness_started, first_cwd, main_checkout,
	space, policy_revision, pinned_at, ended_at, inherited_from`

func (r *pinRegistry) get(ctx context.Context, id string) (sessionPin, bool, error) {
	return scanPin(r.db.QueryRowContext(ctx, `SELECT `+pinColumns+` FROM session_pins WHERE session_id = ?`, id))
}

// firstOf returns the earliest pin of the harness process proc.
func (r *pinRegistry) firstOf(ctx context.Context, proc procstat.Identity) (sessionPin, bool, error) {
	return scanPin(r.db.QueryRowContext(ctx, `SELECT `+pinColumns+` FROM session_pins
		WHERE harness_pid = ? AND harness_started = ? ORDER BY rowid LIMIT 1`,
		proc.PID, proc.Started.UnixNano()))
}

func scanPin(row *sql.Row) (sessionPin, bool, error) {
	var (
		p        sessionPin
		pid      int
		started  int64
		pinnedAt string
		endedAt  sql.NullString
	)
	err := row.Scan(&p.SessionID, &p.Harness, &pid, &started, &p.FirstCwd, &p.MainCheckout,
		&p.Space, &p.Revision, &pinnedAt, &endedAt, &p.InheritedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionPin{}, false, nil
	}
	if err != nil {
		return sessionPin{}, false, err
	}
	p.Process = procstat.Identity{PID: pid, Started: time.Unix(0, started)}
	if p.PinnedAt, err = time.Parse(time.RFC3339Nano, pinnedAt); err != nil {
		return sessionPin{}, false, fmt.Errorf("pin %s: pinned_at: %w", p.SessionID, err)
	}
	if endedAt.Valid {
		if p.EndedAt, err = time.Parse(time.RFC3339Nano, endedAt.String); err != nil {
			return sessionPin{}, false, fmt.Errorf("pin %s: ended_at: %w", p.SessionID, err)
		}
	}
	return p, true, nil
}

// insertCopy pins id to everything from's pin fixes.
func (r *pinRegistry) insertCopy(ctx context.Context, id string, from sessionPin) (sessionPin, error) {
	p := from
	p.SessionID, p.PinnedAt, p.EndedAt = id, r.now().UTC(), time.Time{}
	if p.InheritedFrom == "" {
		p.InheritedFrom = from.SessionID
	}
	return p, r.insert(ctx, p)
}

func (r *pinRegistry) insert(ctx context.Context, p sessionPin) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO session_pins (`+pinColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		p.SessionID, p.Harness, p.Process.PID, p.Process.Started.UnixNano(), p.FirstCwd, p.MainCheckout,
		p.Space, p.Revision, p.PinnedAt.Format(time.RFC3339Nano), p.InheritedFrom)
	return err
}

// harnessCaller is the harness process a caller runs in, and the harness
// processes above it, nearest first.
type harnessCaller struct {
	name  string
	proc  procstat.Identity
	outer []procstat.Identity
	// orphan: the harness's parent is launchd or init.
	orphan bool
}

// harnessCaller returns the harness process ctx's caller runs in. Only a
// process inside a harness, whose start time orchd read when it accepted the
// connection and which still runs, has one. The error is logged, never
// returned to the caller, since it can name other processes.
func (r *pinRegistry) harnessCaller(ctx context.Context) (harnessCaller, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return harnessCaller{}, errors.New("unidentified process")
	}
	a, ok := p.AuthInfo.(callerAuth)
	switch {
	case !ok:
		return harnessCaller{}, errors.New("unidentified process")
	case a.err != nil:
		return harnessCaller{}, a.err
	case a.harnessErr != nil:
		return harnessCaller{}, a.harnessErr
	case a.harness == "" || a.harnessProc.PID <= 0 || a.harnessProc.Started.IsZero():
		return harnessCaller{}, errors.New("the caller does not run inside a claude or codex session")
	}
	now, err := r.started(a.harnessProc.PID)
	if err != nil || !now.Equal(a.harnessProc.Started) {
		return harnessCaller{}, errHarnessGone
	}
	return harnessCaller{name: a.harness, proc: a.harnessProc, outer: a.outerHarnesses, orphan: a.harnessOrphan}, nil
}

func (s *server) PinSession(ctx context.Context, req *orchdv1.PinSessionRequest) (*orchdv1.SessionPin, error) {
	if s.pins == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "session pins are unavailable")
	}
	id, parent := req.GetSessionId(), req.GetParentSessionId()
	if !sessionIDPattern.MatchString(id) || (parent != "" && !sessionIDPattern.MatchString(parent)) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session IDs are 1 to 128 letters, digits, '-' or '_'")
	}
	caller, err := s.pins.harnessCaller(ctx)
	if err != nil {
		log.Printf("orchd: PinSession %s refused: %v", id, err)
		return nil, grpcstatus.Error(codes.PermissionDenied, "only a hook inside a claude or codex session can pin it")
	}
	// Reason: a hook that gives up waiting must not cost the session its
	// pin, so placement runs to its own deadline.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pinPlaceTimeout)
	defer cancel()
	p, err := s.pins.pin(pctx, id, parent, caller, req.GetCwd(), s.placeSession)
	if err != nil {
		log.Printf("orchd: PinSession %s: %v", id, err)
		return nil, pinStatus(err)
	}
	return p.proto(), nil
}

func (s *server) EndSession(ctx context.Context, req *orchdv1.EndSessionRequest) (*orchdv1.EndSessionResponse, error) {
	if s.pins == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "session pins are unavailable")
	}
	id := req.GetSessionId()
	if !sessionIDPattern.MatchString(id) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session IDs are 1 to 128 letters, digits, '-' or '_'")
	}
	caller, err := s.pins.harnessCaller(ctx)
	if err != nil {
		log.Printf("orchd: EndSession %s refused: %v", id, err)
		return nil, grpcstatus.Error(codes.PermissionDenied, "only a hook inside the pinned session can end it")
	}
	at, err := s.pins.end(ctx, id, caller.proc)
	if err != nil {
		log.Printf("orchd: EndSession %s: %v", id, err)
		return nil, pinStatus(err)
	}
	return &orchdv1.EndSessionResponse{EndedAt: at.Unix()}, nil
}

// pinStatus maps a pin error to the status a hook sees.
func pinStatus(err error) error {
	switch {
	case errors.Is(err, errPinnedElsewhere), errors.Is(err, errParentElsewhere):
		return grpcstatus.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, errNoPin):
		return grpcstatus.Error(codes.NotFound, err.Error())
	case errors.Is(err, errParentUnpinned), errors.Is(err, errCwdMismatch),
		errors.Is(err, errOuterUnpinned), errors.Is(err, errPinTooLate), errors.Is(err, errOrphanHarness):
		return grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	return grpcstatus.Error(codes.Internal, "the session pin could not be recorded")
}

// placeSession is the space a session started in cwd writes to, under the
// policy applied now. With no policy it is "" (INV-10); when the applied
// policy does not verify it is quarantine, so the session reads nothing.
func (s *server) placeSession(ctx context.Context, cwd string) (string, int64, error) {
	if s.spaces == nil {
		return "", 0, nil
	}
	res, rev, err := s.spaces.CurrentRevision()
	switch {
	case errors.Is(err, spaces.ErrBroken):
		return spaces.Quarantine, 0, nil
	case err != nil:
		return "", 0, err
	case !res.Configured():
		return "", rev, nil
	}
	o, _, err := s.placementOrigin(ctx, cwd)
	if err != nil {
		return "", 0, err
	}
	o.System = spaces.SystemSession
	p, err := s.spaces.PlaceUnder(ctx, res, o)
	if err != nil {
		return "", 0, err
	}
	return p.Space, rev, nil
}

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
// pin is written at SessionStart. Its space and first directory never change
// (INV-05); a resume moves it to a new harness process and ended_at records
// the end (SP-017). See docs/internals/daemon.md "Session pins".

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
	ResumedAt     time.Time // zero for a session never resumed
	// ExitedAt is when orchd first saw the harness process gone; zero
	// while it runs.
	ExitedAt time.Time
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
	// procs and program read the process table and a process's program,
	// to find harness processes with no pin.
	procs   func() ([]procstat.Process, error)
	program func(pid int) (procstat.Program, error)
	// mu makes each pin decision and its write one step, so two SessionStart
	// hooks of one harness process cannot both create a pin.
	mu sync.Mutex
}

func newPinRegistry(db *sql.DB) *pinRegistry {
	return &pinRegistry{db: db, now: time.Now, started: procstat.StartTime, cwd: procstat.Cwd,
		procs: procstat.Processes, program: procstat.ReadProgram}
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
	errResumeSpace     = errors.New("the session was resumed where it would have another space")
	errResumeHere      = errors.New("only a newly started harness that is not inside another session can resume a session from another process")
)

// newPinWindow is how long after its harness process starts a session may
// take a new pin. SessionStart fires as the harness starts; later, an agent
// might have moved the harness's working directory (see verifyCwd).
const newPinWindow = 5 * time.Minute

// pin returns id's pin, creating it when there is none. The rules, in order:
// an existing pin is returned unchanged, or moved to this harness process
// when source is "resume" (see resume); a subagent copies its parent's pin;
// otherwise the session gets newPin's pin. A pin is only ever returned to the
// harness process it names.
func (r *pinRegistry) pin(ctx context.Context, id, parent, source string, c harnessCaller, cwd string, place placeFunc) (sessionPin, error) {
	proc := c.proc
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok, err := r.get(ctx, id); err != nil || ok {
		switch {
		case err != nil:
		case source == "resume":
			return r.resume(ctx, existing, c, cwd, place)
		case !existing.Process.Same(proc):
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
	p, copied, err := r.newPin(ctx, c, cwd, place)
	if err != nil {
		return sessionPin{}, err
	}
	if copied {
		return r.insertCopy(ctx, id, p)
	}
	p.SessionID = id
	return p, r.insert(ctx, p)
}

// newPin is the pin a new session in harness process c gets. A harness
// process that already holds a pin (Claude Code's /clear) gives a copy of
// that one; a harness started inside another session copies the nearest
// outer harness's pin, and gets none when that session has none; otherwise
// cwd, which must be the harness process's own working directory, is placed
// while the harness is new. copied reports a copy; the pin's SessionID is
// left for the caller.
func (r *pinRegistry) newPin(ctx context.Context, c harnessCaller, cwd string, place placeFunc) (p sessionPin, copied bool, err error) {
	harness, proc := c.name, c.proc
	if earlier, ok, err := r.firstOf(ctx, proc); err != nil || ok {
		return earlier, ok, err
	}
	// Reason: a harness an agent starts in another directory, or any
	// program it names claude, is nested in the agent's own session. It
	// keeps that session's space rather than choosing one by its cwd.
	for _, outer := range c.outer {
		if p, ok, err := r.firstOf(ctx, outer); err != nil || ok {
			p.Harness, p.Process = harness, proc
			return p, ok, err
		}
	}
	if len(c.outer) > 0 {
		return sessionPin{}, false, errOuterUnpinned
	}
	// Reason: a process an agent detaches from its session (a backgrounded
	// subshell that exits) is reparented to launchd and has no harness above
	// it. A real session's harness runs under a shell, terminal or editor.
	// Accepted risk (2026-11-08): a detached shell kept alive above the fake
	// harness still passes; see docs/internals/daemon.md, Session pins.
	if c.orphan {
		return sessionPin{}, false, errOrphanHarness
	}
	if r.now().Sub(proc.Started) > newPinWindow {
		return sessionPin{}, false, errPinTooLate
	}
	// Reason: the hook's cwd is only a report. Checking it against the
	// kernel's view of the harness process keeps a process inside a harness
	// that started without a pin from choosing its own space.
	first, err := r.verifyCwd(proc.PID, cwd)
	if err != nil {
		return sessionPin{}, false, err
	}
	space, rev, err := place(ctx, first)
	if err != nil {
		return sessionPin{}, false, err
	}
	return sessionPin{
		Harness: harness, Process: proc, FirstCwd: first,
		MainCheckout: mainCheckout(first), Space: space, Revision: rev, PinnedAt: r.now().UTC(),
	}, false, nil
}

// resume moves existing's pin to harness process c when its session is
// resumed (SessionStart source=resume), keeping its space, first directory
// and policy revision (INV-05). The move is refused while the old process
// still runs the session, to another harness program, and when a new session
// in c would get another space: a resume can then never widen what the
// process reads. Resuming in the same process clears ended_at.
//
// Only a harness that could take a new pin of its own may take a pin from
// another process: new, not inside another session, and holding no pin.
// Reason: orchd cannot tell a real in-process /resume from an agent calling
// PinSession itself, which would otherwise move any dead session of its
// space onto its own process and write that session's command log (SP-017
// security review). An in-process /resume of another process's session is
// refused, and its events go to the process's own pin.
func (r *pinRegistry) resume(ctx context.Context, existing sessionPin, c harnessCaller, cwd string, place placeFunc) (sessionPin, error) {
	if !existing.Process.Same(c.proc) {
		if existing.Harness != c.name || (existing.EndedAt.IsZero() && r.alive(existing.Process)) {
			return existing, errPinnedElsewhere
		}
		if len(c.outer) > 0 {
			return existing, errResumeHere
		}
		if _, held, err := r.firstOf(ctx, c.proc); err != nil || held {
			if err == nil {
				err = errResumeHere
			}
			return existing, err
		}
		p, _, err := r.newPin(ctx, c, cwd, place)
		if err != nil {
			return existing, err
		}
		if p.Space != existing.Space {
			return existing, errResumeSpace
		}
	} else if existing.EndedAt.IsZero() {
		return existing, nil
	}
	now := r.now().UTC()
	if _, err := r.db.ExecContext(ctx, `UPDATE session_pins
		SET harness_pid = ?, harness_started = ?, ended_at = NULL, exited_at = NULL, resumed_at = ? WHERE session_id = ?`,
		c.proc.PID, c.proc.Started.UnixNano(), now.Format(time.RFC3339Nano), existing.SessionID); err != nil {
		return existing, err
	}
	existing.Process, existing.EndedAt, existing.ExitedAt, existing.ResumedAt = c.proc, time.Time{}, time.Time{}, now
	return existing, nil
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
	space, policy_revision, pinned_at, ended_at, inherited_from, resumed_at, exited_at`

func (r *pinRegistry) get(ctx context.Context, id string) (sessionPin, bool, error) {
	return scanPin(r.db.QueryRowContext(ctx, `SELECT `+pinColumns+` FROM session_pins WHERE session_id = ?`, id))
}

// firstOf returns the earliest pin of the harness process proc.
func (r *pinRegistry) firstOf(ctx context.Context, proc procstat.Identity) (sessionPin, bool, error) {
	return scanPin(r.db.QueryRowContext(ctx, `SELECT `+pinColumns+` FROM session_pins
		WHERE harness_pid = ? AND harness_started = ? ORDER BY rowid LIMIT 1`,
		proc.PID, proc.Started.UnixNano()))
}

// currentOf returns harness process proc's current session: its latest pin
// not yet ended, else its latest pin. Every pin a process holds has one space
// (it took its first and copied that), so this picks only the session ID.
func (r *pinRegistry) currentOf(ctx context.Context, proc procstat.Identity) (sessionPin, bool, error) {
	return scanPin(r.db.QueryRowContext(ctx, `SELECT `+pinColumns+` FROM session_pins
		WHERE harness_pid = ? AND harness_started = ? ORDER BY ended_at IS NULL DESC, rowid DESC LIMIT 1`,
		proc.PID, proc.Started.UnixNano()))
}

func scanPin(row *sql.Row) (sessionPin, bool, error) {
	var (
		p        sessionPin
		pid      int
		started  int64
		pinnedAt string
		endedAt  sql.NullString
		resumed  sql.NullString
		exited   sql.NullString
	)
	err := row.Scan(&p.SessionID, &p.Harness, &pid, &started, &p.FirstCwd, &p.MainCheckout,
		&p.Space, &p.Revision, &pinnedAt, &endedAt, &p.InheritedFrom, &resumed, &exited)
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
	if resumed.Valid {
		if p.ResumedAt, err = time.Parse(time.RFC3339Nano, resumed.String); err != nil {
			return sessionPin{}, false, fmt.Errorf("pin %s: resumed_at: %w", p.SessionID, err)
		}
	}
	if exited.Valid {
		if p.ExitedAt, err = time.Parse(time.RFC3339Nano, exited.String); err != nil {
			return sessionPin{}, false, fmt.Errorf("pin %s: exited_at: %w", p.SessionID, err)
		}
	}
	return p, true, nil
}

// insertCopy pins id to everything from's pin fixes.
func (r *pinRegistry) insertCopy(ctx context.Context, id string, from sessionPin) (sessionPin, error) {
	p := from
	p.SessionID, p.PinnedAt, p.EndedAt, p.ResumedAt, p.ExitedAt = id, r.now().UTC(), time.Time{}, time.Time{}, time.Time{}
	if p.InheritedFrom == "" {
		p.InheritedFrom = from.SessionID
	}
	return p, r.insert(ctx, p)
}

func (r *pinRegistry) insert(ctx context.Context, p sessionPin) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO session_pins (`+pinColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, NULL)`,
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
	p, err := s.pins.pin(pctx, id, parent, req.GetSource(), caller, req.GetCwd(), s.placeSession)
	if err != nil {
		log.Printf("orchd: PinSession %s: %v", id, err)
		return nil, pinStatus(err)
	}
	s.writeGuardPin(p)
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
	s.endToolLog(id)
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
		errors.Is(err, errOuterUnpinned), errors.Is(err, errPinTooLate), errors.Is(err, errOrphanHarness),
		errors.Is(err, errResumeSpace), errors.Is(err, errResumeHere):
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

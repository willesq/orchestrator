package daemon

import (
	"context"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// Session liveness (SP-017): whether a pinned session still runs comes from
// the kernel, never from hook events. A pin's harness PID must still exist
// with the same start time; sessions are often left open without /exit for
// days, and log retention and pins must follow the real process. See
// docs/internals/daemon.md "Session liveness".

// Session states in ListSessions.
const (
	stateActive   = "active"
	stateIdle     = "idle"
	stateEnded    = "ended"
	stateUnpinned = "unpinned"
)

// idleAfter is how long a running session may go without a hook event
// before it shows as idle. It stays idle however long it is left open.
const idleAfter = 30 * time.Minute

// sessionSweepEvery is how often orchd ends the pins whose harness process
// is gone, so such a session is marked ended within a minute.
const sessionSweepEvery = 60 * time.Second

// alive reports whether proc still runs: its PID exists with the same start
// time. A PID the kernel reused for another process is not alive, so a pin
// never follows its PID to an unrelated process. An unreadable process
// counts as gone, as in harnessCaller.
func (r *pinRegistry) alive(proc procstat.Identity) bool {
	started, err := r.started(proc.PID)
	return err == nil && started.Equal(proc.Started)
}

// sweep ends every pin not yet ended whose harness process is gone, with
// ended_at the time orchd noticed, and returns their session IDs. It also
// sets exited_at, the time orchd noticed the process gone, on those pins
// and on pins ended earlier whose process has since exited (SP-015).
func (r *pinRegistry) sweep(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.QueryContext(ctx,
		`SELECT session_id, harness_pid, harness_started, ended_at IS NOT NULL FROM session_pins
		WHERE ended_at IS NULL OR exited_at IS NULL`)
	if err != nil {
		return nil, err
	}
	var gone, exited []string
	// Reason: the sessions of one harness process (/clear, subagents)
	// share it, so each process is read once.
	seen := map[procKey]bool{}
	for rows.Next() {
		var (
			id      string
			pid     int
			started int64
			ended   bool
		)
		if err := rows.Scan(&id, &pid, &started, &ended); err != nil {
			_ = rows.Close()
			return nil, err
		}
		proc := procstat.Identity{PID: pid, Started: time.Unix(0, started)}
		live, ok := seen[keyOf(proc)]
		if !ok {
			live = r.alive(proc)
			seen[keyOf(proc)] = live
		}
		switch {
		case live:
		case ended:
			exited = append(exited, id)
		default:
			gone = append(gone, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(gone)+len(exited) == 0 {
		return nil, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := r.now().UTC().Format(time.RFC3339Nano)
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx,
			`UPDATE session_pins SET ended_at = ?, exited_at = ? WHERE session_id = ? AND ended_at IS NULL`, now, now, id); err != nil {
			return nil, err
		}
	}
	for _, id := range exited {
		if _, err := tx.ExecContext(ctx,
			`UPDATE session_pins SET exited_at = ? WHERE session_id = ? AND exited_at IS NULL`, now, id); err != nil {
			return nil, err
		}
	}
	return gone, tx.Commit()
}

// all returns every pin, oldest first.
func (r *pinRegistry) all(ctx context.Context) ([]sessionPin, error) {
	// Reason: scanPin takes a *sql.Row only. Through an interface the
	// command log's call-graph check (toollog TestCommandLogNeverReachesRAG)
	// would follow every Scan method, so each pin is read by ID.
	rows, err := r.db.QueryContext(ctx, `SELECT session_id FROM session_pins ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]sessionPin, 0, len(ids))
	for _, id := range ids {
		p, ok, err := r.get(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// sessionState is a pin's state at now, given whether its harness process
// runs and when the session was last active.
func sessionState(p sessionPin, alive bool, last, now time.Time) string {
	switch {
	case !p.EndedAt.IsZero() || !alive:
		return stateEnded
	case now.Sub(last) < idleAfter:
		return stateActive
	}
	return stateIdle
}

// listedHarnesses are the kernel names of harness processes the session list
// shows when they hold no pin. gemini has no pins (D-20: no Google harness
// is covered), so a running one always shows as unpinned.
var listedHarnesses = map[string]bool{"claude": true, "codex": true, "gemini": true}

// versionName is the kernel name of Claude Code's native binary, which is
// named for its version.
var versionName = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

// listedHarness names the harness process p runs, or "". Like
// procstat.FindByName it goes by the kernel name; a process whose name says
// nothing (an interpreter, or a binary named for its version) is identified
// by its program, as caller resolution does.
func (r *pinRegistry) listedHarness(p procstat.Process) string {
	if listedHarnesses[p.Name] {
		return p.Name
	}
	if !interpreters[p.Name] && !versionName.MatchString(p.Name) {
		return ""
	}
	prog, err := r.program(p.PID)
	if err != nil {
		return ""
	}
	if h := harnessOf(prog); h != "" {
		return h
	}
	for _, c := range append([]string{prog.Path}, prog.Args[:min(2, len(prog.Args))]...) {
		if strings.Contains(c, "/@google/gemini-cli/") {
			return "gemini"
		}
	}
	return ""
}

// unpinned returns the running harness processes that hold no pin not yet
// ended, e.g. ones started before the hooks were installed. Where the
// process table cannot be read (outside macOS) there are none.
func (r *pinRegistry) unpinned(pins []sessionPin) []procstat.Process {
	procs, err := r.procs()
	if err != nil {
		return nil
	}
	pinned := map[procKey]bool{}
	for _, p := range pins {
		if p.EndedAt.IsZero() {
			pinned[keyOf(p.Process)] = true
		}
	}
	var out []procstat.Process
	for _, p := range procs {
		if pinned[keyOf(p.Identity)] {
			continue
		}
		if h := r.listedHarness(p); h != "" {
			p.Name = h
			out = append(out, p)
		}
	}
	return out
}

// sweepSessions runs one liveness sweep and closes the command log chains
// of the sessions it ended, as SessionEnd would.
func (s *server) sweepSessions(ctx context.Context) {
	gone, err := s.pins.sweep(ctx)
	if err != nil {
		log.Printf("orchd: session liveness sweep: %v", err)
		return
	}
	for _, id := range gone {
		s.endToolLog(id)
	}
}

// runSessionSweeps sweeps at start and then every interval until ctx ends.
func (s *server) runSessionSweeps(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.sweepSessions(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ListSessions lists the sessions the caller may see. The operator sees
// every pin and every running harness process with no pin; a session's
// caller sees only its own row; anyone else sees none. Queued tasks are
// refused before this runs (operatorActions). Ended sessions are listed only
// with all.
func (s *server) ListSessions(ctx context.Context, req *orchdv1.ListSessionsRequest) (*orchdv1.ListSessionsResponse, error) {
	if s.pins == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "session pins are unavailable")
	}
	_, isOperator := operator(ctx)
	caller := callerOf(ctx)
	if !isOperator && caller.kind != callerSession {
		return &orchdv1.ListSessionsResponse{}, nil
	}
	s.sweepSessions(ctx)
	var pins []sessionPin
	if isOperator {
		all, err := s.pins.all(ctx)
		if err != nil {
			log.Printf("orchd: ListSessions: %v", err)
			return nil, grpcstatus.Error(codes.Internal, "the sessions could not be read")
		}
		pins = all
	} else {
		// Reason: the resolved pin is a session of the caller's harness
		// process (RC-27); its row is the one shown, re-read for its state.
		// A connection caches its pin, so a pin a resume has since moved to
		// another process is no longer the caller's.
		p, ok, err := s.pins.get(ctx, caller.pin.SessionID)
		if err != nil {
			log.Printf("orchd: ListSessions: %v", err)
			return nil, grpcstatus.Error(codes.Internal, "the sessions could not be read")
		}
		if ok && p.Process.Same(caller.pin.Process) {
			pins = []sessionPin{p}
		}
	}
	rows := s.sessionRows(ctx, pins, req.GetAll())
	if isOperator {
		for _, p := range s.pins.unpinned(pins) {
			rows = append(rows, &orchdv1.SessionInfo{Harness: p.Name, HarnessPid: int64(p.PID), State: stateUnpinned})
		}
	}
	return &orchdv1.ListSessionsResponse{Sessions: rows}, nil
}

// sessionRows turns pins into rows, newest activity first, leaving out ended
// sessions unless all is set.
func (s *server) sessionRows(ctx context.Context, pins []sessionPin, all bool) []*orchdv1.SessionInfo {
	now := s.pins.now()
	ids := make([]string, len(pins))
	for i, p := range pins {
		ids[i] = p.SessionID
	}
	var activity map[string]toollog.Activity
	if s.toolLog != nil {
		var err error
		if activity, err = s.toolLog.store.SessionActivity(ctx, ids); err != nil {
			log.Printf("orchd: ListSessions: command log: %v", err)
		}
	}
	type row struct {
		info *orchdv1.SessionInfo
		last time.Time
	}
	alive := map[procKey]bool{}
	var out []row
	for _, p := range pins {
		last := p.PinnedAt
		if p.ResumedAt.After(last) {
			last = p.ResumedAt
		}
		a, logged := activity[p.SessionID]
		if at := time.Unix(0, a.Last); logged && at.After(last) {
			last = at
		}
		live, ok := alive[keyOf(p.Process)]
		if !ok {
			live = s.pins.alive(p.Process)
			alive[keyOf(p.Process)] = live
		}
		state := sessionState(p, live, last, now)
		if state == stateEnded && !all {
			continue
		}
		info := &orchdv1.SessionInfo{
			Harness: p.Harness, SessionId: p.SessionID, HarnessPid: int64(p.Process.PID),
			Space: p.Space, FirstCwd: p.FirstCwd, State: state, LastActivity: last.Unix(),
			ToolEvents: a.Events, Refusals: a.Refused,
		}
		if !p.EndedAt.IsZero() {
			info.EndedAt = p.EndedAt.Unix()
		}
		out = append(out, row{info, last})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].last.After(out[j].last) })
	infos := make([]*orchdv1.SessionInfo, len(out))
	for i, r := range out {
		infos[i] = r.info
	}
	return infos
}

// procKey is a process identity usable as a map key: time.Time values that
// name one instant can differ in location or monotonic reading.
type procKey struct {
	pid     int
	started int64
}

func keyOf(p procstat.Identity) procKey { return procKey{p.PID, p.Started.UnixNano()} }

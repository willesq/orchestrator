package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Under a spaces policy only Orch.app is the operator (D-71), so a terminal
// cannot run an operator action such as 'orch rag relabel'. The unlock
// (SP-077) is the way back: 'orch spaces unlock --for 15m' asks orchd for
// operator scope, orchd holds the request until Orch.app confirms or denies
// it, the user types the code Orch.app then shows into the terminal, and
// only then does the unlock make the requesting process and its
// descendants the operator until it ends. A descendant inside an agent
// harness is never the operator, and its first call to orchd ends the
// unlock above it. See docs/internals/daemon.md "Operator unlock"; the
// RPCs are in unlock_rpc.go.

// operatorUnlock is the evidence that a caller descends from a process
// whose unlock Orch.app confirmed.
const operatorUnlock = "unlock"

const (
	// maxUnlockFor bounds an unlock's duration.
	maxUnlockFor = 60 * time.Minute
	// unlockPendingFor is how long a request waits for Orch.app.
	unlockPendingFor = 2 * time.Minute
	// maxPendingUnlocks bounds the requests waiting at once, so a process
	// cannot fill Orch.app with requests.
	maxPendingUnlocks = 4
	// maxActiveUnlocks bounds the unlocks in force at once.
	maxActiveUnlocks = 8
	// maxUnlockRequestsPerMinute bounds new requests, so the user is not
	// worn down by a stream of them.
	maxUnlockRequestsPerMinute = 6
	// maxUnlockCommand bounds the command line shown to the user.
	maxUnlockCommand = 512
	// maxUnlockAncestry bounds the parents named with a request.
	maxUnlockAncestry = 8
	// maxUnlockTries bounds the wrong codes a confirmed request takes.
	maxUnlockTries = 3
)

// Unlock states.
const (
	unlockPending = "pending"
	// unlockConfirmed: Orch.app confirmed it and shows its code; nothing is
	// granted until the requester sends that code (ProveUnlock).
	unlockConfirmed = "confirmed"
	unlockActive    = "active"
)

// unlockStateOrder lists pending requests first, then confirmed ones, then
// unlocks in force.
var unlockStateOrder = map[string]int{unlockPending: 0, unlockConfirmed: 1, unlockActive: 2}

// unlockCallable are the RPCs a caller that is the operator by its process
// tree alone may make under a policy, besides those queued tasks may make:
// asking for an unlock and ending one (refuseUnproven). Their handlers
// refuse sessions, tasks and harness descendants themselves.
var unlockCallable = map[string]bool{
	orchdv1.Orchd_RequestUnlock_FullMethodName: true,
	orchdv1.Orchd_ProveUnlock_FullMethodName:   true,
	orchdv1.Orchd_Lock_FullMethodName:          true,
}

// unlock is one request for operator scope and, once confirmed, its grant.
type unlock struct {
	id    string
	state string
	// code is what Orch.app shows once it confirms, for the user to type
	// in the requesting terminal; tries counts wrong ones. Reason: the
	// terminal is not a channel orchd can trust (any process of the user
	// can write to it), so the proof flows from the app, which only the
	// user sees, to the terminal, which only the requester reads.
	code  string
	tries int
	// forDur is how long the grant lasts once confirmed.
	forDur time.Duration
	// proc is the requesting process, by PID and start time, so a later
	// process taking over its PID is not covered.
	proc       procstat.Identity
	command    string
	executable string
	// tty is the requester's controlling terminal, which Orch.app shows
	// so the user can tell their terminal's request from another's.
	tty       string
	ancestry  []string
	requested time.Time
	// expires is when a pending request lapses, or an active grant ends.
	expires time.Time
	// decided is closed when the request is confirmed or ends unconfirmed;
	// ended then says why (denied, lapsed or locked).
	decided chan struct{}
	ended   string
}

// end closes a pending request's wait for the reason why; a confirmed
// request and an active grant have no waiter.
func (u *unlock) end(why string) {
	if u.state == unlockPending {
		u.ended = why
		close(u.decided)
	}
}

// proto is u for Orch.app, with the code; the requester gets it without
// (requesterProto).
func (u *unlock) proto() *orchdv1.OperatorUnlock {
	return &orchdv1.OperatorUnlock{
		Id: u.id, State: u.state, ForSeconds: int64(u.forDur / time.Second), Pid: int32(u.proc.PID),
		Command: u.command, Ancestry: slices.Clone(u.ancestry),
		RequestedAt: u.requested.Unix(), ExpiresAt: u.expires.Unix(), Code: u.code, Executable: u.executable, Tty: u.tty,
	}
}

func (u *unlock) requesterProto() *orchdv1.OperatorUnlock {
	p := u.proto()
	p.Code = ""
	return p
}

// unlockRegistry holds the unlocks in orchd's memory: a restart ends them
// all, which fails safe.
type unlockRegistry struct {
	mu   sync.Mutex
	byID map[string]*unlock
	// recent are the times of the requests of the last minute.
	recent []time.Time
	now    func() time.Time
	// parent, started and program read the process table; samePeer checks
	// that a PID still names the process that opened a connection
	// (peer_token.go). Tests replace them.
	parent   func(int) (int, error)
	started  func(int) (time.Time, error)
	program  func(int) (procstat.Program, error)
	tty      func(int) (string, error)
	samePeer func(net.Conn, int) error
}

func newUnlockRegistry() *unlockRegistry {
	return &unlockRegistry{
		byID: map[string]*unlock{}, now: time.Now,
		parent: procstat.Parent, started: procstat.StartTime, program: procstat.ReadProgram, tty: procstat.TTY,
		samePeer: connSamePeer,
	}
}

// errUnlockLimit refuses a request while too many wait or are in force, or
// too many came in the last minute.
var errUnlockLimit = errors.New("too many unlocks are waiting, in force or were asked for in the last minute; deny or lock them in Orch.app, or try again in a minute")

// livePeer reports whether a's PID still names the process that opened its
// connection. Reason (D-44): identify() checks the PID only at accept, and
// a connection handed to another process outlives its opener, whose PID can
// then be reused by a process under an unlock. A caller orchd may never
// treat as the operator has no connection to check (a.app is set only for
// callers outside any harness that orchd did not start), and fails.
func (r *unlockRegistry) livePeer(a callerAuth) bool {
	return a.app != nil && a.app.c != nil && r.samePeer(a.app.c, a.pid) == nil
}

// request records a pending request from a's process for forDur.
func (r *unlockRegistry) request(a callerAuth, forDur time.Duration) (*unlock, error) {
	pid := a.pid
	started, err := r.started(pid)
	if err != nil {
		return nil, fmt.Errorf("the requesting process cannot be identified: %w", err)
	}
	command, executable, ancestry := r.describe(pid)
	tty := "(unknown)"
	if t, err := r.tty(pid); err == nil {
		tty = printable(t, 64)
	}
	// Reason: checked after reading the process, so what was read is the
	// caller's: its PID cannot be reused while it still runs.
	if !r.livePeer(a) {
		return nil, errors.New("the requesting process cannot be identified: it is not the process that connected")
	}
	id, err := unlockID()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	pending, active := 0, 0
	for _, u := range r.byID {
		switch {
		case u.state == unlockActive:
			active++
		case u.proc.PID == pid:
			// Reason: one process waits for one answer at a time.
			return nil, errors.New("this process already waits for an unlock")
		default:
			pending++
		}
	}
	if pending >= maxPendingUnlocks || active >= maxActiveUnlocks || len(r.recent) >= maxUnlockRequestsPerMinute {
		return nil, errUnlockLimit
	}
	r.recent = append(r.recent, now)
	u := &unlock{
		id: id, state: unlockPending, forDur: forDur, proc: procstat.Identity{PID: pid, Started: started},
		command: command, executable: executable, tty: tty, ancestry: ancestry, requested: now, expires: now.Add(unlockPendingFor),
		decided: make(chan struct{}),
	}
	r.byID[id] = u
	return u, nil
}

// wait holds a pending request until it is decided, lapses, or ctx ends;
// a request that is not confirmed is forgotten, and so is one whose
// requester gave up as it was confirmed.
func (r *unlockRegistry) wait(ctx context.Context, u *unlock) (*orchdv1.OperatorUnlock, error) {
	r.mu.Lock()
	lapses := u.expires.Sub(r.now())
	r.mu.Unlock()
	timer := time.NewTimer(lapses)
	defer timer.Stop()
	var cause error
	select {
	case <-u.decided:
	case <-ctx.Done():
		cause = grpcstatus.Error(codes.Canceled, "the request was withdrawn")
	case <-timer.C:
		cause = grpcstatus.Errorf(codes.DeadlineExceeded, "Orch.app did not confirm the unlock within %s", unlockPendingFor)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch u.ended {
	case "":
		if u.state == unlockConfirmed && ctx.Err() == nil {
			return u.requesterProto(), nil
		}
	case "denied":
		cause = grpcstatus.Error(codes.PermissionDenied, "the unlock was denied in Orch.app")
	case "locked":
		cause = grpcstatus.Error(codes.PermissionDenied, "the request was ended in Orch.app")
	case "lapsed":
		cause = grpcstatus.Errorf(codes.DeadlineExceeded, "Orch.app did not confirm the unlock within %s", unlockPendingFor)
	}
	if cause == nil {
		cause = grpcstatus.Error(codes.Canceled, "the request was withdrawn")
	}
	if r.byID[u.id] == u {
		delete(r.byID, u.id)
	}
	return nil, cause
}

// confirm confirms (allow) or denies the pending request id. A confirmed
// request gets the code Orch.app shows, and lapses unless the requester
// sends it in time.
func (r *unlockRegistry) confirm(id string, allow bool) (*orchdv1.OperatorUnlock, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	u, ok := r.byID[id]
	switch {
	case !ok:
		return nil, grpcstatus.Error(codes.NotFound, "no such unlock request; it may have lapsed or been withdrawn")
	case u.state != unlockPending:
		return nil, grpcstatus.Error(codes.FailedPrecondition, "the unlock was already confirmed")
	}
	if !allow {
		delete(r.byID, id)
		u.end("denied")
		return &orchdv1.OperatorUnlock{}, nil
	}
	code, err := unlockCode()
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	u.state, u.code, u.expires = unlockConfirmed, code, now.Add(unlockPendingFor)
	close(u.decided)
	return u.proto(), nil
}

// errUnlockCode refuses a code that does not match.
var errUnlockCode = errors.New("that is not the code Orch.app shows")

// prove grants the confirmed request id when a's process asked for it and
// sends the code Orch.app shows. Each wrong code counts; the request is
// dropped at the maxUnlockTries-th.
func (r *unlockRegistry) prove(a callerAuth, id, code string) (*orchdv1.OperatorUnlock, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	u, ok := r.byID[id]
	switch {
	case !ok || u.proc.PID != a.pid || !r.alive(u.proc) || !r.livePeer(a):
		return nil, grpcstatus.Error(codes.NotFound, "no such unlock waits for this process's code; it may have lapsed")
	case u.state != unlockConfirmed:
		return nil, grpcstatus.Error(codes.FailedPrecondition, "the unlock is not confirmed in Orch.app yet")
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(u.code)) != 1 {
		u.tries++
		if u.tries >= maxUnlockTries {
			delete(r.byID, id)
			return nil, grpcstatus.Errorf(codes.PermissionDenied, "%v; the request is dropped after %d tries", errUnlockCode, maxUnlockTries)
		}
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "%v (%d of %d tries left)", errUnlockCode, maxUnlockTries-u.tries, maxUnlockTries)
	}
	active := 0
	for _, o := range r.byID {
		if o.state == unlockActive {
			active++
		}
	}
	if active >= maxActiveUnlocks {
		return nil, grpcstatus.Error(codes.ResourceExhausted, errUnlockLimit.Error())
	}
	u.state, u.code, u.expires = unlockActive, "", now.Add(u.forDur)
	return u.requesterProto(), nil
}

// list returns the pending requests, the confirmed ones (with their codes,
// for Orch.app), then the unlocks in force, oldest first.
func (r *unlockRegistry) list() []*orchdv1.OperatorUnlock {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	us := make([]*unlock, 0, len(r.byID))
	for _, u := range r.byID {
		us = append(us, u)
	}
	slices.SortFunc(us, func(a, b *unlock) int {
		if a.state != b.state {
			return unlockStateOrder[a.state] - unlockStateOrder[b.state]
		}
		if c := a.requested.Compare(b.requested); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	out := make([]*orchdv1.OperatorUnlock, 0, len(us))
	for _, u := range us {
		out = append(out, u.proto())
	}
	return out
}

// lock ends the unlocks keep reports true for, and returns how many.
func (r *unlockRegistry) lock(keep func(*unlock) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, u := range r.byID {
		if !keep(u) {
			continue
		}
		delete(r.byID, id)
		u.end("locked")
		n++
	}
	return n
}

// covering returns the unlocks in force that a's process is, or descends
// from, when its PID still names the process that connected.
func (r *unlockRegistry) covering(a callerAuth) []*unlock {
	if r == nil {
		return nil
	}
	grants := r.grants()
	if len(grants) == 0 {
		return nil
	}
	mine := r.coveredBy(a.pid, grants)
	// Reason: checked after the walk, so the walk was of the caller.
	if len(mine) == 0 || !r.livePeer(a) {
		return nil
	}
	return mine
}

// covers reports whether an unlock in force covers a's process.
func (r *unlockRegistry) covers(a callerAuth) bool { return len(r.covering(a)) > 0 }

// endUnder ends every unlock in force above a caller that runs inside an
// agent harness. Reason: an agent started in an unlocked shell must not
// drive that shell (typing into its terminal, scripting the terminal app)
// to act as the operator; its first call to orchd (its session hooks and
// MCP server connect as it starts) ends the unlock. The walk starts at the
// harness process orchd identified at accept, and only while it still runs,
// so a connection outliving its harness cannot end another's unlock.
func (r *unlockRegistry) endUnder(a callerAuth) {
	if r == nil || a.harnessProc.PID <= 0 {
		return
	}
	grants := r.grants()
	if len(grants) == 0 || !r.alive(a.harnessProc) {
		return
	}
	pid, harness := a.harnessProc.PID, a.harness
	under := r.coveredBy(pid, grants)
	if len(under) == 0 {
		return
	}
	if n := r.lock(func(u *unlock) bool { return slices.Contains(under, u) }); n > 0 {
		log.Printf("orchd: %d unlock(s) ended: %s process %d runs under them", n, harness, pid)
	}
}

// grants returns the unlocks in force now.
func (r *unlockRegistry) grants() []*unlock {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	var out []*unlock
	for _, u := range r.byID {
		if u.state == unlockActive {
			out = append(out, u)
		}
	}
	return out
}

// coveredBy returns the grants among grants that pid is, or descends from.
func (r *unlockRegistry) coveredBy(pid int, grants []*unlock) []*unlock {
	var out []*unlock
	for range maxAncestry {
		for _, u := range grants {
			if u.proc.PID == pid && r.alive(u.proc) {
				out = append(out, u)
			}
		}
		ppid, err := r.parent(pid)
		if err != nil || ppid <= 1 || ppid == pid {
			return out
		}
		pid = ppid
	}
	return out
}

// pruneLocked forgets lapsed requests, ended grants, grants whose process
// is gone (it covers nothing any more: its descendants were reparented, and
// Orch.app should not show it in force), and requests older than a minute
// in the rate count.
func (r *unlockRegistry) pruneLocked(now time.Time) {
	for id, u := range r.byID {
		if now.Before(u.expires) && (u.state == unlockPending || r.alive(u.proc)) {
			continue
		}
		delete(r.byID, id)
		u.end("lapsed")
	}
	r.recent = slices.DeleteFunc(r.recent, func(t time.Time) bool { return now.Sub(t) >= time.Minute })
}

// alive reports whether proc still runs: its PID names a process that
// started when it did.
func (r *unlockRegistry) alive(proc procstat.Identity) bool {
	started, err := r.started(proc.PID)
	return err == nil && proc.Same(procstat.Identity{PID: proc.PID, Started: started})
}

// describe names process pid for the user: its command line (each argument
// quoted when needed), its executable, and its parents' programs (e.g. zsh,
// login, Terminal), bounded and printable.
func (r *unlockRegistry) describe(pid int) (command, executable string, ancestry []string) {
	command = "(unknown)"
	if p, err := r.program(pid); err == nil {
		quoted := make([]string, 0, len(p.Args))
		for _, a := range p.Args {
			quoted = append(quoted, argQuote(a))
		}
		command = printable(strings.Join(quoted, " "), maxUnlockCommand)
		if len(p.Args) == 0 {
			command = printable(p.Name, maxUnlockCommand)
		}
		executable = printable(p.Path, maxUnlockCommand)
	}
	for len(ancestry) < maxUnlockAncestry {
		ppid, err := r.parent(pid)
		if err != nil || ppid <= 1 || ppid == pid {
			break
		}
		name := "?"
		if p, err := r.program(ppid); err == nil {
			name = p.Name
			if p.Path != "" {
				name = p.Path
			}
		}
		ancestry = append(ancestry, fmt.Sprintf("%s (%d)", printable(name, 128), ppid))
		pid = ppid
	}
	return command, executable, ancestry
}

// argQuote quotes an argument that is empty or holds anything but plain
// characters (shellQuote), so the user sees where each one starts and ends.
func argQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) {
		return s
	}
	return shellQuote(s)
}

// printable keeps s to printable characters and at most n bytes, so a
// command line cannot hide text from the user who reads it: control
// characters, line and paragraph separators, and invisible or reordering
// format characters become '?'.
func printable(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0, r == 0xad,
			r >= 0x200b && r <= 0x200f, r >= 0x2028 && r <= 0x202e, r >= 0x2060 && r <= 0x2069,
			r == 0xfeff, r >= 0xfff9 && r <= 0xfffb, r >= 0xe0000 && r <= 0xe007f:
			return '?'
		}
		return r
	}, s)
	if len(s) > n {
		cut := n
		for cut > 0 && s[cut]&0xc0 == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// unlockCode returns a random code of three digits, a dash and three
// digits, e.g. 482-913.
func unlockCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	v := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1_000_000
	return fmt.Sprintf("%03d-%03d", v/1000, v%1000), nil
}

func unlockID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// unlockLogs limits the unlock RPCs' refusal log lines, which any session
// could otherwise repeat without end.
var unlockLogs = &logLimit{every: time.Second}

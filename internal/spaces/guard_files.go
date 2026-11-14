package spaces

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The enforcing guard (spaces plan SP-041) runs in orch-hook, which starts
// for every tool call and cannot wait on orchd. orchd therefore writes what
// the guard reads into two kinds of owner-only files: one snapshot of the
// applied policy with the guard's switch, and one pin file per session. The
// hook reads them, extracts the call's targets and calls Decide, all within
// the call. Anything it cannot read while the guard enforces is a refusal
// (INV-13).

// errSnapshotVersion marks a snapshot of another format version.
var errSnapshotVersion = errors.New("policy snapshot has another version")

// GuardSnapshotVersion is the snapshot format orchd writes and the hook
// reads. A snapshot of another version is unreadable. Version 2 carries the
// whole self-protection (SP-042, RC-110); a version 1 snapshot, which
// carried only the guard directory, is therefore unreadable, and the guard
// refuses while it enforces until orchd rewrites it (INV-13).
const GuardSnapshotVersion = 2

// Rules the hook adds around Decide. Like Decide's rules, they name a rule,
// never a path or text.
const (
	// RulePolicyUnreadable: the policy snapshot is missing, damaged or
	// marks a policy that does not verify, while spaces may be configured;
	// enforcing modes refuse (INV-13).
	RulePolicyUnreadable GuardRule = "policy_unreadable"
	// RuleGuardTimeout: the decision did not finish in time; enforcing
	// modes refuse (INV-13).
	RuleGuardTimeout GuardRule = "guard_timeout"
)

// GuardSwitch is the guard layer's switch as the snapshot carries it. A
// timed switch carries its end and the state it reverts to, so the hook
// reverts it on time even when orchd is not running.
type GuardSwitch struct {
	State    string    `json:"state"`
	Until    time.Time `json:"until,omitzero"`
	Previous string    `json:"previous,omitempty"`
}

// GuardSnapshot is the policy file orchd writes for the hook.
type GuardSnapshot struct {
	Version int `json:"version"`
	// Revision is the applied policy revision, 0 when none is applied.
	Revision int64 `json:"revision"`
	// Policy is the applied policy; nil when none is applied, so no space
	// is configured.
	Policy *Policy `json:"policy,omitempty"`
	// Broken marks an applied policy that no longer verifies: no space can
	// be placed, so enforcing modes refuse every call.
	Broken bool `json:"broken,omitempty"`
	// Home expands "~" in the policy's roots, as orchd expanded them.
	Home string `json:"home"`
	// Protection is what the guard protects (SP-042): ProtectedPaths for
	// the applied policy, plus the guard directory. A snapshot with a
	// policy and no write protection is unreadable, so the guard never
	// enforces a policy without its self-protection.
	Protection Protection  `json:"protection"`
	Guard      GuardSwitch `json:"guard"`
	// Terms is the outbound terms layer's switch (SP-044, RC-116), which
	// the hook's terms check reads from here rather than from orch.db.
	// A snapshot without it holds the layer's default.
	Terms GuardSwitch `json:"terms,omitzero"`
	// Rules are the operator's outbound rules (SP-075) as of revision
	// RulesRev; nil is the baseline. RulesBroken marks rules orchd could
	// not read, which the terms check cannot vouch for: it refuses
	// outbound calls.
	Rules       *OutboundRules `json:"rules,omitempty"`
	RulesRev    int64          `json:"rules_rev,omitempty"`
	RulesBroken bool           `json:"rules_broken,omitempty"`
	WrittenAt   time.Time      `json:"written_at"`
}

// Mode is the guard's mode at now, after a timed switch's end.
func (s GuardSnapshot) Mode(now time.Time) GuardMode {
	sw := Enforcement{LayerGuard: {Layer: LayerGuard, State: s.Guard.State, Until: s.Guard.Until, Previous: s.Guard.Previous}}
	return GuardMode(sw.At(now).State(LayerGuard))
}

// TermsState is the terms layer's state at now, after a timed switch's
// end; the layer's default when the snapshot carries no switch.
func (s GuardSnapshot) TermsState(now time.Time) string {
	if s.Terms.State == "" {
		return LayerTerms.Default()
	}
	sw := Enforcement{LayerTerms: {Layer: LayerTerms, State: s.Terms.State, Until: s.Terms.Until, Previous: s.Terms.Previous}}
	return sw.At(now).State(LayerTerms)
}

// GuardPinFile is one session's pin as orchd writes it for the hook.
type GuardPinFile struct {
	SessionID string `json:"session_id"`
	// Space is the pinned space; "" or Quarantine for none.
	Space    string `json:"space"`
	Revision int64  `json:"revision"`
	Harness  string `json:"harness,omitempty"`
}

// guardSessionID is what a session ID must look like to name a pin file,
// the same rule orchd applies to every session ID.
var guardSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// maxGuardFile bounds what the hook reads of a guard file. A policy is at
// most 1 MiB (maxDocBytes) plus its protected paths.
const maxGuardFile = 2 << 20

const (
	guardSnapshotName = "policy.json"
	guardPinsDir      = "pins"
)

// GuardSnapshotPath is the snapshot's path in guard directory dir.
func GuardSnapshotPath(dir string) string { return filepath.Join(dir, guardSnapshotName) }

// GuardPinPath is session id's pin file in dir, "" for an ID that cannot
// name a file.
func GuardPinPath(dir, id string) string {
	if !guardSessionID.MatchString(id) {
		return ""
	}
	return filepath.Join(dir, guardPinsDir, id+".json")
}

// WriteGuardSnapshot replaces the snapshot in dir atomically.
func WriteGuardSnapshot(dir string, s GuardSnapshot) error {
	s.Version = GuardSnapshotVersion
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeGuardFile(dir, GuardSnapshotPath(dir), b)
}

// WriteGuardPin replaces session p.SessionID's pin file in dir atomically.
func WriteGuardPin(dir string, p GuardPinFile) error {
	path := GuardPinPath(dir, p.SessionID)
	if path == "" {
		return fmt.Errorf("session ID %q cannot name a pin file", p.SessionID)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeGuardFile(dir, path, b)
}

// writeGuardFile writes b to path through a temporary file and a rename, so
// the hook never reads half a file, with the directories owner-only.
func writeGuardFile(dir, path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Reason: MkdirAll leaves an existing directory's mode alone.
	for _, d := range []string{dir, filepath.Dir(path)} {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// readGuardFile reads a guard file that is a regular file of bounded size.
// Reason: it checks and reads one handle, opened without following a
// symlink and without blocking on a FIFO, so the file cannot be swapped
// between the check and the read.
func readGuardFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !st.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	b, err := io.ReadAll(io.LimitReader(f, maxGuardFile+1))
	switch {
	case err != nil:
		return nil, err
	case len(b) > maxGuardFile:
		return nil, fmt.Errorf("%s is over %d bytes", filepath.Base(path), maxGuardFile)
	}
	return b, nil
}

// ReadGuardSnapshot reads the snapshot in dir. A missing file is an error
// wrapping os.ErrNotExist.
func ReadGuardSnapshot(dir string) (GuardSnapshot, error) {
	b, err := readGuardFile(GuardSnapshotPath(dir))
	if err != nil {
		return GuardSnapshot{}, err
	}
	var s GuardSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return GuardSnapshot{}, fmt.Errorf("policy snapshot: %w", err)
	}
	switch {
	case s.Version == 0:
		// Reason: every orchd writes a version; a snapshot without one is
		// damaged, not of another release.
		return GuardSnapshot{}, errors.New("policy snapshot has no version")
	case s.Version != GuardSnapshotVersion:
		return GuardSnapshot{}, fmt.Errorf("%w: %d, not %d", errSnapshotVersion, s.Version, GuardSnapshotVersion)
	}
	return s, nil
}

// ReadGuardPin reads session id's pin file in dir.
func ReadGuardPin(dir, id string) (GuardPinFile, error) {
	path := GuardPinPath(dir, id)
	if path == "" {
		return GuardPinFile{}, fmt.Errorf("session ID %q cannot name a pin file", id)
	}
	b, err := readGuardFile(path)
	if err != nil {
		return GuardPinFile{}, err
	}
	var p GuardPinFile
	if err := json.Unmarshal(b, &p); err != nil {
		return GuardPinFile{}, fmt.Errorf("pin file: %w", err)
	}
	if p.SessionID != id {
		return GuardPinFile{}, errors.New("pin file names another session")
	}
	return p, nil
}

// HasApplied reports, read-only, whether the orch.db at dbPath has ever
// applied a spaces policy. A missing store, or one that predates spaces,
// has not. The hook asks when the snapshot cannot be read: then a store
// with an applied policy means spaces may be configured.
func HasApplied(ctx context.Context, dbPath string) (bool, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// Reason: built as a URL, as ReadEnforcement does, so the path cannot
	// add connection options.
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}).String()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	e, err := newestApplied(ctx, conn)
	switch {
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return false, nil
	case err != nil:
		return false, err
	}
	return e != nil, nil
}

// GuardInput is the tool call the hook decides.
type GuardInput struct {
	Kind      GuardEventKind
	SessionID string
	Tool      string
	Input     []byte
	Cwd       string
	// NewCwd is a cwd-changed event's new directory.
	NewCwd string
}

// GuardFiles decides tool calls from orchd's guard files.
type GuardFiles struct {
	// Dir is the guard directory orchd writes.
	Dir string
	// DB is orch.db, read only when the snapshot cannot be.
	DB string
	// Now is the clock for timed switches; nil is time.Now.
	Now func() time.Time
	// OnMode, when set, is told the guard's mode as soon as it is known,
	// so a caller that gives up waiting knows whether to refuse.
	OnMode func(GuardMode)
}

// GuardResult is a decision with the policy revision it was made against.
type GuardResult struct {
	GuardDecision
	Revision int64
	// Terms is what the outbound terms check needs from the same files.
	Terms GuardTerms
}

// GuardTerms is what the hook's outbound terms check (SP-044) takes from
// the files a guard decision read (RC-116), so the two judge one policy
// and one set of switches.
type GuardTerms struct {
	// Known is set when the policy is known: the snapshot was read and
	// verified, or nothing was ever applied. When it is not, the check
	// cannot know the terms and refuses an outbound call.
	Known bool
	// State is the terms layer's state; "" when it could not be read,
	// which counts as on.
	State string
	// Policy is the applied policy, nil for none; Home expands the ~ in
	// its terms files.
	Policy *Resolved
	Home   string
	// Rules are the operator's outbound rules (SP-075); nil is the
	// baseline. RulesBroken is set when orchd could not read them: the
	// check then refuses every call they may have covered.
	Rules       *OutboundRules
	RulesBroken bool
	// RulesRev is the rules' revision, which the hook hands to orchd with
	// the call (guard_rules_rev, D-66).
	RulesRev int64
}

// Decide decides in from the files. It never fails: what it cannot read
// becomes a rule, refused while the guard enforces. A cwd-changed event
// reports a move that already happened, so it is never refused.
func (f GuardFiles) Decide(ctx context.Context, in GuardInput) GuardResult {
	r := f.decide(ctx, in)
	if in.Kind == EventCwdChanged {
		r.Verdict, r.Enforced = Allow, false
	}
	return r
}

func (f GuardFiles) decide(ctx context.Context, in GuardInput) GuardResult {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	snap, err := ReadGuardSnapshot(f.Dir)
	if err != nil {
		return f.unreadable(ctx, err, now())
	}
	mode := snap.Mode(now())
	if f.OnMode != nil {
		f.OnMode(mode)
	}
	terms := GuardTerms{State: snap.TermsState(now()), Home: snap.Home, Rules: snap.Rules}
	if snap.Broken {
		return withTerms(fixed(mode, Refuse, RulePolicyUnreadable, snap.Revision), terms)
	}
	var res *Resolved
	if snap.Policy != nil {
		// Reason: orchd validated the policy at apply; the git-tree rule
		// is about terms files on disk, which must not undo an apply.
		r, verr := Validate(*snap.Policy, Env{Home: snap.Home, InGitTree: func(string) bool { return false }})
		if verr != nil {
			return withTerms(fixed(mode, Refuse, RulePolicyUnreadable, snap.Revision), terms)
		}
		res = r
	}
	terms.Known, terms.Policy, terms.RulesBroken, terms.RulesRev = true, res, snap.RulesBroken, snap.RulesRev
	prot := snap.Protection
	switch {
	case !res.Configured():
		// Reason: without spaces nothing is guarded (INV-10), so a
		// protection orchd could not spell (no home) must not refuse.
		prot = Protection{}
	case len(prot.Write) == 0:
		// Reason: orchd always protects its own directories; a snapshot
		// without them was not written by this orchd, and enforcing a
		// policy without self-protection would let an agent switch the
		// guard off.
		return withTerms(fixed(mode, Refuse, RulePolicyUnreadable, snap.Revision), terms)
	}
	pol, err := NewGuardPolicyProtecting(res, prot)
	if err != nil {
		return withTerms(fixed(mode, Refuse, RulePolicyUnreadable, snap.Revision), terms)
	}
	var pin GuardPin
	if res.Configured() {
		// Reason: an unreadable pin is a missing pin; Decide refuses it
		// while enforcing (pin_missing).
		if p, perr := ReadGuardPin(f.Dir, in.SessionID); perr == nil {
			pin = GuardPin{Found: true, Space: p.Space}
		}
	}
	ev := GuardEvent{Kind: in.Kind, Tool: in.Tool, Cwd: in.Cwd}
	if in.Kind == EventCwdChanged {
		ev.Targets.CdDest = []string{in.NewCwd}
	} else if res.Configured() {
		ev.Targets = Targets(in.Tool, in.Input, in.Cwd)
	}
	d := Decide(pol, pin, GuardEnforcement{Guard: mode}, ev)
	return GuardResult{GuardDecision: d, Revision: snap.Revision, Terms: terms}
}

// withTerms is r with the terms check's inputs.
func withTerms(r GuardResult, t GuardTerms) GuardResult {
	r.Terms = t
	return r
}

// unreadable decides when the snapshot cannot be read. Without an orch.db
// that ever applied a policy, a missing snapshot, or one of another version
// (an orchd of another release wrote it), means nothing is configured, and
// the call is allowed (INV-10). Otherwise the guard's switch is read from orch.db: off or log
// allow, anything else, or a store that cannot be read, refuses.
func (f GuardFiles) unreadable(ctx context.Context, cause error, now time.Time) GuardResult {
	applied, err := HasApplied(ctx, f.DB)
	// Reason: a damaged snapshot still refuses; another version is what
	// every install without spaces holds after an upgrade until orchd
	// rewrites it.
	if err == nil && !applied && (errors.Is(cause, os.ErrNotExist) || errors.Is(cause, errSnapshotVersion)) {
		// Nothing was ever applied, so no space has terms.
		return withTerms(fixed(GuardBlockOtherJobs, Allow, RuleNoPolicy, 0), GuardTerms{Known: true})
	}
	mode := GuardStrict
	var terms GuardTerms
	if sw, serr := ReadEnforcement(ctx, f.DB); serr == nil {
		mode = GuardMode(sw.At(now).State(LayerGuard))
		// Reason: as for the guard's switch, so an operator who switched
		// the terms layer off is not refused while the snapshot is gone.
		terms.State = sw.At(now).State(LayerTerms)
	}
	return withTerms(fixed(mode, Refuse, RulePolicyUnreadable, 0), terms)
}

// fixed is the decision of a rule decided outside Decide: would takes
// effect only in an enforcing mode.
func fixed(mode GuardMode, would Verdict, rule GuardRule, rev int64) GuardResult {
	switch mode {
	case GuardOff, GuardLog, GuardBlockOtherJobs, GuardStrict:
	case "":
		mode = GuardBlockOtherJobs
	default:
		mode = GuardStrict
	}
	d := GuardDecision{Verdict: Allow, Would: would, Rule: rule, Mode: mode}
	if mode.Enforcing() {
		d.Verdict, d.Enforced = would, true
	}
	return GuardResult{GuardDecision: d, Revision: rev}
}

// TimedOut is the decision when deciding took too long; mode is the
// guard's mode when it is known, "" when not (then it enforces).
func TimedOut(mode GuardMode) GuardResult {
	return fixed(mode, Refuse, RuleGuardTimeout, 0)
}

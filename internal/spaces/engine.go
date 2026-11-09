package spaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Drift reasons, as Status reports them.
const (
	// DriftDraft: a saved draft differs from the applied revision.
	DriftDraft = "draft"
	// DriftChanged: the applied revision in orch.db no longer matches what
	// orchd enforces, so it was changed outside orchd.
	DriftChanged = "changed_outside_orchd"
)

// ErrBroken is returned by Current when applies exist but the newest no
// longer verifies. Callers that enforce must fail closed.
var ErrBroken = errors.New("the applied spaces policy does not verify")

// Status is the spaces line of orch status. Error never names a space, root
// or org: Status is open to every caller.
type Status struct {
	Configured      bool
	AppliedRevision int64
	// DraftRevision is the newest saved revision when it differs from the
	// applied one, else 0.
	DraftRevision int64
	Drift         []string
	Error         string
}

// Engine holds the policy orchd enforces. Only Apply changes it; anything
// written to orch.db by other means shows as drift and changes nothing.
type Engine struct {
	db  *sql.DB
	env Env
	now func() time.Time

	mu      sync.RWMutex
	applied *Applied
	// broken says why the newest apply did not verify at startup; empty
	// when it did or nothing was applied.
	broken string
	// checked caches whether a draft validates, by revision and hash, so
	// orch status does not validate the same draft on every call.
	checked struct {
		number int64
		sha256 string
		valid  bool
	}
}

// Open loads the newest applied revision from db.
func Open(ctx context.Context, db *sql.DB, env Env) (*Engine, error) {
	e := &Engine{db: db, env: env, now: time.Now}
	applied, err := LoadApplied(ctx, db, env)
	switch {
	case errors.Is(err, ErrTampered):
		e.broken = "the newest applied spaces revision does not verify, so no space is readable; apply a revision again"
	case err != nil:
		return nil, err
	}
	e.applied = applied
	return e, nil
}

// Current returns the enforced policy: nil for none (the implicit Shared
// space), or ErrBroken when the newest apply did not verify.
func (e *Engine) Current() (*Resolved, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch {
	case e.broken != "":
		return nil, ErrBroken
	case e.applied == nil:
		return nil, nil
	}
	return e.applied.Resolved, nil
}

// CurrentRevision is Current with the number of the applied revision it
// enforces, 0 when none is applied.
func (e *Engine) CurrentRevision() (*Resolved, int64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch {
	case e.broken != "":
		return nil, 0, ErrBroken
	case e.applied == nil:
		return nil, 0, nil
	}
	return e.applied.Resolved, e.applied.Revision.Number, nil
}

// SaveDraft stores p as a new draft and returns it with its problems, if any.
// byOperator records whether the operator saved it rather than an agent
// session.
func (e *Engine) SaveDraft(ctx context.Context, p Policy, by string, byOperator bool) (Revision, Problems, error) {
	r, err := SaveDraft(ctx, e.db, p, by, byOperator, e.now())
	if err != nil {
		return Revision{}, nil, err
	}
	return r, problemsOf(Validate(p, e.env)), nil
}

// Apply enforces revision reviewed from now on. acceptAgentDraft confirms
// the operator reviewed a draft an agent session saved; confirmOrgs names
// GitHub orgs whose changed identity the operator accepts. Every bound org is
// looked up on GitHub first. On any error the enforced policy is unchanged.
func (e *Engine) Apply(ctx context.Context, reviewed int64, by string, acceptAgentDraft bool, confirmOrgs []string) (Revision, error) {
	ids, err := e.resolveOrgs(ctx, reviewed, acceptAgentDraft)
	if err != nil {
		return Revision{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := Apply(ctx, e.db, reviewed, by, acceptAgentDraft, e.now(), e.env, ids, confirmOrgs)
	if err != nil {
		return Revision{}, err
	}
	e.applied, e.broken = &a, ""
	return a.Revision, nil
}

// Diff is the applied revision, the pending draft if any, the draft's
// problems, and the applied GitHub orgs awaiting the operator's confirmation.
type Diff struct {
	Applied  *Revision
	Draft    *Revision
	Changes  []Change
	Problems Problems
	// UnconfirmedOrgs are bound orgs whose new content goes to quarantine.
	UnconfirmedOrgs []string
}

// Diff compares the enforced revision with the newest draft.
func (e *Engine) Diff(ctx context.Context) (Diff, error) {
	e.mu.RLock()
	applied := e.applied
	e.mu.RUnlock()
	var d Diff
	var from Policy
	var err error
	if applied != nil {
		r := applied.Revision
		d.Applied, from = &r, applied.Resolved.Policy
	}
	if d.UnconfirmedOrgs, err = e.UnconfirmedOrgs(ctx); err != nil {
		return Diff{}, err
	}
	latest, err := Latest(ctx, e.db)
	if err != nil {
		return Diff{}, err
	}
	if latest == nil || (applied != nil && latest.Number == applied.Revision.Number) {
		return d, nil
	}
	d.Draft = latest
	to, err := Parse(latest.Doc)
	if err != nil {
		d.Problems = Problems{{Rule: RuleUnparseable, Message: err.Error()}}
		return d, nil
	}
	d.Changes = Compare(from, to)
	d.Problems = problemsOf(Validate(to, e.env))
	return d, nil
}

// Status reports the applied and pending revisions and any drift.
func (e *Engine) Status(ctx context.Context) (Status, error) {
	e.mu.RLock()
	applied, broken := e.applied, e.broken
	e.mu.RUnlock()
	st := Status{Error: broken}
	if applied != nil {
		st.AppliedRevision = applied.Revision.Number
		st.Configured = applied.Resolved.Configured()
	}
	unconfirmed, err := e.UnconfirmedOrgs(ctx)
	if err != nil {
		return Status{}, err
	}
	if n := len(unconfirmed); n > 0 {
		st.Error = joinErr(st.Error, fmt.Sprintf("%d bound GitHub org(s) not confirmed, so their new content goes to quarantine; run 'orch spaces diff'", n))
	}
	latest, err := Latest(ctx, e.db)
	if err != nil {
		return Status{}, err
	}
	if latest != nil && (applied == nil || latest.Number != applied.Revision.Number) {
		st.DraftRevision = latest.Number
		if applied == nil || latest.SHA256 != applied.Revision.SHA256 {
			st.Drift = append(st.Drift, DriftDraft)
		}
		if !e.validates(latest) {
			st.Error = joinErr(st.Error, fmt.Sprintf("draft revision %d does not validate; run 'orch spaces diff'", latest.Number))
		}
	}
	changed, err := e.changedOutside(ctx, applied, broken != "")
	if err != nil {
		return Status{}, err
	}
	if changed {
		st.Drift = append(st.Drift, DriftChanged)
	}
	return st, nil
}

// validates reports whether r parses and validates, remembering the answer
// for the newest revision.
func (e *Engine) validates(r *Revision) bool {
	e.mu.RLock()
	c := e.checked
	e.mu.RUnlock()
	if c.number == r.Number && c.sha256 == r.SHA256 {
		return c.valid
	}
	p, err := Parse(r.Doc)
	valid := err == nil && problemsOf(Validate(p, e.env)) == nil
	e.mu.Lock()
	e.checked.number, e.checked.sha256, e.checked.valid = r.Number, r.SHA256, valid
	e.mu.Unlock()
	return valid
}

// changedOutside reports whether orch.db's applied revision differs from the
// one in memory: a newer row orchd did not write, or the enforced revision's
// document edited in place.
func (e *Engine) changedOutside(ctx context.Context, applied *Applied, broken bool) (bool, error) {
	newest, err := newestApplied(ctx, e.db)
	if err != nil {
		return false, err
	}
	if applied == nil {
		// A broken engine already reports its own error.
		return newest != nil && !broken, nil
	}
	if newest == nil || newest.revision != applied.Revision.Number || newest.sha256 != applied.Revision.SHA256 {
		return true, nil
	}
	r, err := Get(ctx, e.db, applied.Revision.Number)
	if err != nil {
		return true, nil
	}
	return r.SHA256 != applied.Revision.SHA256 || Hash(r.Doc) != applied.Revision.SHA256, nil
}

func problemsOf(_ *Resolved, err error) Problems {
	var ps Problems
	if errors.As(err, &ps) {
		return ps
	}
	return nil
}

func joinErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

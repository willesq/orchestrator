package daemon

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The operator's outbound rules (spaces SP-075): which baseline rules of
// the outbound terms check are on, and the stricter rules the operator
// added. orchd holds them, writes each change to orch.db first, logs it with
// its time and caller (never a rule's value, INV-07) and puts them in the
// guard snapshot, from which the hook's check reads them. See
// docs/internals/daemon.md "Outbound rules".

// termsRules serves the rules in orch.db.
type termsRules struct {
	db  *sql.DB
	now func() time.Time
	// emit reports a change; nil appends it to the spaces event stream.
	emit func(spaces.RulesChange)
	// onChange, when set, is given the rules after a change, under mu:
	// orchd rewrites the guard's snapshot with them.
	onChange func(rules spaces.StoredRules, broken bool)

	// mu guards cur and problem, and orders changes.
	mu  sync.Mutex
	cur spaces.StoredRules
	// problem says why orch.db's rules could not be read; while it is
	// set the check refuses outbound calls.
	problem string
}

// newTermsRules serves the rules in db; nil when there is no store.
func newTermsRules(ctx context.Context, db *sql.DB) *termsRules {
	if db == nil {
		return nil
	}
	t := &termsRules{db: db, now: time.Now}
	cur, problem, err := spaces.LoadOutboundRules(ctx, db)
	if err != nil {
		problem = "could not read the outbound rules; outbound calls are refused until they can be: " + err.Error()
	}
	if problem != "" {
		log.Printf("orchd: outbound rules: %s", problem)
	}
	t.cur, t.problem = cur, problem
	return t
}

// current returns the rules orchd applies and why they cannot be trusted,
// if so. Without a store they are the baseline.
func (t *termsRules) current() (spaces.StoredRules, string) {
	if t == nil {
		return spaces.StoredRules{}, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur, t.problem
}

// unreadableRulesRev is recorded while orchd holds no readable rules and
// no stored revision to name; a replay cannot judge with it.
const unreadableRulesRev = -1

// revision is the rules revision orchd applies; 0 for the baseline or
// without a store. Rules that cannot be read keep their stored revision
// (which does not verify), or unreadableRulesRev when none could be read,
// so a replay never judges such a call under the baseline.
func (t *termsRules) revision() int64 {
	cur, problem := t.current()
	if problem != "" && cur.Rev == 0 {
		return unreadableRulesRev
	}
	return cur.Rev
}

// change applies one change as by: it stores the new rules, then logs and
// announces the change. changed is false when nothing changed. Rules that
// could not be read are replaced: the change starts from the baseline.
func (t *termsRules) change(ctx context.Context, action spaces.RuleAction, id, kind, value, by string) (spaces.RulesChange, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	base := t.cur.Rules
	if t.problem != "" {
		base = spaces.OutboundRules{}
	}
	next, c, changed, err := spaces.ChangeOutboundRules(base, action, id, kind, value)
	if err != nil || !changed && t.problem == "" {
		return c, false, err
	}
	c.By, c.At = by, t.now()
	c, err = spaces.WriteOutboundRules(ctx, t.db, c, next)
	if err != nil {
		return c, false, err
	}
	// orch.db now holds what orchd applies.
	t.cur = spaces.StoredRules{Rules: next, Rev: c.Rev, At: c.At, By: by}
	t.problem = ""
	if t.onChange != nil {
		t.onChange(t.cur, false)
	}
	summary := c.Describe()
	log.Printf("orchd: outbound rules: %s", summary)
	if t.emit != nil {
		t.emit(c)
		return c, true, nil
	}
	if err := events.Append(paths.EventsDir(), spacesEventStream, events.Event{
		Kind: "info", Source: "enforcement", Summary: summary, Timestamp: c.At.UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("orchd: outbound rules: record event: %v", err)
	}
	return c, true, nil
}

// replayRules gives a replay the rules of the revision each event was
// recorded with (D-66); nil (judge none) without a store.
func (t *termsRules) replayRules(ctx context.Context) (spaces.ReplayRules, error) {
	if t == nil {
		return nil, nil
	}
	tl, err := spaces.LoadRulesTimeline(ctx, t.db)
	if err != nil {
		return nil, err
	}
	return tl.Rev, nil
}

// setTermsRules takes the rules after a change and rewrites the snapshot.
// It runs under the rules' lock, so it must not call back into them.
func (s *server) setTermsRules(r spaces.StoredRules, broken bool) {
	g := s.guard
	if g == nil {
		return
	}
	g.takeRules(r, broken)
	s.writeGuardSnapshot()
}

// changeOutboundRule makes one change for the RPC method, as the operator
// only (INV-06): callers descended from a harness or orchd are refused.
func (s *server) changeOutboundRule(ctx context.Context, method string, action spaces.RuleAction, id, kind, value string) (spaces.RulesChange, bool, error) {
	if err := refuseNonOperator(ctx, method, "change the outbound rules"); err != nil {
		return spaces.RulesChange{}, false, err
	}
	if s.termsRules == nil {
		return spaces.RulesChange{}, false, grpcstatus.Error(codes.Unavailable, "the outbound rules are unavailable")
	}
	c, changed, err := s.termsRules.change(ctx, action, id, kind, value, callerLabel(ctx))
	switch {
	case errors.Is(err, spaces.ErrUnknownRule):
		return c, false, grpcstatus.Error(codes.NotFound, err.Error())
	case errors.Is(err, spaces.ErrRuleLoosens), errors.Is(err, spaces.ErrBadRule):
		// Reason: these errors never quote a rule's value.
		return c, false, grpcstatus.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		log.Printf("orchd: outbound rules: %s rule %s: %v", action, c.RuleID, err)
		return c, false, grpcstatus.Error(codes.Internal, "the outbound rules could not be stored")
	}
	return c, changed, nil
}

// OutboundRuleView is one rule as a list shows it: never its value.
type OutboundRuleView struct {
	ID       string
	Kind     string // "baseline" or the kind of an added rule
	On       bool
	Loosens  bool
	Summary  string
	Baseline bool
}

// outboundRuleViews lists the baseline rules, then the added ones, without
// any value.
func outboundRuleViews(r spaces.OutboundRules) []OutboundRuleView {
	var out []OutboundRuleView
	for _, b := range spaces.OutboundBaseline {
		out = append(out, OutboundRuleView{ID: string(b.ID), Kind: "baseline", On: r.On(b.ID), Loosens: b.Loosens,
			Summary: b.Summary, Baseline: true})
	}
	for _, u := range r.User {
		out = append(out, OutboundRuleView{ID: u.ID, Kind: string(u.Kind), On: !u.Off, Summary: userRuleSummary(u.Kind)})
	}
	return out
}

// userRuleSummary says what an added rule of kind does, without its value.
func userRuleSummary(k spaces.UserRuleKind) string {
	switch k {
	case spaces.UserRuleCommand:
		return "checks a command's words as text sent to a destination that holds no space"
	case spaces.UserRuleDestination:
		return "treats an owner or repo as holding no space, so no space's terms may reach it"
	case spaces.UserRuleFailClosed:
		return "refuses a tool call whose tool name or text matches a pattern"
	}
	return ""
}

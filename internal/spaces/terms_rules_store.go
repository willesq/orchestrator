package spaces

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The outbound rules in orch.db (SP-075, migration 0030): one row per change,
// newest last, holding the whole rule set after it with its hash. The newest
// row applies. A row whose hash or rules do not verify is damaged, so orchd
// refuses outbound calls until the operator changes the rules again. The
// hash finds damage and careless edits, not a writer who recomputes it: the
// guard keeps agents from writing orch.db (RC-600).

// RuleAction is what a change did.
type RuleAction string

// The changes the operator can make.
const (
	RuleEnable  RuleAction = "enable"
	RuleDisable RuleAction = "disable"
	RuleAdd     RuleAction = "add"
	RuleRemove  RuleAction = "remove"
)

// RulesChange is one change as the log shows it: never the rule's value.
type RulesChange struct {
	Rev    int64
	At     time.Time
	By     string
	Action RuleAction
	RuleID string
}

// Describe is the change in one line, for orchd's log and event stream.
func (c RulesChange) Describe() string {
	return fmt.Sprintf("outbound rule %s: %s by %s (rules revision %d)", c.RuleID, c.Action, c.By, c.Rev)
}

// StoredRules is the rule set orch.db holds and the change that made it.
// Rev is 0 when the operator never changed the rules: the baseline.
type StoredRules struct {
	Rules OutboundRules
	Rev   int64
	At    time.Time
	By    string
	// Broken marks a stored row that does not verify (RulesTimeline).
	Broken bool
}

// rulesDoc is the stored JSON of r and its hash.
func rulesDoc(r OutboundRules) (string, string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}

// LoadOutboundRules reads the newest rule set. problem is set, with the
// baseline returned, when the newest row does not verify; the caller then
// treats the rules as unreadable. An error is a store that cannot be read.
func LoadOutboundRules(ctx context.Context, q querier) (s StoredRules, problem string, err error) {
	row := q.QueryRowContext(ctx, `SELECT rev, at, by, rules, hash FROM spaces_terms_rules ORDER BY rev DESC LIMIT 1`)
	return scanRules(row)
}

// RulesTimeline is every stored rule set, oldest first, for a replay to
// judge each event with the rules revision it was recorded with (D-66).
type RulesTimeline []StoredRules

// LoadRulesTimeline reads every stored rule set. A row that does not verify
// is kept as broken: an event recorded under it cannot be judged.
func LoadRulesTimeline(ctx context.Context, q querier) (RulesTimeline, error) {
	rows, err := q.QueryContext(ctx, `SELECT rev, at, by, rules, hash FROM spaces_terms_rules ORDER BY rev`)
	if err != nil {
		return nil, fmt.Errorf("read the outbound rules: %w", err)
	}
	defer rows.Close()
	var out RulesTimeline
	for rows.Next() {
		var rev int64
		var at, by, doc, hash string
		if err := rows.Scan(&rev, &at, &by, &doc, &hash); err != nil {
			return nil, fmt.Errorf("read the outbound rules: %w", err)
		}
		s, problem := verifyRules(rev, at, by, doc, hash)
		s.Broken = problem != ""
		out = append(out, s)
	}
	return out, rows.Err()
}

// ErrRulesUnreadable is returned for an event recorded under rules that do
// not verify.
var ErrRulesUnreadable = errors.New("the outbound rules in force then cannot be read")

// Rev is rules revision rev; nil (the baseline) for 0. A revision the
// store does not hold, or one that does not verify, cannot be judged with.
func (tl RulesTimeline) Rev(rev int64) (*OutboundRules, error) {
	if rev == 0 {
		return nil, nil
	}
	for i := range tl {
		if tl[i].Rev != rev {
			continue
		}
		if tl[i].Broken {
			return nil, ErrRulesUnreadable
		}
		r := tl[i].Rules
		return &r, nil
	}
	return nil, ErrRulesUnreadable
}

// rulesTime is how a change's time is stored. Reason: fixed width, so
// times compare as text in SQL.
const rulesTime = "2006-01-02T15:04:05.000000000Z"

func scanRules(row *sql.Row) (StoredRules, string, error) {
	var rev int64
	var at, by, doc, hash string
	switch err := row.Scan(&rev, &at, &by, &doc, &hash); {
	case errors.Is(err, sql.ErrNoRows):
		return StoredRules{}, "", nil
	case err != nil:
		return StoredRules{}, "", fmt.Errorf("read the outbound rules: %w", err)
	}
	s, problem := verifyRules(rev, at, by, doc, hash)
	return s, problem, nil
}

// verifyRules checks a stored row: its hash, time and rules.
func verifyRules(rev int64, at, by, doc, hash string) (StoredRules, string) {
	const bad = "the outbound rules in orch.db do not verify (changed outside orchd?); outbound calls are refused until the operator changes the rules"
	sum := sha256.Sum256([]byte(doc))
	var r OutboundRules
	t, terr := time.Parse(rulesTime, at)
	if hex.EncodeToString(sum[:]) != hash || terr != nil || json.Unmarshal([]byte(doc), &r) != nil || r.Validate() != nil {
		return StoredRules{Rev: rev}, bad
	}
	return StoredRules{Rules: r, Rev: rev, At: t, By: by}, ""
}

// WriteOutboundRules stores r, the rule set after change c, as a new row and
// returns c with its revision.
func WriteOutboundRules(ctx context.Context, db *sql.DB, c RulesChange, r OutboundRules) (RulesChange, error) {
	if err := r.Validate(); err != nil {
		return c, err
	}
	doc, hash, err := rulesDoc(r)
	if err != nil {
		return c, err
	}
	c.At = c.At.UTC()
	res, err := db.ExecContext(ctx, `INSERT INTO spaces_terms_rules (at, by, action, rule_id, rules, hash) VALUES (?, ?, ?, ?, ?, ?)`,
		c.At.Format(rulesTime), c.By, string(c.Action), c.RuleID, doc, hash)
	if err != nil {
		return c, fmt.Errorf("store the outbound rules: %w", err)
	}
	c.Rev, err = res.LastInsertId()
	return c, err
}

// OutboundRulesHistory returns the newest limit changes, newest first,
// without any rule's value.
func OutboundRulesHistory(ctx context.Context, q querier, limit int) ([]RulesChange, error) {
	rows, err := q.QueryContext(ctx, `SELECT rev, at, by, action, rule_id FROM spaces_terms_rules ORDER BY rev DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read the outbound rules' log: %w", err)
	}
	defer rows.Close()
	var out []RulesChange
	for rows.Next() {
		var c RulesChange
		var at, action string
		if err := rows.Scan(&c.Rev, &at, &c.By, &action, &c.RuleID); err != nil {
			return nil, fmt.Errorf("read the outbound rules' log: %w", err)
		}
		c.At, _ = time.Parse(rulesTime, at)
		c.Action = RuleAction(action)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChangeOutboundRules applies one change to cur: enable or disable rule id,
// add a rule of kind with value, or remove rule id. It returns the new rule
// set and the change's log entry (no value; for an add, the new rule's id).
// changed is false when nothing would change.
func ChangeOutboundRules(cur OutboundRules, action RuleAction, id, kind, value string) (next OutboundRules, c RulesChange, changed bool, err error) {
	c = RulesChange{Action: action, RuleID: id}
	switch action {
	case RuleEnable, RuleDisable:
		next, changed, err = cur.Switch(id, action == RuleEnable)
	case RuleAdd:
		var u UserRule
		next, u, err = cur.Add(kind, value)
		c.RuleID, changed = u.ID, err == nil
	case RuleRemove:
		next, err = cur.Remove(id)
		changed = err == nil
	default:
		err = fmt.Errorf("%w: unknown change", ErrBadRule)
	}
	if err != nil {
		return cur, RulesChange{}, false, err
	}
	return next, c, changed, nil
}

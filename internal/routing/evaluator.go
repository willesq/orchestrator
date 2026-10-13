package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Evaluator re-runs discovery and routing evaluation on a cadence. It proposes
// rather than applies unless AutoApply is set, and reports through Emit, which
// lands in the next session's digest.
type Evaluator struct {
	Router *Router
	// Refresh runs model discovery and returns the refs it saw for the first time.
	Refresh   func(ctx context.Context) ([]string, error)
	Emit      func(summary string) error
	AutoApply bool
	Every     time.Duration
	// StatePath remembers the last run, so a restart of orchd does not re-run
	// early, and the last report, so an unchanged proposal is not re-announced.
	StatePath string
	Now       func() time.Time
}

type evaluatorState struct {
	LastRun     int64  `json:"last_run"`
	LastAttempt int64  `json:"last_attempt"`
	LastReport  string `json:"last_report"` // hash of the last emitted summary
	// Pending holds models discovered by a run that could not report them, so
	// they are not lost: the next run sees them as already known.
	Pending []string `json:"pending,omitempty"`
}

// retryAfter spaces out retries of a failed run, which re-reads every harness
// cache.
const retryAfter = 6 * time.Hour

// maxListed bounds how many changes a summary names.
const maxListed = 5

// Tick runs one evaluation if the cadence is due.
func (e *Evaluator) Tick(ctx context.Context) error {
	now := e.Now()
	st := e.load()
	// A time in the future (a skewed clock, or a tampered file) must not stop
	// the evaluator for good.
	if st.LastRun > now.Unix() {
		st.LastRun = 0
	}
	if st.LastAttempt > now.Unix() {
		st.LastAttempt = 0
	}
	if st.LastRun != 0 && now.Sub(time.Unix(st.LastRun, 0)) < e.Every {
		return nil
	}
	if st.LastAttempt > st.LastRun && now.Sub(time.Unix(st.LastAttempt, 0)) < retryAfter {
		return nil
	}
	st.LastAttempt = now.Unix()
	var errs []error
	discovered, refreshErr := e.Refresh(ctx)
	if refreshErr != nil {
		// Discovery failing for one provider still leaves the rest to evaluate.
		errs = append(errs, fmt.Errorf("discovery: %w", refreshErr))
	}
	st.Pending = union(st.Pending, discovered)
	summary, applied, err := e.evaluate(ctx, st.Pending, refreshErr != nil, now)
	if err != nil {
		errs = append(errs, err)
		if serr := e.save(st); serr != nil {
			errs = append(errs, serr)
		}
		return errors.Join(errs...)
	}
	st.LastRun = now.Unix()
	if summary != "" {
		// An applied change is always announced; only a repeated proposal is not.
		if h := digest(summary); applied || h != st.LastReport {
			if err := e.Emit(summary); err != nil {
				errs = append(errs, err)
			} else {
				st.LastReport, st.Pending = h, nil
			}
		} else {
			st.Pending = nil
		}
	}
	if err := e.save(st); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (e *Evaluator) evaluate(ctx context.Context, discovered []string, refreshFailed bool, now time.Time) (summary string, applied bool, err error) {
	cands, err := e.Router.Candidates(ctx)
	if err != nil {
		return "", false, err
	}
	current, err := e.Router.Store.Table(ctx)
	if err != nil {
		return "", false, err
	}
	ev, err := e.Router.Store.Evidence(ctx)
	if err != nil {
		return "", false, err
	}
	proposed := Propose(e.Router.Policy, cands, ev, now.Unix())
	var parts []string
	if n := len(discovered); n > 0 {
		parts = append(parts, fmt.Sprintf("%d new model(s) discovered (%s); `orch model enable` to use one",
			n, strings.Join(firstN(discovered, maxListed), ", ")))
	}
	changes := pendingChanges(current, proposed)
	if len(changes) == 0 {
		return strings.Join(parts, "; "), false, nil
	}
	listed := describe(changes)
	// Unattended changes are held to more than a reviewed one: never from a run
	// whose discovery failed (a provider may only look gone), and never one that
	// drops a route, which would leave a kind with fewer fallbacks.
	hold := ""
	switch {
	case !e.AutoApply:
	case refreshFailed:
		hold = " (not auto-applied: discovery failed this run)"
	case removesRoute(changes):
		hold = " (not auto-applied: it removes routes)"
	default:
		if _, err := e.Router.Store.Apply(ctx, proposed, now.Unix()); err != nil {
			return "", false, err
		}
		parts = append(parts, fmt.Sprintf("applied %d routing change(s) (routing.auto_apply): %s; `orch routing rollback` undoes it",
			len(changes), listed))
		return strings.Join(parts, "; "), true, nil
	}
	parts = append(parts, fmt.Sprintf("%d routing change(s) proposed%s: %s; review with `orch routing evaluate`",
		len(changes), hold, listed))
	return strings.Join(parts, "; "), false, nil
}

func removesRoute(changes []Change) bool {
	for _, c := range changes {
		if c.To == "" {
			return true
		}
	}
	return false
}

func describe(changes []Change) string {
	var out []string
	for _, c := range changes {
		from, to := c.From, c.To
		if from == "" {
			from = "none"
		}
		if to == "" {
			to = "none"
		}
		out = append(out, fmt.Sprintf("%s#%d %s→%s", c.Kind, c.Rank, from, to))
	}
	return strings.Join(firstN(out, maxListed), ", ")
}

// pendingChanges lists differing ranks, ignoring kinds that are pinned (apply
// would leave them alone, so proposing them is noise).
func pendingChanges(current, proposed []Route) []Change {
	pinned := map[string]bool{}
	for _, r := range current {
		if r.Pinned {
			pinned[r.Kind] = true
		}
	}
	var out []Change
	for _, c := range Diff(current, proposed) {
		if !pinned[c.Kind] {
			out = append(out, c)
		}
	}
	return out
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("+%d more", len(s)-n))
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (e *Evaluator) load() evaluatorState {
	var st evaluatorState
	b, err := os.ReadFile(e.StatePath)
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (e *Evaluator) save(st evaluatorState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.StatePath), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	tmp := e.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.StatePath)
}

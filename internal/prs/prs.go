// Package prs watches the user's GitHub scopes (their account and the orgs
// they opted in to) for open PRs and issues via the gh CLI, caches them, and
// emits events on CI/review/mergeable transitions of the user's own PRs.
// `orch prs` reads the cache and never shells out — the babysitter (run inside
// orchd on a cadence) is the only thing that calls gh.
package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/github"
)

// Kinds of cached item.
const (
	KindPR    = "pr"
	KindIssue = "issue"
)

// PR is the cached state of one pull request or issue (Kind).
type PR struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Repo  string `json:"repo"`
	// Kind is KindPR or KindIssue; empty in caches written before issues
	// were tracked, which held only PRs.
	Kind           string `json:"kind,omitempty"`
	Number         int    `json:"number,omitempty"`
	Author         string `json:"author,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"` // RFC 3339, from GitHub
	CIState        string `json:"ci_state"`             // success | failure | pending | none
	Checks         string `json:"checks,omitempty"`     // synopsis, such as "3 passed, 1 skipped, 0 failed"
	ReviewDecision string `json:"review_decision"`      // APPROVED | CHANGES_REQUESTED | ...
	Mergeable      string `json:"mergeable"`
	IsDraft        bool   `json:"is_draft"`
	// Reasons says why the item is watched (ReasonAuthored, ReasonAssigned,
	// ReasonInvolved, ReasonReviewRequested, ReasonAllOpen).
	Reasons []string `json:"reasons,omitempty"`
}

// IsPR reports whether the item is a pull request.
func (pr PR) IsPR() bool { return pr.Kind == "" || pr.Kind == KindPR }

// Trusted reports whether pr belongs in agent-facing output (orch prs, its
// JSON): a PR the user opened, or one whose review they were asked for. Other
// cached items (mentions, everything in a scope) carry titles anyone who can
// open a PR wrote, so only the app shows them.
func Trusted(pr PR) bool {
	return pr.IsPR() && (pr.Authored() || slices.Contains(pr.Reasons, ReasonReviewRequested))
}

// State is the persisted per-PR snapshot.
type State struct {
	PRs map[string]PR `json:"prs"`
}

// LoadState reads the cache (empty if absent/corrupt).
func LoadState(path string) State {
	s, err := ReadState(path)
	if err != nil {
		return State{PRs: map[string]PR{}}
	}
	return s
}

// ReadState reads the cache: empty when the file does not exist yet, an error
// when it exists but cannot be read or parsed. Callers that report the cache
// to someone use it, so a bad file never passes for "no PRs".
func ReadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{PRs: map[string]PR{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.PRs == nil {
		s.PRs = map[string]PR{}
	}
	return s, nil
}

// SaveState writes the cache atomically (temp file, then rename), so a reader
// never sees a half-written file. Owner-only: it lists private repos' PRs.
func SaveState(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Diff returns an event for each of the user's own PRs whose gated fields
// changed (or is new). PRs that only await the user's review are left out:
// their titles are written by other people, and events reach every agent
// session's context through the SessionStart digest.
func Diff(old, next State) []events.Event {
	var evs []events.Event
	for url, pr := range next.PRs {
		if !pr.IsPR() || !pr.Authored() {
			continue
		}
		prev, existed := old.PRs[url]
		if existed && sameSignature(prev, pr) {
			continue
		}
		evs = append(evs, events.Event{
			Kind:    classify(pr),
			Source:  "pr-babysitter",
			Summary: pr.Repo + ": " + pr.Title + " [ci=" + pr.CIState + " review=" + orNone(pr.ReviewDecision) + "]",
		})
	}
	return evs
}

func sameSignature(a, b PR) bool {
	return a.CIState == b.CIState && a.ReviewDecision == b.ReviewDecision &&
		a.Mergeable == b.Mergeable && a.IsDraft == b.IsDraft && slices.Equal(a.Reasons, b.Reasons)
}

func classify(pr PR) string {
	switch {
	case pr.CIState == "failure" || pr.ReviewDecision == "CHANGES_REQUESTED":
		return "blocked"
	case pr.CIState == "success" && pr.ReviewDecision == "APPROVED":
		return "complete"
	default:
		return "info"
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Filter returns PRs from the state, optionally narrowed to blocked or ready.
func Filter(s State, blocked, ready bool) []PR {
	var out []PR
	for _, pr := range s.PRs {
		if !Trusted(pr) {
			continue
		}
		switch {
		case blocked && !isBlocked(pr):
			continue
		case ready && !isReady(pr):
			continue
		}
		out = append(out, pr)
	}
	return out
}

func isBlocked(pr PR) bool {
	return pr.CIState == "failure" || pr.ReviewDecision == "CHANGES_REQUESTED"
}

func isReady(pr PR) bool {
	return pr.CIState == "success" && pr.ReviewDecision == "APPROVED" && !pr.IsDraft
}

// ghRunner runs the gh CLI; injectable for tests.
type ghRunner func(ctx context.Context, args ...string) ([]byte, error)

// realGH is github.GH, whose errors carry gh's (sanitized) message, so a
// rate-limit failure can be recognized.
func realGH(ctx context.Context, args ...string) ([]byte, error) {
	return github.GH(ctx, args...)
}

// Babysitter polls the watched scopes and maintains the cache + event log.
type Babysitter struct {
	stateFile string
	eventsDir string
	gh        ghRunner
	watch     WatchSource
}

// New builds a babysitter writing to stateFile and eventsDir, watching what
// watch reports.
func New(stateFile, eventsDir string, watch WatchSource) *Babysitter {
	return &Babysitter{stateFile: stateFile, eventsDir: eventsDir, gh: realGH, watch: watch}
}

// Sweep polls the watched scopes, appends events for transitions, and saves
// the new state.
func (b *Babysitter) Sweep(ctx context.Context) error {
	prsList, err := b.poll(ctx)
	if err != nil {
		return err
	}
	next := State{PRs: map[string]PR{}}
	for _, pr := range prsList {
		next.PRs[pr.URL] = pr
	}
	old := LoadState(b.stateFile)
	for _, e := range Diff(old, next) {
		_ = events.Append(b.eventsDir, "pr-babysitter", e)
	}
	return SaveState(b.stateFile, next)
}

func enrich(ctx context.Context, gh ghRunner, pr *PR) {
	out, err := gh(ctx, "pr", "view", pr.URL, "--json", "statusCheckRollup,reviewDecision,mergeable")
	if err != nil {
		return
	}
	var v struct {
		StatusCheckRollup []Check `json:"statusCheckRollup"`
		ReviewDecision    string  `json:"reviewDecision"`
		Mergeable         string  `json:"mergeable"`
	}
	if json.Unmarshal(out, &v) != nil {
		return
	}
	pr.ReviewDecision = v.ReviewDecision
	pr.Mergeable = v.Mergeable
	pr.CIState = RollupCI(v.StatusCheckRollup)
	pr.Checks = CountChecks(v.StatusCheckRollup).String()
}

// Check is one entry of gh's statusCheckRollup: a check run (name, workflow,
// status and conclusion) or a commit status (context and state).
type Check struct {
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Context      string `json:"context"`
	StartedAt    string `json:"startedAt"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	State        string `json:"state"`
}

// CheckCounts is how a PR's checks ended up. Skipped includes neutral.
type CheckCounts struct {
	Passed, Skipped, Failed, Running int
}

// The outcomes of a check, least to most worrying.
const (
	checkPassed = iota
	checkSkipped
	checkRunning
	checkFailed
)

func (c Check) result() string {
	if c.Conclusion != "" {
		return c.Conclusion
	}
	return c.State
}

// outcome sorts a check. A skip passes as intentional: when a PR changes its
// own workflows, whether it was meant is for the review to judge (pr-verdict).
// Anything not known to have ended counts as running, which includes stale: gh
// treats it the same, and a re-run replaces it.
func (c Check) outcome() int {
	switch c.result() {
	case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return checkFailed
	case "SUCCESS":
		return checkPassed
	case "SKIPPED", "NEUTRAL":
		return checkSkipped
	}
	return checkRunning
}

func (c Check) started() time.Time {
	t, err := time.Parse(time.RFC3339, c.StartedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// supersedes reports whether c replaces other, an earlier run of the same check.
func (c Check) supersedes(other Check) bool {
	// A queued re-run has no start time yet but is the newest run. A finished
	// run with none, such as one cancelled while queued, is not.
	cQueued, otherQueued := c.started().IsZero() && c.result() == "", other.started().IsZero() && other.result() == ""
	if cQueued != otherQueued {
		return cQueued
	}
	if !c.started().Equal(other.started()) {
		return c.started().After(other.started())
	}
	// Nothing says which came last, so the worse result stands.
	return c.outcome() > other.outcome()
}

// LatestChecks keeps the newest run of each check. statusCheckRollup returns
// every run on the head commit, including ones a re-run or re-triggered workflow
// replaced; GitHub's checks box and gh pr checks show only the newest. A check is
// its workflow and name, or its context; gh's export has no event to tell apart
// a job run by two triggers. Entries with neither name nor context are all kept.
// A skip never erases a failure: a job that failed and then skipped on a
// re-trigger has not passed.
func LatestChecks(checks []Check) []Check {
	at := make(map[string]int, len(checks))
	out := make([]Check, 0, len(checks))
	ran := make(map[string]Check, len(checks)) // the newest run of each that did not skip
	for _, c := range checks {
		key := "status\x00" + c.Context
		if c.Context == "" {
			key = "run\x00" + c.WorkflowName + "\x00" + c.Name
		}
		if c.Context == "" && c.Name == "" {
			out = append(out, c)
			continue
		}
		if prev, seen := ran[key]; c.outcome() != checkSkipped && (!seen || c.supersedes(prev)) {
			ran[key] = c
		}
		i, seen := at[key]
		switch {
		case !seen:
			at[key] = len(out)
			out = append(out, c)
		case c.supersedes(out[i]):
			out[i] = c
		}
	}
	for key, i := range at {
		if prev, seen := ran[key]; seen && prev.outcome() == checkFailed && out[i].outcome() == checkSkipped {
			out[i] = prev
		}
	}
	return out
}

// ReplacedFailures counts the failed runs LatestChecks dropped. The newer run
// that replaced one is only as trustworthy as its name, which a PR's own workflow
// can copy, so an untrusted author's PR is not approved on it alone.
func ReplacedFailures(checks []Check) int {
	count := func(list []Check) int {
		n := 0
		for _, c := range list {
			if c.outcome() == checkFailed {
				n++
			}
		}
		return n
	}
	return count(checks) - count(LatestChecks(checks))
}

// CountChecks sorts the newest run of each check by outcome.
func CountChecks(checks []Check) CheckCounts {
	var n CheckCounts
	for _, c := range LatestChecks(checks) {
		switch c.outcome() {
		case checkFailed:
			n.Failed++
		case checkPassed:
			n.Passed++
		case checkSkipped:
			n.Skipped++
		default:
			n.Running++
		}
	}
	return n
}

// String is the synopsis shown to the user and to the review agent.
func (n CheckCounts) String() string {
	if n == (CheckCounts{}) {
		return "no checks"
	}
	s := fmt.Sprintf("%d passed, %d skipped, %d failed", n.Passed, n.Skipped, n.Failed)
	if n.Running > 0 {
		s += fmt.Sprintf(", %d running", n.Running)
	}
	return s
}

// RollupCI sums checks up as none, failure, pending or success.
func RollupCI(checks []Check) string {
	n := CountChecks(checks)
	switch {
	case n == (CheckCounts{}):
		return "none"
	case n.Failed > 0:
		return "failure"
	case n.Running > 0:
		return "pending"
	}
	return "success"
}

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

// Check is one entry of gh's statusCheckRollup: a check run (status and
// conclusion) or a commit status (state). An alias, so a literal of the same
// shape is one.
type Check = struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// CheckCounts is how a PR's checks ended up. Skipped includes neutral.
type CheckCounts struct {
	Passed, Skipped, Failed, Running int
}

// CountChecks sorts checks by outcome. A skip passes as intentional: when a PR
// changes its own workflows, whether it was meant is for the review to judge
// (pr-verdict). Only a check that has not finished counts as running.
func CountChecks(checks []Check) CheckCounts {
	var n CheckCounts
	for _, c := range checks {
		concl := c.Conclusion
		if concl == "" {
			concl = c.State
		}
		switch concl {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			n.Failed++
		case "SUCCESS":
			n.Passed++
		case "SKIPPED", "NEUTRAL":
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

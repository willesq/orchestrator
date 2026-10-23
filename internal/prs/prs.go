// Package prs polls the user's open PRs, and PRs awaiting the user's review,
// via the gh CLI, caches their state, and
// emits events on CI/review/mergeable transitions. `orch prs` reads the cache and
// never shells out — the babysitter (run inside orchd on a cadence) is the only
// thing that calls gh.
package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/willesq/orchestrator/internal/events"
)

// PR is the cached state of one pull request.
type PR struct {
	URL            string `json:"url"`
	Title          string `json:"title"`
	Repo           string `json:"repo"`
	CIState        string `json:"ci_state"`        // success | failure | pending | none
	ReviewDecision string `json:"review_decision"` // APPROVED | CHANGES_REQUESTED | ...
	Mergeable      string `json:"mergeable"`
	IsDraft        bool   `json:"is_draft"`
	// Reasons lists which searches found the PR (ReasonAuthored,
	// ReasonReviewRequested).
	Reasons []string `json:"reasons,omitempty"`
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
		if !pr.Authored() {
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

func realGH(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", args...).Output()
}

// Babysitter polls PRs and maintains the cache + event log.
type Babysitter struct {
	stateFile string
	eventsDir string
	gh        ghRunner
}

// New builds a babysitter writing to stateFile and eventsDir.
func New(stateFile, eventsDir string) *Babysitter {
	return &Babysitter{stateFile: stateFile, eventsDir: eventsDir, gh: realGH}
}

// Sweep polls PRs, appends events for transitions, and saves the new state.
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

// discovery is one search the babysitter runs; Reason tags what it finds.
var discovery = []struct {
	Reason string
	Flag   string
}{
	{ReasonAuthored, "--author=@me"},
	// Includes requests made to a team the user is on.
	{ReasonReviewRequested, "--review-requested=@me"},
}

// poll runs the 2-phase gh query: discovery (one search per reason, merged by
// URL) + per-PR enrichment.
func (b *Babysitter) poll(ctx context.Context) ([]PR, error) {
	byURL := map[string]*PR{}
	var order []string
	for _, d := range discovery {
		found, err := b.search(ctx, d.Flag)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			pr, seen := byURL[f.URL]
			if !seen {
				pr = &PR{URL: f.URL, Title: f.Title, Repo: f.Repository.NameWithOwner, IsDraft: f.IsDraft}
				byURL[f.URL] = pr
				order = append(order, f.URL)
			}
			pr.Reasons = append(pr.Reasons, d.Reason)
		}
	}
	prsOut := make([]PR, 0, len(order))
	for _, url := range order {
		pr := byURL[url]
		enrich(ctx, b.gh, pr)
		prsOut = append(prsOut, *pr)
	}
	return prsOut, nil
}

type searchHit struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	IsDraft    bool   `json:"isDraft"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

func (b *Babysitter) search(ctx context.Context, flag string) ([]searchHit, error) {
	// Newest first, so a burst of stale PRs cannot push recent ones past the
	// limit.
	out, err := b.gh(ctx, "search", "prs", flag, "--state=open", "--sort=updated",
		"--json", "url,title,repository,isDraft", "--limit", "50")
	if err != nil {
		return nil, err
	}
	var found []searchHit
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, err
	}
	return found, nil
}

func enrich(ctx context.Context, gh ghRunner, pr *PR) {
	out, err := gh(ctx, "pr", "view", pr.URL, "--json", "statusCheckRollup,reviewDecision,mergeable")
	if err != nil {
		return
	}
	var v struct {
		StatusCheckRollup []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
		ReviewDecision string `json:"reviewDecision"`
		Mergeable      string `json:"mergeable"`
	}
	if json.Unmarshal(out, &v) != nil {
		return
	}
	pr.ReviewDecision = v.ReviewDecision
	pr.Mergeable = v.Mergeable
	pr.CIState = rollupCI(v.StatusCheckRollup)
}

func rollupCI(checks []struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}) string {
	if len(checks) == 0 {
		return "none"
	}
	pending := false
	for _, c := range checks {
		concl := c.Conclusion
		if concl == "" {
			concl = c.State
		}
		switch concl {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED":
			return "failure"
		case "SUCCESS":
		default:
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "success"
}

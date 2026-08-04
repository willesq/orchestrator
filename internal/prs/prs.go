// Package prs polls the user's open PRs via the gh CLI, caches their state, and
// emits events on CI/review/mergeable transitions. `orch prs` reads the cache and
// never shells out — the babysitter (run inside orchd on a cadence) is the only
// thing that calls gh.
package prs

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

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
}

// State is the persisted per-PR snapshot.
type State struct {
	PRs map[string]PR `json:"prs"`
}

// LoadState reads the cache (empty if absent/corrupt).
func LoadState(path string) State {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{PRs: map[string]PR{}}
	}
	var s State
	if json.Unmarshal(b, &s) != nil || s.PRs == nil {
		return State{PRs: map[string]PR{}}
	}
	return s
}

// SaveState writes the cache.
func SaveState(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Diff returns an event for each PR whose gated fields changed (or is new).
func Diff(old, next State) []events.Event {
	var evs []events.Event
	for url, pr := range next.PRs {
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
		a.Mergeable == b.Mergeable && a.IsDraft == b.IsDraft
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

// poll runs the 2-phase gh query: discovery + per-PR enrichment.
func (b *Babysitter) poll(ctx context.Context) ([]PR, error) {
	out, err := b.gh(ctx, "search", "prs", "--author=@me", "--state=open",
		"--json", "url,title,repository,isDraft", "--limit", "50")
	if err != nil {
		return nil, err
	}
	var found []struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		IsDraft    bool   `json:"isDraft"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, err
	}
	prsOut := make([]PR, 0, len(found))
	for _, f := range found {
		pr := PR{URL: f.URL, Title: f.Title, Repo: f.Repository.NameWithOwner, IsDraft: f.IsDraft}
		enrich(ctx, b.gh, &pr)
		prsOut = append(prsOut, pr)
	}
	return prsOut, nil
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

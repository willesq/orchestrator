package reviews

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/prs"
)

// Status is what decides whether a watched PR is ready for review.
type Status struct {
	State     string // OPEN | MERGED | CLOSED
	IsDraft   bool
	Head      string // head commit SHA
	Base      string // base commit SHA: the tip of the branch the PR merges into
	Mergeable string // MERGEABLE | CONFLICTING | UNKNOWN
	CI        string // none | failure | pending | success
	Checks    string // synopsis, such as "3 passed, 1 skipped, 0 failed"
	// ReplacedFailures counts failed runs that a newer run of the check replaced.
	ReplacedFailures int
	Author           string
	// Association is the author's relation to the repo (OWNER, MEMBER,
	// COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE).
	Association string
}

// Ready reports whether the PR may be reviewed: open, not a draft, CI green
// or absent, and free of merge conflicts.
func (s Status) Ready() bool {
	return s.State == "OPEN" && !s.IsDraft && (s.CI == "success" || s.CI == "none") &&
		s.Mergeable != "CONFLICTING" && s.Head != ""
}

// Trusted reports whether the author is the repo's owner, a member or a
// collaborator: someone whose code orch may run unattended in a review.
func (s Status) Trusted() bool {
	switch s.Association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// ApproveReady reports whether orch may approve: ready, and with CI that
// actually passed unless the author is trusted. A fork PR whose workflows
// await a maintainer's approval shows no checks at all, which must not pass
// for green. Checks that all skipped do count: that can be the intent, and the
// review, which is told the counts, judges it.
func (s Status) ApproveReady() bool {
	return s.Ready() && (s.Trusted() || (s.CI == "success" && s.ReplacedFailures == 0))
}

// promptChecks is the CI synopsis for the review, which judges whether the runs
// that replaced a failure are real re-runs.
func (s Status) promptChecks() string {
	switch s.ReplacedFailures {
	case 0:
		return s.Checks
	case 1:
		return s.Checks + "; 1 earlier run failed and a newer run replaced it"
	}
	return fmt.Sprintf("%s; %d earlier runs failed and newer runs replaced them", s.Checks, s.ReplacedFailures)
}

// Waiting says why an open PR is not ready, for the user.
func (s Status) Waiting() string {
	switch {
	case s.IsDraft:
		return "waiting: the PR is a draft"
	case s.CI == "failure":
		return "waiting: CI is failing"
	case s.CI == "pending":
		return "waiting: CI is running"
	case s.Mergeable == "CONFLICTING":
		return "waiting: the PR has merge conflicts"
	}
	return "waiting for the PR to be ready"
}

func sha(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// FetchStatus reads a PR's readiness with one gh call.
func FetchStatus(ctx context.Context, gh github.Runner, url string) (Status, error) {
	out, err := gh(ctx, "pr", "view", url, "--json", "state,isDraft,headRefOid,baseRefOid,mergeable,statusCheckRollup,author")
	if err != nil {
		return Status{}, err
	}
	var v struct {
		State             string      `json:"state"`
		IsDraft           bool        `json:"isDraft"`
		HeadRefOid        string      `json:"headRefOid"`
		BaseRefOid        string      `json:"baseRefOid"`
		Mergeable         string      `json:"mergeable"`
		StatusCheckRollup []prs.Check `json:"statusCheckRollup"`
		Author            struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Status{}, fmt.Errorf("read gh pr view: %w", err)
	}
	if !sha(v.HeadRefOid) {
		return Status{}, fmt.Errorf("gh pr view gave no head commit")
	}
	st := Status{State: v.State, IsDraft: v.IsDraft, Head: v.HeadRefOid, Base: v.BaseRefOid, Mergeable: v.Mergeable,
		CI: prs.RollupCI(v.StatusCheckRollup), Checks: prs.CountChecks(v.StatusCheckRollup).String(), ReplacedFailures: prs.ReplacedFailures(v.StatusCheckRollup),
		Author: v.Author.Login}
	// gh pr view has no author association, so the REST API supplies it, and
	// only when it matters: for a ready PR, to review or approve it.
	if st.Ready() {
		repo, number, err := ParseURL(url)
		if err != nil {
			return Status{}, err
		}
		out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, number), "--jq", ".author_association")
		if err != nil {
			return Status{}, err
		}
		st.Association = strings.TrimSpace(string(out))
	}
	return st, nil
}

// Detail is a PR's metadata for the app, read on demand.
type Detail struct {
	URL, Title, Body, Author  string
	State                     string
	IsDraft                   bool
	Base, HeadRef, Head       string
	Additions, Deletions      int
	ChangedFiles              int
	CreatedAt, UpdatedAt      string
	Mergeable, ReviewDecision string
	CI                        string
	Labels                    []string
	RequestedReviewers        []string
	Reviews                   []ReviewerState
	Checks                    []CheckState
	Files                     []FileChange
}

// ReviewerState is one reviewer's latest review.
type ReviewerState struct{ Login, State string }

// CheckState is one CI check and how it ended (or its status while it runs).
type CheckState struct{ Name, Result string }

// FileChange is one changed file.
type FileChange struct {
	Path                 string
	Additions, Deletions int
}

// maxBody and maxFiles keep a detail response small; the PR page has the rest.
const (
	maxBody  = 8000
	maxFiles = 100
	maxList  = 100
)

// FetchDetail reads a PR's metadata with one gh call. Text in it (title, body,
// labels, file paths) is other people's and is cleaned but not trusted.
func FetchDetail(ctx context.Context, gh github.Runner, url string) (Detail, error) {
	out, err := gh(ctx, "pr", "view", url, "--json",
		"title,body,author,state,isDraft,baseRefName,headRefName,headRefOid,additions,deletions,changedFiles,"+
			"createdAt,updatedAt,mergeable,reviewDecision,labels,reviewRequests,latestReviews,statusCheckRollup,files")
	if err != nil {
		return Detail{}, err
	}
	var v struct {
		Title       string                  `json:"title"`
		Body        string                  `json:"body"`
		Author      struct{ Login string }  `json:"author"`
		State       string                  `json:"state"`
		IsDraft     bool                    `json:"isDraft"`
		BaseRefName string                  `json:"baseRefName"`
		HeadRefName string                  `json:"headRefName"`
		HeadRefOid  string                  `json:"headRefOid"`
		Additions   int                     `json:"additions"`
		Deletions   int                     `json:"deletions"`
		Changed     int                     `json:"changedFiles"`
		CreatedAt   string                  `json:"createdAt"`
		UpdatedAt   string                  `json:"updatedAt"`
		Mergeable   string                  `json:"mergeable"`
		Decision    string                  `json:"reviewDecision"`
		Labels      []struct{ Name string } `json:"labels"`
		Requests    []struct {
			Login string `json:"login"`
			Name  string `json:"name"`
			Slug  string `json:"slug"`
		} `json:"reviewRequests"`
		Reviews []struct {
			Author struct{ Login string } `json:"author"`
			State  string                 `json:"state"`
		} `json:"latestReviews"`
		Checks []prs.Check `json:"statusCheckRollup"`
		Files  []struct {
			Path      string `json:"path"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Detail{}, fmt.Errorf("read gh pr view: %w", err)
	}
	d := Detail{
		URL: url, Title: clean(v.Title, 400, false), Body: clean(v.Body, maxBody, true),
		Author: clean(v.Author.Login, 60, false), State: v.State, IsDraft: v.IsDraft,
		Base: clean(v.BaseRefName, 255, false), HeadRef: clean(v.HeadRefName, 255, false), Head: v.HeadRefOid,
		Additions: v.Additions, Deletions: v.Deletions, ChangedFiles: v.Changed,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Mergeable: v.Mergeable, ReviewDecision: v.Decision,
	}
	latest := prs.LatestChecks(v.Checks)
	for i, c := range latest {
		if i == maxList {
			break
		}
		name := c.Name
		if name == "" {
			name = c.Context
		}
		result := c.Conclusion
		if result == "" {
			result = c.State
		}
		if result == "" {
			result = c.Status
		}
		d.Checks = append(d.Checks, CheckState{Name: clean(name, 200, false), Result: strings.ToLower(result)})
	}
	// CI sums up every check, not only the listed ones.
	d.CI = prs.RollupCI(latest)
	for _, l := range v.Labels[:min(len(v.Labels), maxList)] {
		d.Labels = append(d.Labels, clean(l.Name, 100, false))
	}
	for _, r := range v.Requests[:min(len(v.Requests), maxList)] {
		who := r.Login
		if who == "" {
			who = r.Slug
		}
		if who == "" {
			who = r.Name
		}
		d.RequestedReviewers = append(d.RequestedReviewers, clean(who, 100, false))
	}
	for _, r := range v.Reviews[:min(len(v.Reviews), maxList)] {
		d.Reviews = append(d.Reviews, ReviewerState{Login: clean(r.Author.Login, 60, false), State: r.State})
	}
	for i, f := range v.Files {
		if i == maxFiles {
			break
		}
		d.Files = append(d.Files, FileChange{Path: clean(f.Path, 500, false), Additions: f.Additions, Deletions: f.Deletions})
	}
	return d, nil
}

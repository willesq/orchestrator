// Package landing lets the user decide what happens to a commit an agent made
// for a PR without pushing it. The agent reports the commit on the last line of
// its result; orchd copies only that commit into a private bare repository,
// checks it against the PR's current head and shows it to the user. Nothing
// reaches GitHub until the user asks, and then only as a fast-forward of the
// PR's own branch. Undoing pushes a new commit and rewrites nothing.
package landing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/reviews"
)

// Where a task's proposal stands.
const (
	StateNone      = "none"      // the task proposed nothing
	StateProposed  = "proposed"  // waits on the user, and builds on the PR's head
	StateStale     = "stale"     // the PR moved on, so it would not fast-forward
	StateInvalid   = "invalid"   // the commit could not be read
	StateOnPR      = "on_pr"     // it already is the PR's head
	StateLanded    = "landed"    // the user pushed it
	StateUndone    = "undone"    // the user pushed it, then restored the earlier tree
	StateDiscarded = "discarded" // the user set it aside
)

// ProposalPrefix starts the line a task's result ends with to propose a commit.
const ProposalPrefix = "PROPOSED: "

var (
	// ErrNotFound means no task has that ID.
	ErrNotFound = errors.New("no such task")
	// ErrRefused means the action does not apply to the landing as it stands.
	ErrRefused = errors.New("refused")

	proposalLine = regexp.MustCompile(`^PROPOSED: (/.*) ([0-9a-f]{40})$`)
	prLine       = regexp.MustCompile(`(?m)^PR: (https://github\.com/\S+)$`)
	safeTaskID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
	// safeBranch is narrower than git allows; a branch outside it is refused
	// rather than passed to a refspec.
	safeBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	fullSHA    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Proposal is the commit a task reports and where it left it.
type Proposal struct{ Path, SHA string }

// ParseProposal reads a task's result. Only its last non-empty line counts, so
// a report that quotes the format elsewhere proposes nothing.
func ParseProposal(result string) (Proposal, bool) {
	lines := strings.Split(strings.TrimRight(result, " \t\r\n"), "\n")
	line := strings.Trim(strings.TrimSpace(lines[len(lines)-1]), "`")
	m := proposalLine.FindStringSubmatch(line)
	if m == nil {
		return Proposal{}, false
	}
	return Proposal{Path: m[1], SHA: m[2]}, true
}

// PRURL returns the PR a task's body names on the "PR: <url>" line a hand-off
// writes, or "".
func PRURL(body string) string {
	m := prLine.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	if _, _, err := reviews.ParseURL(m[1]); err != nil {
		return ""
	}
	return m[1]
}

// checkPath refuses a proposal path that is not plainly a directory under the
// user's home. It came from an agent, so it must not name a transport or a
// location outside the user's own files.
func checkPath(path, home string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\n\r") {
		return fmt.Errorf("%w: the proposed path is not a clean absolute path", ErrRefused)
	}
	if home == "" || !strings.HasPrefix(path, strings.TrimRight(home, "/")+"/") {
		return fmt.Errorf("%w: the proposed path is outside your home directory", ErrRefused)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: the proposed worktree no longer exists", ErrRefused)
	}
	// A symlink must not lead the fetch out of home after the check above.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%w: the proposed worktree no longer exists", ErrRefused)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil || !strings.HasPrefix(resolved, strings.TrimRight(realHome, "/")+"/") {
		return fmt.Errorf("%w: the proposed path is outside your home directory", ErrRefused)
	}
	return nil
}

func isSHA(s string) bool { return fullSHA.MatchString(s) }

// Landing is a task's proposal as the user sees it.
type Landing struct {
	TaskID, URL, State, Problem string
	Path, Branch, BaseSHA, SHA  string
	Commits                     []Commit
	TotalCommits                int
	Files                       []File
	TotalFiles                  int
	Additions, Deletions        int
	BinaryFiles                 int
	Diff                        string
	DiffTruncated               bool
	CanLand                     bool
	Blocked                     string
	CanUndo                     bool
	RevertSHA                   string
	DecidedAt                   int64
}

// Task is what landing needs of a task.
type Task struct{ Body, Result, Status string }

// Service decides what happens to proposals. One action runs at a time, so a
// double click cannot push twice.
type Service struct {
	Store *Store
	// Dir holds a bare repository per task.
	Dir string
	// Home bounds where a proposal may live.
	Home  string
	GH    github.Runner
	Git   GitRunner
	Login func(context.Context) (string, error)
	Task  func(context.Context, string) (Task, bool, error)
	// AccountFor, when set, picks the GitHub account for a PR's owner: a
	// space's gh profile (SP-063). The PR must be that account's, and pushes
	// run as it. An error refuses. nil uses Login and the default gh login.
	AccountFor func(ctx context.Context, owner string) (Account, error)
	// GitAs runs git with a profile's environment; nil is GitWithEnv.
	GitAs func(env []string) GitRunner

	mu sync.Mutex
}

// Account is the GitHub account a PR's pushes run as.
type Account struct {
	Login string
	// Env, when set, is the gh environment (github.ProfileEnv) pushes run
	// with, without the user's git configuration; nil is the default login.
	Env []string
}

// account returns the account for the PR at url.
func (s *Service) account(ctx context.Context, url string) (Account, error) {
	ownerRepo, _, err := reviews.ParseURL(url)
	if err != nil {
		return Account{}, err
	}
	return s.ownerAccount(ctx, ownerRepo)
}

// ownerAccount returns the account for the owner of ownerRepo.
func (s *Service) ownerAccount(ctx context.Context, ownerRepo string) (Account, error) {
	if s.AccountFor == nil {
		login, err := s.Login(ctx)
		return Account{Login: login}, err
	}
	owner, _, _ := strings.Cut(ownerRepo, "/")
	return s.AccountFor(ctx, owner)
}

// PRState is the part of a PR that decides whether a proposal may land.
type PRState struct {
	State, Head, Branch, Base, Author string
	CrossRepository                   bool
}

func (s *Service) readPR(ctx context.Context, url string) (PRState, error) {
	out, err := s.GH(ctx, "pr", "view", url, "--json", "state,headRefOid,headRefName,baseRefName,author,isCrossRepository")
	if err != nil {
		return PRState{}, err
	}
	var v struct {
		State             string `json:"state"`
		HeadRefOid        string `json:"headRefOid"`
		HeadRefName       string `json:"headRefName"`
		BaseRefName       string `json:"baseRefName"`
		IsCrossRepository bool   `json:"isCrossRepository"`
		Author            struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return PRState{}, fmt.Errorf("read gh pr view: %w", err)
	}
	if !isSHA(v.HeadRefOid) {
		return PRState{}, errors.New("gh pr view gave no head commit")
	}
	return PRState{State: v.State, Head: v.HeadRefOid, Branch: v.HeadRefName, Base: v.BaseRefName,
		Author: v.Author.Login, CrossRepository: v.IsCrossRepository}, nil
}

// blocked says why a commit may not be pushed to the PR, or "". The app has
// its own version of this for the form; this one is the check that counts.
func (s *Service) blocked(ctx context.Context, url string, pr PRState) (string, error) {
	switch {
	case pr.State != "OPEN":
		return "The PR is no longer open.", nil
	case pr.CrossRepository:
		return "The PR's branch is in a fork, so orch will not push to it.", nil
	case !safeBranch.MatchString(pr.Branch) || strings.Contains(pr.Branch, ".."):
		return "The PR's branch name is not one orch will push to.", nil
	}
	lowered := strings.ToLower(pr.Branch)
	if lowered == strings.ToLower(pr.Base) || lowered == "main" || lowered == "master" || lowered == "develop" || lowered == "trunk" {
		return "The PR's head is a protected branch, so orch will not push to it.", nil
	}
	acct, err := s.account(ctx, url)
	if err != nil {
		return "", err
	}
	if login := acct.Login; login == "" || !strings.EqualFold(login, pr.Author) {
		return "Someone else wrote this PR, so orch will not push to it.", nil
	}
	return "", nil
}

func (s *Service) repoFor(taskID string) (string, error) {
	if !safeTaskID.MatchString(taskID) {
		return "", fmt.Errorf("%w: bad task ID", ErrRefused)
	}
	return filepath.Join(s.Dir, taskID+".git"), nil
}

// Get returns the task's landing, reading the PR from GitHub when it has an
// undecided proposal.
func (s *Service) Get(ctx context.Context, taskID string) (Landing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(ctx, taskID)
}

func (s *Service) get(ctx context.Context, taskID string) (Landing, error) {
	repo, err := s.repoFor(taskID)
	if err != nil {
		return Landing{}, err
	}
	task, found, err := s.Task(ctx, taskID)
	if err != nil {
		return Landing{}, err
	}
	if !found {
		return Landing{}, ErrNotFound
	}
	l := Landing{TaskID: taskID, URL: PRURL(task.Body), State: StateNone}
	proposal, proposed := ParseProposal(task.Result)
	if l.URL == "" || !proposed || task.Status != "complete" {
		return l, nil
	}
	l.Path, l.SHA = proposal.Path, proposal.SHA

	if rec, decided, err := s.Store.Get(ctx, taskID); err != nil {
		return Landing{}, err
	} else if decided {
		return s.decided(ctx, repo, rec, l)
	}

	if err := s.ensureRepo(ctx, repo); err != nil {
		return Landing{}, err
	}
	current, err := s.fetchProposal(ctx, repo, proposal)
	if err != nil {
		l.State, l.Problem = StateInvalid, "Could not read the proposed commit: "+problemText(err)
		return l, nil
	}
	if !current {
		l.State = StateInvalid
		l.Problem = "The worktree has moved on from the commit the agent reported."
		return l, nil
	}
	pr, err := s.readPR(ctx, l.URL)
	if err != nil {
		return Landing{}, err
	}
	l.Branch, l.BaseSHA = pr.Branch, pr.Head
	if pr.Head == proposal.SHA {
		l.State = StateOnPR
		return l, nil
	}
	on, err := s.builtOn(ctx, repo, pr.Head, proposal.SHA)
	if err != nil {
		return Landing{}, err
	}
	if !on {
		l.State = StateStale
		l.Problem = fmt.Sprintf("The PR's head is now %s, which this commit does not build on, so it would not fast-forward. Send it back to rebuild on the new head.", short(pr.Head))
		return l, nil
	}
	if err := s.fillChange(ctx, repo, pr.Head, proposal.SHA, &l); err != nil {
		return Landing{}, err
	}
	l.State = StateProposed
	if l.Blocked, err = s.blocked(ctx, l.URL, pr); err != nil {
		return Landing{}, err
	}
	l.CanLand = l.Blocked == ""
	return l, nil
}

// decided fills a landing from what the user chose. The commit list is a
// nicety here: a pruned repository only loses it.
func (s *Service) decided(ctx context.Context, repo string, rec Record, l Landing) (Landing, error) {
	l.State, l.Branch, l.BaseSHA, l.SHA, l.RevertSHA = rec.State, rec.Branch, rec.PriorHead, rec.SHA, rec.RevertSHA
	l.DecidedAt = rec.UpdatedAt
	if rec.Path != "" {
		l.Path = rec.Path
	}
	if rec.PriorHead != "" && repoTrusted(repo) && s.hasCommit(ctx, repo, rec.PriorHead) && s.hasCommit(ctx, repo, rec.SHA) {
		if err := s.fillChange(ctx, repo, rec.PriorHead, rec.SHA, &l); err != nil {
			log.Printf("landing: read %s: %v", rec.TaskID, err)
		}
	}
	if rec.State == StateLanded {
		pr, err := s.readPR(ctx, rec.URL)
		if err != nil {
			l.Blocked = "Could not read the PR from GitHub to offer undo: " + problemText(err)
			return l, nil
		}
		l.Blocked = undoBlocked(pr, rec)
		l.CanUndo = l.Blocked == ""
	}
	return l, nil
}

func undoBlocked(pr PRState, rec Record) string {
	switch {
	case pr.State != "OPEN":
		return "The PR is no longer open, so undo is off."
	case pr.Head != rec.SHA:
		return fmt.Sprintf("The branch has moved to %s since this landed, so undo is off: it would drop those commits' changes.", short(pr.Head))
	}
	return ""
}

// Land pushes sha, the commit the user reviewed, to the PR's branch. Every
// check runs again here: the PR may have moved since the user looked.
func (s *Service) Land(ctx context.Context, taskID, sha string) (Landing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.get(ctx, taskID)
	if err != nil {
		return Landing{}, err
	}
	switch {
	case l.State != StateProposed:
		return l, fmt.Errorf("%w: this proposal is %s. %s", ErrRefused, l.State, l.Problem)
	case !l.CanLand:
		return l, fmt.Errorf("%w: %s", ErrRefused, l.Blocked)
	case !isSHA(sha) || sha != l.SHA:
		return l, fmt.Errorf("%w: that is not the commit this task proposed", ErrRefused)
	}
	repo, ownerRepo, err := s.pushTarget(taskID, l.URL)
	if err != nil {
		return l, err
	}
	if err := s.verifyForPush(ctx, repo); err != nil {
		return l, err
	}
	if err := s.push(ctx, repo, ownerRepo, sha, l.Branch); err != nil {
		return l, err
	}
	rec := Record{TaskID: taskID, URL: l.URL, Branch: l.Branch, Path: l.Path, SHA: sha, PriorHead: l.BaseSHA, State: StateLanded}
	if err := s.Store.Put(ctx, rec); err != nil {
		return l, fmt.Errorf("pushed %s, but could not record it: %w", short(sha), err)
	}
	return s.get(ctx, taskID)
}

// Undo pushes a commit restoring the tree the PR had before the landing, only
// while the landed commit is still the branch's head.
func (s *Service) Undo(ctx context.Context, taskID string) (Landing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, decided, err := s.Store.Get(ctx, taskID)
	if err != nil {
		return Landing{}, err
	}
	if !decided || rec.State != StateLanded {
		return Landing{}, fmt.Errorf("%w: this task has no landing to undo", ErrRefused)
	}
	pr, err := s.readPR(ctx, rec.URL)
	if err != nil {
		return Landing{}, err
	}
	if why := undoBlocked(pr, rec); why != "" {
		return Landing{}, fmt.Errorf("%w: %s", ErrRefused, why)
	}
	if why, err := s.blocked(ctx, rec.URL, pr); err != nil {
		return Landing{}, err
	} else if why != "" {
		return Landing{}, fmt.Errorf("%w: %s", ErrRefused, why)
	}
	repo, ownerRepo, err := s.pushTarget(taskID, rec.URL)
	if err != nil {
		return Landing{}, err
	}
	if err := s.verifyForPush(ctx, repo); err != nil {
		return Landing{}, err
	}
	revert, err := s.restoreCommit(ctx, repo, rec.PriorHead, rec.SHA)
	if err != nil {
		return Landing{}, err
	}
	if err := s.push(ctx, repo, ownerRepo, revert, pr.Branch); err != nil {
		return Landing{}, err
	}
	rec.RevertSHA, rec.State = revert, StateUndone
	if err := s.Store.Put(ctx, rec); err != nil {
		return Landing{}, fmt.Errorf("pushed %s, but could not record it: %w", short(revert), err)
	}
	return s.get(ctx, taskID)
}

func (s *Service) pushTarget(taskID, url string) (repo, ownerRepo string, err error) {
	if repo, err = s.repoFor(taskID); err != nil {
		return "", "", err
	}
	ownerRepo, _, err = reviews.ParseURL(url)
	return repo, ownerRepo, err
}

// Discard sets an undecided proposal aside; restore brings a discarded one
// back. Neither touches GitHub or the agent's worktree.
func (s *Service) Discard(ctx context.Context, taskID string, restore bool) (Landing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if restore {
		rec, decided, err := s.Store.Get(ctx, taskID)
		if err != nil {
			return Landing{}, err
		}
		if !decided || rec.State != StateDiscarded {
			return Landing{}, fmt.Errorf("%w: this proposal was not discarded", ErrRefused)
		}
		if err := s.Store.Delete(ctx, taskID); err != nil {
			return Landing{}, err
		}
		return s.get(ctx, taskID)
	}
	l, err := s.get(ctx, taskID)
	if err != nil {
		return Landing{}, err
	}
	switch l.State {
	case StateProposed, StateStale, StateInvalid, StateOnPR:
	default:
		return l, fmt.Errorf("%w: this proposal is %s", ErrRefused, l.State)
	}
	rec := Record{TaskID: taskID, URL: l.URL, Branch: l.Branch, Path: l.Path, SHA: l.SHA, PriorHead: l.BaseSHA, State: StateDiscarded}
	if err := s.Store.Put(ctx, rec); err != nil {
		return l, err
	}
	return s.get(ctx, taskID)
}

// Prune removes bare repositories untouched for maxAge. Decisions stay; they
// only lose their commit list.
func (s *Service) Prune(maxAge time.Duration, now time.Time) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || !strings.HasSuffix(e.Name(), ".git") || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, e.Name())); err != nil {
			log.Printf("landing: prune %s: %v", e.Name(), err)
		}
	}
}

// problemText drops the sentinel's wording from an error shown to the user.
func problemText(err error) string {
	return strings.TrimPrefix(err.Error(), ErrRefused.Error()+": ")
}

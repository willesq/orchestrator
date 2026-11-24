package reviews

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/github"
)

// Task statuses the watcher reads from the dispatch ledger.
const (
	taskComplete = "complete"
	taskPending  = "pending"
	taskRunning  = "in_progress"
)

// DefaultPoll is how often each watched PR's readiness is checked.
const DefaultPoll = 2 * time.Minute

const maxWatchDays = 14

// MaxWatch is how long one watch runs before it stops and asks the user to
// watch the PR again, so a forgotten PR does not keep polling and reviewing.
const MaxWatch = maxWatchDays * 24 * time.Hour

// Dispatcher queues a review task of repo's PR ("owner/name") with body, to
// run in co, and returns its ID. The repo decides the space the task reads
// (SP-026).
type Dispatcher func(ctx context.Context, repo, body string, co Checkout) (string, error)

// Workspaces prepares the checkouts a review runs in and removes those no
// review needs any more.
type Workspaces interface {
	Prepare(ctx context.Context, spec CheckoutSpec) (Checkout, error)
	Sweep(ctx context.Context, keep map[string]bool)
}

// TaskLookup reads a task's status and result from the dispatch ledger.
type TaskLookup func(ctx context.Context, id string) (status, result string, found bool, err error)

// TaskCanceller cancels a queued or running task, recording reason.
type TaskCanceller func(ctx context.Context, id, reason string) error

// ErrNotFound is returned for a PR that is not watched.
var ErrNotFound = errors.New("that PR is not watched")

// ErrPosting is returned for a change to a PR that orch is posting to
// GitHub for right now: the post is already on its way and cannot be undone.
var ErrPosting = errors.New("orch is posting to GitHub for this PR; try again in a moment")

// Watcher reviews watched PRs once they are ready. Its RPC-facing methods and
// Tick share one lock, held around reading and writing rows and queueing a
// review (a local write), never across a gh call.
type Watcher struct {
	Store    *Store
	GH       github.Runner
	Post     Poster
	Dispatch Dispatcher
	Task     TaskLookup
	// Workspaces prepares each review's checkouts; nil runs reviews in a
	// scratch directory with no checkout.
	Workspaces Workspaces
	// HooksDefault reports whether a review of owner's repo runs the repo's
	// hooks and MCP servers when the PR leaves that automatic; nil means never.
	HooksDefault func(ctx context.Context, owner string) bool
	// Cancel stops a review task with reason as its result; nil leaves a
	// review whose PR moved on to finish, unposted.
	Cancel TaskCanceller
	// Login is the user's GitHub login; GitHub refuses to let them approve
	// their own PRs.
	Login func(ctx context.Context) (string, error)
	// Poll is how often each PR is checked; zero uses DefaultPoll.
	Poll time.Duration
	// CheckOwner, when set, re-checks the PR owner's GitHub org binding
	// before anything from the PR is acted on or recorded (D-52); a refused
	// owner's PR is left as it was, with a note.
	CheckOwner func(ctx context.Context, owner string) error

	mu      sync.Mutex
	checked map[string]time.Time
	due     map[string]bool
	// posting holds PRs with a review on its way to GitHub; changes to them
	// are refused until it lands, so what the user sees is what was posted.
	posting map[string]bool
	// unrecorded holds, per PR, a commit posted to whose row could not say so.
	unrecorded map[string]string
	now        func() time.Time
}

func (w *Watcher) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *Watcher) poll() time.Duration {
	if w.Poll > 0 {
		return w.Poll
	}
	return DefaultPoll
}

// List returns every watched PR.
func (w *Watcher) List(ctx context.Context) ([]Review, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Store.List(ctx)
}

// Watch starts watching url, or changes what orch may post for it. A nil
// comment keeps the drafted one; a stopped, closed or failed review starts
// over. The PR is checked on the next tick.
func (w *Watcher) Watch(ctx context.Context, url, mode string, comment *string) (Review, error) {
	return w.WatchWith(ctx, url, mode, comment, nil)
}

// WatchWith is Watch that can also set the user's notes for the PR's
// reviews; nil keeps them. New notes on a PR that has a verdict or a failed
// review review its current commit again with them.
func (w *Watcher) WatchWith(ctx context.Context, url, mode string, comment, instructions *string) (Review, error) {
	return w.WatchOpts(ctx, url, mode, comment, instructions, false, nil, nil)
}

// WatchOpts is WatchWith that can also set whether later comments edit orch's
// earlier one, give the go-ahead to review the next ready commit although its
// author is not trusted, and set how its reviews are set up (nil keeps it). A
// new setup on a PR that has a verdict or a failed review reviews its current
// commit again with it.
func (w *Watcher) WatchOpts(ctx context.Context, url, mode string, comment, instructions *string, goAhead bool, updateInPlace *bool, setup *Setup) (Review, error) {
	if !ValidMode(mode) {
		return Review{}, fmt.Errorf("mode must be %s, %s, %s, %s or %s", ModeVerdict, ModeApprove, ModeComment, ModeFeedback, ModeAuto)
	}
	if setup != nil {
		if err := setup.Validate(); err != nil {
			return Review{}, err
		}
	}
	repo, number, err := ParseURL(url)
	if err != nil {
		return Review{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.posting[url] {
		return Review{}, ErrPosting
	}
	r, found, err := w.Store.Get(ctx, url)
	if err != nil {
		return Review{}, err
	}
	if !found {
		r = Review{URL: url, Repo: repo, Number: number, State: StateWaiting}
	}
	if goAhead {
		r.GoAhead = true
	}
	if updateInPlace != nil {
		r.UpdateInPlace = *updateInPlace
	}
	if r.Mode != mode {
		// Who changed what orch may post, and when, is worth keeping.
		log.Printf("pr-reviews: %s may now post as %q (was %q)", url, mode, r.Mode)
	}
	if !r.Active() || w.pastLimit(r) {
		// A new watch, with a full MaxWatch ahead of it. A go-ahead covers one
		// review on the watch it was given for, unless this call gives it again.
		r.WatchedAt, r.GoAhead = time.Time{}, goAhead
	}
	switch r.State {
	case StateStopped, StateClosed, StateHeld:
		r.State = StateWaiting
	case StateFailed:
		// A failed head is not retried on its own; asking again retries it.
		r.State, r.HeadSHA = StateWaiting, ""
	}
	if r.Posted == PostedFailed || (mode == ModeComment && (r.Posted == PostedCommented || r.Posted == PostedNone)) {
		// Asking again is another attempt; a comment is posted once per ask.
		r.Posted = ""
	}
	r.Mode = mode
	if comment != nil {
		r.Comment = clean(*comment, maxComment, true)
	}
	if instructions != nil {
		next := clean(*instructions, MaxInstructions, true)
		if next != r.Instructions && (r.State == StateVerdict || r.State == StateFailed || r.Verdict != "") &&
			r.State != StateReviewing {
			r.State, r.HeadSHA = StateWaiting, ""
		}
		r.Instructions = next
	}
	if setup != nil {
		next := setup.Normalized()
		if next != r.Setup.Normalized() {
			log.Printf("pr-reviews: %s reviews now take instructions from %s, compare against %s, project hooks %q",
				url, next.InstructionsFrom, next.CompareAgainst, next.ProjectHooks)
			if (r.State == StateVerdict || r.State == StateFailed || r.Verdict != "") && r.State != StateReviewing {
				r.State, r.HeadSHA = StateWaiting, ""
			}
		}
		r.Setup = next
	}
	r.Note = ""
	if err := w.Store.Put(ctx, &r); err != nil {
		return Review{}, err
	}
	w.markDue(url)
	return r, nil
}

// SetUpdateInPlace changes whether later comments edit orch's earlier one on
// the PR. It is a preference, not an ask: it never posts, retries or reviews.
func (w *Watcher) SetUpdateInPlace(ctx context.Context, url string, on bool) (Review, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.posting[url] {
		return Review{}, ErrPosting
	}
	r, found, err := w.Store.Get(ctx, url)
	if err != nil {
		return Review{}, err
	}
	if !found {
		return Review{}, ErrNotFound
	}
	if r.UpdateInPlace != on {
		log.Printf("pr-reviews: %s update in place is now %v", url, on)
	}
	r.UpdateInPlace = on
	if err := w.Store.Put(ctx, &r); err != nil {
		return Review{}, err
	}
	return r, nil
}

// Stop stops watching url; forget also deletes its verdict.
func (w *Watcher) Stop(ctx context.Context, url string, forget bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.posting[url] {
		return ErrPosting
	}
	r, found, err := w.Store.Get(ctx, url)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if forget {
		return w.Store.Delete(ctx, url)
	}
	r.State = StateStopped
	return w.Store.Put(ctx, &r)
}

func (w *Watcher) markDue(url string) {
	if w.due == nil {
		w.due = map[string]bool{}
	}
	w.due[url] = true
}

// Tick moves every active review along: it collects finished review tasks,
// checks PRs that are due, starts reviews of ready PRs, and posts what the
// user opted in to.
func (w *Watcher) Tick(ctx context.Context) {
	list, err := w.List(ctx)
	if err != nil {
		log.Printf("pr-reviews: list: %v", err)
		return
	}
	if w.Workspaces != nil {
		w.Workspaces.Sweep(ctx, w.liveWorkspaces(ctx, list))
	}
	for _, r := range list {
		if ctx.Err() != nil {
			return
		}
		if !r.Active() {
			continue
		}
		if r.State == StateReviewing {
			if w.Cancel != nil && w.isDue(r.URL) && w.supersede(ctx, r.URL) {
				continue
			}
			w.collect(ctx, r.URL)
			continue
		}
		if w.isDue(r.URL) {
			w.check(ctx, r.URL)
		}
	}
}

func (w *Watcher) isDue(url string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.due[url] {
		return true
	}
	last, ok := w.checked[url]
	return !ok || w.clock().Sub(last) >= w.poll()
}

// Schedule reports when orchd last read url's PR from GitHub and when it reads
// it next; both are zero for a review it no longer checks, and checked is zero
// before the first read since orchd started. A review under way is read only
// to see whether new commits replaced it, which needs Cancel.
func (w *Watcher) Schedule(r Review) (checked, next time.Time) {
	if !r.Active() {
		return time.Time{}, time.Time{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	checked = w.checked[r.URL]
	if r.State == StateReviewing && w.Cancel == nil {
		return checked, time.Time{}
	}
	if w.due[r.URL] || checked.IsZero() {
		return checked, w.clock()
	}
	return checked, checked.Add(w.poll())
}

func (w *Watcher) checkedNow(url string) {
	if w.checked == nil {
		w.checked = map[string]time.Time{}
	}
	w.checked[url] = w.clock()
	delete(w.due, url)
}

// update applies change to the stored review of url under the lock, and
// writes it when change reports a change. A review that went away or stopped
// in the meantime is left alone.
func (w *Watcher) update(ctx context.Context, url string, change func(r *Review) bool) (Review, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, found, err := w.Store.Get(ctx, url)
	if err != nil || !found || !r.Active() {
		return r, false
	}
	if !change(&r) {
		return r, true
	}
	if err := w.Store.Put(ctx, &r); err != nil {
		log.Printf("pr-reviews: save %s: %v", url, err)
		return r, false
	}
	return r, true
}

// collect records a finished review task's verdict.
func (w *Watcher) collect(ctx context.Context, url string) {
	r, _ := w.update(ctx, url, func(*Review) bool { return false })
	status, result, found, err := w.Task(ctx, r.TaskID)
	if err != nil {
		log.Printf("pr-reviews: read task %s: %v", r.TaskID, err)
		return
	}
	if found && (status == taskPending || status == taskRunning) {
		return
	}
	var v Verdict
	failure := w.ownerRefused(ctx, url)
	switch {
	case failure != "":
	case !found:
		failure = "the review task is gone from the queue"
	case status != taskComplete:
		failure = "the review task ended " + status
	default:
		if v, err = ParseVerdict(result, r.Nonce); err != nil {
			failure = github.Printable(err.Error(), 300)
		}
	}
	w.update(ctx, url, func(r *Review) bool {
		if r.State != StateReviewing {
			return false
		}
		// Settled for this head either way, so a broken review is not rerun
		// on its own; a new commit or the user asking again retries.
		r.HeadSHA, r.PendingSHA = r.PendingSHA, ""
		// The task is done with its checkout; the next sweep removes it.
		r.Workspace = ""
		if failure != "" {
			r.State, r.Note = StateFailed, failure
			r.Verdict, r.Reason, r.Comment, r.ForUser, r.Report, r.Posted = "", "", "", "", "", ""
			return true
		}
		r.State, r.Note, r.Posted = StateVerdict, "", ""
		r.Verdict, r.Reason, r.Comment, r.ForUser, r.Report = v.Verdict, v.Reason, v.Comment, v.ForUser, v.Report
		if r.Mode == ModeComment {
			// "Post comment" was for the verdict the user read; a new one
			// waits for the user's own call.
			r.Mode = ModeVerdict
		}
		r.VerdictAt = w.clock().UTC().Truncate(time.Second)
		return true
	})
	w.mu.Lock()
	w.markDue(url)
	w.mu.Unlock()
}

// liveWorkspaces are the checkouts of review tasks that may still run or
// resume: a review under way, or a stopped watch whose task is still going.
func (w *Watcher) liveWorkspaces(ctx context.Context, list []Review) map[string]bool {
	keep := map[string]bool{}
	for _, r := range list {
		if r.Workspace == "" {
			continue
		}
		if r.State == StateReviewing {
			keep[r.Workspace] = true
			continue
		}
		status, _, found, err := w.Task(ctx, r.TaskID)
		// An unreadable ledger keeps it: a later sweep can still remove it.
		if err != nil || (found && (status == taskPending || status == taskRunning)) {
			keep[r.Workspace] = true
		}
	}
	return keep
}

// prepare makes the checkout for reviewing st.Head when check is about to
// start that review, and returns the setup it was made for. Both are empty
// when no review starts or Workspaces is nil; note says why the checkout
// could not be made.
func (w *Watcher) prepare(ctx context.Context, url string, st Status) (co *Checkout, setup Setup, note string) {
	if w.Workspaces == nil || !st.Ready() {
		return nil, Setup{}, ""
	}
	w.mu.Lock()
	r, found, err := w.Store.Get(ctx, url)
	w.mu.Unlock()
	if err != nil || !found || !r.Active() || r.State == StateReviewing || w.pastLimit(r) ||
		st.Head == r.HeadSHA || (!st.Trusted() && !r.GoAhead) {
		return nil, Setup{}, ""
	}
	setup = r.Setup.Normalized()
	spec := CheckoutSpec{Repo: r.Repo, Head: st.Head, Base: st.Base, From: setup.InstructionsFrom, Compare: setup.CompareAgainst}
	headNote := ""
	if spec.From == FromHead && !st.Trusted() {
		// A go-ahead lets the PR's code be read and run, not its author
		// decide what the reviewer is told or runs.
		spec.From = FromDefault
		headNote = "the user chose the PR head's project instructions, but its author is not a member or collaborator, so the default branch's were used"
	}
	switch setup.ProjectHooks {
	case HooksOn:
		spec.Hooks = true
	case HooksAuto:
		// Automatic trusts what the repo merged, never a branch nobody has
		// reviewed yet: settings from the PR's base or head need an explicit on.
		owner, _, _ := strings.Cut(r.Repo, "/")
		spec.Hooks = spec.From == FromDefault && w.HooksDefault != nil && w.HooksDefault(ctx, owner)
	}
	c, err := w.Workspaces.Prepare(ctx, spec)
	if err != nil {
		return nil, setup, "could not prepare the review's checkout: " + github.Printable(err.Error(), 300)
	}
	c.Note = headNote
	return &c, setup, ""
}

// ownerRefused re-checks the org binding of url's owner and says why the
// PR's content may not be recorded, if it may not.
func (w *Watcher) ownerRefused(ctx context.Context, url string) string {
	if w.CheckOwner == nil {
		return ""
	}
	owner, _, ok := strings.Cut(strings.TrimPrefix(url, "https://github.com/"), "/")
	if !ok || owner == "" {
		return "not reviewed: the PR's owner cannot be read from its URL"
	}
	if err := w.CheckOwner(ctx, owner); err != nil {
		return "not reviewed: " + github.Printable(err.Error(), 300)
	}
	return ""
}

// check reads the PR's status and acts on it.
func (w *Watcher) check(ctx context.Context, url string) {
	st, err := FetchStatus(ctx, w.GH, url)
	w.mu.Lock()
	w.checkedNow(url)
	w.mu.Unlock()
	if err != nil {
		w.update(ctx, url, func(r *Review) bool {
			// A PR that can no longer be read still reaches the limit.
			if !w.expire(r) {
				r.Note = "could not read the PR: " + github.Printable(err.Error(), 300)
			}
			return true
		})
		return
	}
	if note := w.ownerRefused(ctx, url); note != "" {
		w.update(ctx, url, func(r *Review) bool {
			if w.expire(r) {
				return true
			}
			if r.Note == note {
				return false
			}
			r.Note = note
			return true
		})
		return
	}
	// Read before taking the lock: it may call gh.
	me := ""
	if w.Login != nil {
		me, _ = w.Login(ctx)
	}
	// Made before taking the lock: it fetches from GitHub.
	co, coSetup, coNote := w.prepare(ctx, url, st)
	var post *posting
	w.update(ctx, url, func(r *Review) bool {
		if st.State != "OPEN" {
			r.State, r.Note = StateClosed, "the PR is "+strings.ToLower(st.State)
			return true
		}
		if w.expire(r) {
			return true
		}
		if st.Head != r.HeadSHA {
			superseded := ""
			if wouldPost(*r) {
				// The verdict covers a commit the PR moved past: posting it
				// would raise findings the new commits may already fix.
				r.Posted = PostedSuperseded
				superseded = supersededNote(r.HeadSHA, st.Head) + "; "
			}
			if !st.Ready() {
				r.State, r.Note = StateWaiting, superseded+st.Waiting()
				if superseded == "" && r.HeadSHA != "" && r.Verdict != "" {
					r.Note = "new commits since the verdict; " + r.Note
				}
				return true
			}
			if !st.Trusted() && !r.GoAhead {
				// A review runs the PR's code with the user's credentials, so
				// an untrusted author's waits for the user to look first.
				r.State = StateHeld
				r.Note = "held: the author is not a member or collaborator, so orch does not run their code until you choose Review anyway"
				return true
			}
			if w.Workspaces != nil && coNote != "" {
				r.State, r.Note = StateWaiting, coNote
				return true
			}
			if w.Workspaces != nil && (co == nil || coSetup != r.Setup.Normalized()) {
				// The review changed since its workspace was made; the next
				// check makes one for it as it is now.
				w.markDue(url)
				return false
			}
			var run Checkout
			if co != nil {
				run = *co
			}
			nonce := NewNonce()
			id, err := w.Dispatch(ctx, r.Repo, Prompt(r.URL, r.Repo, r.Number, st.Head, nonce, st.promptChecks(), r.Instructions, co), run)
			if err != nil {
				r.State, r.Note = StateWaiting, "could not start the review: "+github.Printable(err.Error(), 300)
				return true
			}
			r.State, r.TaskID, r.PendingSHA, r.Nonce, r.Note, r.GoAhead = StateReviewing, id, st.Head, nonce, "", false
			r.Workspace = run.Dir
			return true
		}
		changed := false
		if r.State == StateWaiting && r.Verdict != "" {
			// Watching again after a stop: the verdict still covers this head.
			r.State, r.Note, changed = StateVerdict, "", true
		}
		if r.State == StateVerdict {
			post = plan(*r, st, me)
			if post != nil && post.note != "" {
				changed = changed || r.Note != post.note
				r.Note = post.note
				post = nil
			}
		}
		return changed
	})
	if post != nil {
		w.send(ctx, url, *post)
	}
}

func (w *Watcher) pastLimit(r Review) bool { return w.clock().Sub(r.WatchedAt) > MaxWatch }

// expire stops a watch that has run past MaxWatch and reports whether it did.
// Only check calls it, so a review under way finishes and keeps its verdict.
func (w *Watcher) expire(r *Review) bool {
	if !w.pastLimit(*r) {
		return false
	}
	log.Printf("pr-reviews: %s reached the %d-day watch limit", r.URL, maxWatchDays)
	r.State = StateStopped
	r.Note = fmt.Sprintf("reached the %d-day watch limit; watch the PR again to keep going", maxWatchDays)
	return true
}

// posting is a review orch is about to submit, or why it will not.
type posting struct {
	event, commit, posted string
	note                  string
}

// plan decides what to post for r's verdict now that the PR's head is still
// the one reviewed; me is the user's login, if known. nil posts nothing.
func plan(r Review, st Status, me string) *posting {
	switch {
	case r.Posted == PostedFailed:
		return nil
	case approves(r.Mode) && r.Verdict == VerdictApprove && r.Posted != PostedApproved:
		if !st.Ready() {
			return &posting{note: "not approving yet: " + st.Waiting()}
		}
		if !st.ApproveReady() {
			if st.ReplacedFailures > 0 {
				return &posting{note: "not approving: an earlier run of a check failed, and the author is not a member or collaborator; the verdict is yours to act on"}
			}
			return &posting{note: "not approving: no CI has run, and the author is not a member or collaborator"}
		}
		if me != "" && strings.EqualFold(me, st.Author) {
			return &posting{note: "GitHub does not let you approve your own PR; the verdict is yours to act on"}
		}
		return &posting{event: "APPROVE", commit: r.HeadSHA, posted: PostedApproved}
	}
	return planComment(r)
}

// planComment decides what r's permissions post as a comment, on any commit.
func planComment(r Review) *posting {
	if r.Posted != "" {
		return nil
	}
	switch r.Mode {
	case ModeComment:
		if r.Comment != "" {
			return &posting{event: commentEvent(r), commit: r.HeadSHA, posted: PostedCommented}
		}
	case ModeFeedback, ModeAuto:
		if r.Verdict == VerdictApprove {
			if !r.UpdateInPlace {
				return nil
			}
			// Nothing new to raise, but an earlier comment must not keep
			// asking for changes.
			return &posting{event: EventRefresh, commit: r.HeadSHA, posted: PostedCommented}
		}
		if r.Comment != "" {
			// Findings for the author to fix; the next push is reviewed again.
			return &posting{event: commentEvent(r), commit: r.HeadSHA, posted: PostedCommented}
		}
	}
	return nil
}

func commentEvent(r Review) string {
	if r.UpdateInPlace {
		return EventUpdate
	}
	return "COMMENT"
}

// wouldPost reports whether r's verdict still has something to post for its
// commit under r's mode.
func wouldPost(r Review) bool {
	if r.HeadSHA == "" || r.Verdict == "" || r.Posted != "" {
		return false
	}
	return (approves(r.Mode) && r.Verdict == VerdictApprove) || planComment(r) != nil
}

func supersededNote(reviewed, head string) string {
	return fmt.Sprintf("superseded by %s: the verdict for %s was not posted", shortSHA(head), shortSHA(reviewed))
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// supersede cancels url's review task once it no longer matters, so no tokens
// go on it, and reports whether it did: a PR merged or closed ends the watch,
// and new commits past the one under review are reviewed next, once ready.
func (w *Watcher) supersede(ctx context.Context, url string) bool {
	r, ok := w.update(ctx, url, func(*Review) bool { return false })
	if !ok || r.State != StateReviewing {
		return false
	}
	st, err := FetchStatus(ctx, w.GH, url)
	w.mu.Lock()
	w.checkedNow(url)
	w.mu.Unlock()
	if err != nil {
		return false
	}
	closed := st.State != "OPEN"
	if !closed && st.Head == r.PendingSHA {
		return false
	}
	var reason, note string
	if closed {
		how := strings.ToLower(st.State)
		reason = "cancelled: the PR was " + how + " before the review finished"
		note = fmt.Sprintf("the PR is %s; cancelled the review of %s", how, shortSHA(r.PendingSHA))
	} else {
		reason = fmt.Sprintf("cancelled: the PR head moved from %s to %s before the review finished",
			shortSHA(r.PendingSHA), shortSHA(st.Head))
		note = fmt.Sprintf("cancelled the review of %s: new commits moved the head to %s",
			shortSHA(r.PendingSHA), shortSHA(st.Head))
	}
	if err := w.Cancel(ctx, r.TaskID, reason); err != nil {
		// Most likely it just ended; collect records the verdict, and the
		// check after it closes the watch or holds the post back.
		log.Printf("pr-reviews: cancel the review of %s at %s: %v", url, shortSHA(r.PendingSHA), err)
		return false
	}
	log.Printf("pr-reviews: %s; cancelled review task %s of %s", reason, r.TaskID, url)
	w.update(ctx, url, func(cur *Review) bool {
		if cur.State != StateReviewing || cur.TaskID != r.TaskID {
			return false
		}
		cur.State, cur.PendingSHA, cur.Nonce, cur.Note = StateWaiting, "", "", note
		if closed {
			// Closed rows are no longer read: the watch ends here.
			cur.State = StateClosed
		}
		return true
	})
	if !closed {
		w.mu.Lock()
		w.markDue(url)
		w.mu.Unlock()
	}
	return true
}

// send submits p with the comment as stored now, unless the user changed
// their choice or new commits arrived meanwhile: the PR's head is read again
// right before posting, and a verdict for any other commit is kept, marked
// superseded, and never posted. While it posts, the user's changes to the PR
// are refused (ErrPosting), and the outcome is recorded whatever state the
// row is in, so the app never shows less than was posted.
func (w *Watcher) send(ctx context.Context, url string, p posting) {
	w.mu.Lock()
	r, found, err := w.Store.Get(ctx, url)
	if err != nil || !found || r.HeadSHA != p.commit || r.State != StateVerdict ||
		(p.event == "APPROVE" && !approves(r.Mode)) || (p.event != "APPROVE" && !comments(r.Mode)) ||
		(p.event == EventRefresh && !r.UpdateInPlace) || w.unrecorded[url] == p.commit {
		w.mu.Unlock()
		return
	}
	if p.event == "COMMENT" || p.event == EventUpdate {
		// The plan may predate a change of the setting.
		p.event = commentEvent(r)
	}
	if w.posting == nil {
		w.posting = map[string]bool{}
	}
	w.posting[url] = true
	w.mu.Unlock()

	// Accepted risk: GitHub has no compare-and-set for reviews, so a push
	// between this read and the post can still land after it. The review stays
	// pinned to p.commit, and branch protection's "dismiss stale approvals"
	// drops an approval the push made stale.
	if head, herr := w.liveHead(ctx, url); herr != nil || head != p.commit {
		w.holdBack(ctx, url, p.commit, head, herr)
		return
	}
	err = w.Post(ctx, r.Repo, r.Number, p.event, p.commit, r.Comment)
	if err == nil && p.event == "APPROVE" && comments(r.Mode) && r.UpdateInPlace {
		// The approval stands either way, so a stale comment is not its failure.
		if rerr := w.Post(ctx, r.Repo, r.Number, EventRefresh, p.commit, r.Comment); rerr != nil && !errors.Is(rerr, ErrNoComment) {
			log.Printf("pr-reviews: update the comment on %s: %v", url, rerr)
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.posting, url)
	// What reached GitHub is recorded even during shutdown: a comment posted
	// again would be a second comment.
	ctx = context.WithoutCancel(ctx)
	r, found, gerr := w.Store.Get(ctx, url)
	if gerr != nil || !found {
		log.Printf("pr-reviews: %s posted to %s but the row is gone: %v", p.event, url, gerr)
		w.markUnrecorded(url, p.commit, err)
		return
	}
	if errors.Is(err, ErrNoComment) {
		r.Posted = PostedNone
	} else if err != nil {
		r.Posted, r.Note = PostedFailed, "could not post to GitHub: "+github.Printable(err.Error(), 300)
	} else {
		r.Posted, r.Note = p.posted, ""
		if r.Mode == ModeComment {
			// An asked-for comment is one-shot: later verdicts post nothing
			// until asked again.
			r.Mode = ModeVerdict
			r.Note = "comment posted; nothing more is posted unless you ask"
		}
	}
	if perr := w.Store.Put(ctx, &r); perr != nil {
		log.Printf("pr-reviews: record %s on %s: %v", p.event, url, perr)
		w.markUnrecorded(url, p.commit, err)
	}
}

// liveHead reads the PR's head commit from GitHub.
func (w *Watcher) liveHead(ctx context.Context, url string) (string, error) {
	st, err := FetchStatus(ctx, w.GH, url)
	if err != nil {
		return "", err
	}
	if st.Head == "" {
		return "", errors.New("GitHub returned no head commit")
	}
	return st.Head, nil
}

// holdBack records a post send did not make: the head moved to head, or it
// could not be read (err), in which case the next check tries again.
func (w *Watcher) holdBack(ctx context.Context, url, commit, head string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.posting, url)
	ctx = context.WithoutCancel(ctx)
	r, found, gerr := w.Store.Get(ctx, url)
	if gerr != nil || !found || r.HeadSHA != commit {
		return
	}
	if err != nil {
		r.Note = "not posted: could not confirm the PR's head: " + github.Printable(err.Error(), 300)
	} else {
		log.Printf("pr-reviews: %s moved to %s before posting the verdict for %s", url, shortSHA(head), shortSHA(commit))
		r.Posted, r.Note = PostedSuperseded, supersededNote(commit, head)
		w.markDue(url)
	}
	if perr := w.Store.Put(ctx, &r); perr != nil {
		log.Printf("pr-reviews: record the held-back post on %s: %v", url, perr)
	}
}

// markUnrecorded stops commit being posted to url again this run, when the post
// went out but the row does not say so. Call with w.mu held.
func (w *Watcher) markUnrecorded(url, commit string, postErr error) {
	if postErr != nil {
		return
	}
	if w.unrecorded == nil {
		w.unrecorded = map[string]string{}
	}
	w.unrecorded[url] = commit
}

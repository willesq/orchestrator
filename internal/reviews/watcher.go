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

// Dispatcher queues a review task with body and returns its ID.
type Dispatcher func(ctx context.Context, body string) (string, error)

// TaskLookup reads a task's status and result from the dispatch ledger.
type TaskLookup func(ctx context.Context, id string) (status, result string, found bool, err error)

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
	// Login is the user's GitHub login; GitHub refuses to let them approve
	// their own PRs.
	Login func(ctx context.Context) (string, error)
	// Poll is how often each PR is checked; zero uses DefaultPoll.
	Poll time.Duration

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
	return w.WatchOpts(ctx, url, mode, comment, instructions, false, nil)
}

// WatchOpts is WatchWith that can also set whether later comments edit orch's
// earlier one, and give the go-ahead to review the next
// ready commit although its author is not trusted.
func (w *Watcher) WatchOpts(ctx context.Context, url, mode string, comment, instructions *string, goAhead bool, updateInPlace *bool) (Review, error) {
	if !ValidMode(mode) {
		return Review{}, fmt.Errorf("mode must be %s, %s, %s, %s or %s", ModeVerdict, ModeApprove, ModeComment, ModeFeedback, ModeAuto)
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
	switch r.State {
	case StateStopped, StateClosed:
		// A new watch, with a full MaxWatch ahead of it. A go-ahead covers one
		// review on the watch it was given for, unless this call gives it again.
		r.State, r.WatchedAt, r.GoAhead = StateWaiting, time.Time{}, goAhead
	case StateHeld:
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
	for _, r := range list {
		if ctx.Err() != nil {
			return
		}
		if !r.Active() {
			continue
		}
		if r.State == StateReviewing {
			// Before collecting: a new verdict replaces the comment asked for.
			if post := planStale(r); post != nil {
				w.send(ctx, r.URL, *post)
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
// before the first read since orchd started. A review under way is not read
// from GitHub, so next is zero for it too.
func (w *Watcher) Schedule(r Review) (checked, next time.Time) {
	if !r.Active() {
		return time.Time{}, time.Time{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	checked = w.checked[r.URL]
	if r.State == StateReviewing {
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
	var failure string
	switch {
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
	// Read before taking the lock: it may call gh.
	me := ""
	if w.Login != nil {
		me, _ = w.Login(ctx)
	}
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
			post = planStale(*r)
			if !st.Ready() {
				r.State, r.Note = StateWaiting, st.Waiting()
				if r.HeadSHA != "" && r.Verdict != "" {
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
			nonce := NewNonce()
			id, err := w.Dispatch(ctx, Prompt(r.URL, r.Repo, r.Number, st.Head, nonce, st.promptChecks(), r.Instructions))
			if err != nil {
				r.State, r.Note = StateWaiting, "could not start the review: "+github.Printable(err.Error(), 300)
				return true
			}
			r.State, r.TaskID, r.PendingSHA, r.Nonce, r.Note, r.GoAhead = StateReviewing, id, st.Head, nonce, "", false
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

// expire stops a watch that has run past MaxWatch and reports whether it did.
// Only check calls it, so a review under way finishes and keeps its verdict.
func (w *Watcher) expire(r *Review) bool {
	if w.clock().Sub(r.WatchedAt) <= MaxWatch {
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
	// stale posts a verdict for a commit that is no longer the PR's head.
	stale bool
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

// planStale decides what to post for a verdict on a commit the PR has moved
// past. The comment stays pinned to the reviewed commit, and an older commit
// is never approved, nor are its findings replaced with approving text while
// the new commit is unreviewed. A verdict with no HeadSHA is being redone at
// the user's request, so it is not posted.
func planStale(r Review) *posting {
	if r.HeadSHA == "" || r.Verdict == "" {
		return nil
	}
	p := planComment(r)
	if p == nil || p.event == EventRefresh {
		return nil
	}
	p.stale = true
	return p
}

// send submits p with the comment as stored now, unless the user changed
// their choice or new commits arrived meanwhile. While it posts, the user's
// changes to the PR are refused (ErrPosting), and the outcome is recorded
// whatever state the row is in, so the app never shows less than was posted.
func (w *Watcher) send(ctx context.Context, url string, p posting) {
	w.mu.Lock()
	r, found, err := w.Store.Get(ctx, url)
	if err != nil || !found || r.HeadSHA != p.commit || (r.State != StateVerdict && !(p.stale && r.Active())) ||
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
		r.Posted = p.posted
		if !p.stale {
			// A stale verdict's note says what the new commit is waiting for.
			r.Note = ""
		}
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

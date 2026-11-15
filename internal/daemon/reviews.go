package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/reviews"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/supervisor"
	"github.com/willesq/orchestrator/internal/workflows"
)

// UnitReviews names the PR review watcher's unit.
const UnitReviews = "pr-reviews"

// reviewsTick is how often the watcher looks at its PRs. Each PR's status is
// read from GitHub only every reviews.DefaultPoll, or right after the user
// changes it; between those a tick reads only the local ledger.
const reviewsTick = 15 * time.Second

// newReviewWatcher wires the watcher to the dispatch queue, the ledger and the
// shared GitHub identity. A nil ctl leaves reviews running when their PR moves.
func newReviewWatcher(store *reviews.Store, disp dispatcher, tasks taskReader, ctl *queue.Control, gh *github.Service, ghp *ghProfiles) *reviews.Watcher {
	w := &reviews.Watcher{
		Store: store,
		GH:    ghp.Routed(),
		Post:  reviews.NewGHPoster(ghp.Env),
		// Unrouted, so the agent's own model runs: the review fans out to
		// subagents, which only Claude Code sessions have. The task reads
		// the memory of the space the PR's owner is bound to (SP-026).
		Dispatch: func(ctx context.Context, repo, body string) (string, error) {
			ctx = queue.WithReadSpace(ctx, reviewSpace(ghp, repo))
			return disp.Dispatch(ctx, reviews.Agent, body, "", workflows.Route{})
		},
		Task: func(ctx context.Context, id string) (string, string, bool, error) {
			t, ok, err := tasks.Get(ctx, id)
			return t.Status, t.Result, ok, err
		},
		Login: func(ctx context.Context) (string, error) {
			id, err := gh.Identity(ctx, false)
			return id.Login, err
		},
	}
	if ghp != nil {
		w.CheckOwner = docsOwnerCheck(ghp.spaces)
	}
	if ctl != nil {
		w.Cancel = func(ctx context.Context, id, reason string) error {
			_, err := ctl.CancelFor(ctx, id, reason)
			return err
		}
	}
	return w
}

// reviewSpace is the space a review task of repo's PR ("owner/name") reads
// and is recorded in: its owner's space (watchedOwnerSpace).
func reviewSpace(ghp *ghProfiles, repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return watchedOwnerSpace(ghp, owner)
}

// watchedOwnerSpace is the space GitHub owner's watched items and review
// tasks belong to: the space its org is bound to, Quarantine (public
// memory only) for an owner no space binds, and "" with no policy applied
// or the policy unreadable, as a dispatch records then (SP-026).
func watchedOwnerSpace(ghp *ghProfiles, owner string) string {
	if ghp == nil || ghp.spaces == nil {
		return ""
	}
	res, err := ghp.spaces.Current()
	if err != nil {
		// Reason: with the policy unreadable the task records no space,
		// so it reads public memory only, and an event shows only in the
		// operator's views.
		return ""
	}
	return orgSpaceOf(res, owner)
}

// reviewsPoll is pr_reviews.poll_interval, never below the unit's tick, which
// would only read as the tick anyway.
func reviewsPoll(spec string) time.Duration {
	return max(interval(spec, reviews.DefaultPoll), reviewsTick)
}

func reviewsUnit(w *reviews.Watcher) supervisor.Unit {
	return supervisor.Unit{
		Name:     UnitReviews,
		Detail:   "reviews the PRs you watch once they are ready; posts only what you opted each in to",
		Tick:     w.Tick,
		Interval: reviewsTick,
	}
}

func (s *server) reviewWatcher() (*reviews.Watcher, error) {
	if s.reviews == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "PR reviews unavailable")
	}
	return s.reviews, nil
}

// Reason: a watch can approve PRs, and verdicts carry text drawn from
// other people's code, so neither belongs in a task's reach.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ListPRReviews_FullMethodName, "read PR review verdicts",
	spaceDecision{spaceRecords, "PR review verdicts, by their PR owner's space (SP-026)"}))

func (s *server) ListPRReviews(ctx context.Context, _ *orchdv1.ListPRReviewsRequest) (*orchdv1.ListPRReviewsResponse, error) {
	w, err := s.reviewWatcher()
	if err != nil {
		return nil, err
	}
	list, err := w.List(ctx)
	if err != nil {
		return nil, err
	}
	out := &orchdv1.ListPRReviewsResponse{Reviews: make([]*orchdv1.PRReview, 0, len(list))}
	scope := s.recordScope(ctx, orchdv1.Orchd_ListPRReviews_FullMethodName)
	for _, r := range list {
		// Reason: a review, its verdict and report are its PR's space's
		// (SP-026).
		if !scope.seesRepo(r.Repo) {
			continue
		}
		pb := reviewPB(r)
		checked, next := w.Schedule(r)
		pb.CheckedAt, pb.NextCheckAt = unixOrZero(checked), unixOrZero(next)
		out.Reviews = append(out.Reviews, pb)
	}
	return out, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_WatchPRReview_FullMethodName, "change PR review watches",
	spaceDecision{spaceNone, "starts a review watch, only from the PR owner's space (SP-026)"}))

func (s *server) WatchPRReview(ctx context.Context, req *orchdv1.WatchPRReviewRequest) (*orchdv1.PRReview, error) {
	w, err := s.reviewWatcher()
	if err != nil {
		return nil, err
	}
	repo, _, err := reviews.ParseURL(req.GetUrl())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	// Reason: a watch dispatches a review task that reads the PR owner's
	// space with the caller's instructions, and its answer carries the
	// review's report, so only that space's callers start or change one
	// (SP-026).
	if !changesRepo(ctx, s.recordScope(ctx, orchdv1.Orchd_WatchPRReview_FullMethodName), repo) {
		return nil, errNoPR
	}
	if !reviews.ValidMode(req.GetMode()) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "mode must be %s, %s, %s, %s or %s",
			reviews.ModeVerdict, reviews.ModeApprove, reviews.ModeComment, reviews.ModeFeedback, reviews.ModeAuto)
	}
	var comment *string
	if req.GetSetComment() {
		c := req.GetComment()
		comment = &c
	}
	var instructions *string
	if req.GetSetInstructions() {
		in := req.GetInstructions()
		if len(in) > reviews.MaxInstructions {
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "instructions are %d bytes; the limit is %d", len(in), reviews.MaxInstructions)
		}
		instructions = &in
	}
	var updateInPlace *bool
	if req.GetSetUpdateInPlace() {
		u := req.GetUpdateInPlace()
		updateInPlace = &u
	}
	r, err := w.WatchOpts(ctx, req.GetUrl(), req.GetMode(), comment, instructions, req.GetReviewAnyway(), updateInPlace)
	if errors.Is(err, reviews.ErrPosting) {
		return nil, grpcstatus.Error(codes.Aborted, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return reviewPB(r), nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_SetPRReviewUpdateInPlace_FullMethodName, "change PR review watches",
	spaceDecision{spaceNone, "changes a review watch, only from the PR owner's space (SP-026)"}))

func (s *server) SetPRReviewUpdateInPlace(ctx context.Context, req *orchdv1.SetPRReviewUpdateInPlaceRequest) (*orchdv1.PRReview, error) {
	w, err := s.reviewWatcher()
	if err != nil {
		return nil, err
	}
	if repo, _, err := reviews.ParseURL(req.GetUrl()); err == nil &&
		!changesRepo(ctx, s.recordScope(ctx, orchdv1.Orchd_SetPRReviewUpdateInPlace_FullMethodName), repo) {
		return nil, errNoPR
	}
	r, err := w.SetUpdateInPlace(ctx, req.GetUrl(), req.GetUpdateInPlace())
	switch {
	case errors.Is(err, reviews.ErrNotFound):
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	case errors.Is(err, reviews.ErrPosting):
		return nil, grpcstatus.Error(codes.Aborted, err.Error())
	case err != nil:
		return nil, err
	}
	return reviewPB(r), nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_StopPRReview_FullMethodName, "change PR review watches",
	spaceDecision{spaceNone, "stops a review watch, only from the PR owner's space (SP-026)"}))

func (s *server) StopPRReview(ctx context.Context, req *orchdv1.StopPRReviewRequest) (*orchdv1.StopPRReviewResponse, error) {
	w, err := s.reviewWatcher()
	if err != nil {
		return nil, err
	}
	if repo, _, err := reviews.ParseURL(req.GetUrl()); err == nil &&
		!changesRepo(ctx, s.recordScope(ctx, orchdv1.Orchd_StopPRReview_FullMethodName), repo) {
		return nil, errNoPR
	}
	if err := w.Stop(ctx, req.GetUrl(), req.GetForget()); errors.Is(err, reviews.ErrNotFound) {
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	} else if errors.Is(err, reviews.ErrPosting) {
		return nil, grpcstatus.Error(codes.Aborted, err.Error())
	} else if err != nil {
		return nil, err
	}
	return &orchdv1.StopPRReviewResponse{}, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_GetPRDetail_FullMethodName, "read PR details through orchd (use gh)",
	spaceDecision{spaceRecords, "PR titles and bodies; another space's PR is NotFound before the detail cache is read (SP-026, RC-351)"}))

func (s *server) GetPRDetail(ctx context.Context, req *orchdv1.GetPRDetailRequest) (*orchdv1.PRDetail, error) {
	repo, _, err := reviews.ParseURL(req.GetUrl())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	// Reason: checked before the detail cache, which is keyed by URL alone
	// (RC-351), so a PR of another space is neither served from it nor
	// fetched into it for this caller (SP-026). It reads as a PR GitHub
	// does not show this caller.
	if !s.recordScope(ctx, orchdv1.Orchd_GetPRDetail_FullMethodName).seesRepo(repo) {
		return nil, errNoPR
	}
	if s.details == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "PR details unavailable")
	}
	d, at, err := s.details.get(ctx, req.GetUrl(), req.GetRefresh())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Unavailable, "GitHub: %v", err)
	}
	return detailPB(d, at), nil
}

// detailTTL bounds how often one PR's detail costs a gh call: the app asks
// each time a PR is selected.
const detailTTL = time.Minute

// detailCacheSize bounds the cache; past it the oldest entry goes.
const detailCacheSize = 64

type detailEntry struct {
	d  reviews.Detail
	at time.Time
}

// detailCache serves PR details from memory for detailTTL. A forced refresh
// is honoured at most every 10 s per PR.
type detailCache struct {
	gh  github.Runner
	now func() time.Time

	mu      sync.Mutex
	entries map[string]detailEntry
}

func newDetailCache(gh github.Runner) *detailCache {
	return &detailCache{gh: gh, now: time.Now, entries: map[string]detailEntry{}}
}

func (c *detailCache) get(ctx context.Context, url string, refresh bool) (reviews.Detail, time.Time, error) {
	c.mu.Lock()
	e, ok := c.entries[url]
	c.mu.Unlock()
	age := c.now().Sub(e.at)
	if ok && (age < 10*time.Second || (!refresh && age < detailTTL)) {
		return e.d, e.at, nil
	}
	d, err := reviews.FetchDetail(ctx, c.gh, url)
	if err != nil {
		return reviews.Detail{}, time.Time{}, err
	}
	at := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= detailCacheSize {
		oldest := ""
		for k, v := range c.entries {
			if oldest == "" || v.at.Before(c.entries[oldest].at) {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[url] = detailEntry{d: d, at: at}
	return d, at, nil
}

func reviewPB(r reviews.Review) *orchdv1.PRReview {
	return &orchdv1.PRReview{
		Url: r.URL, Repo: r.Repo, Number: clampInt32(r.Number), Mode: r.Mode, State: r.State,
		HeadSha: r.HeadSHA, PendingSha: r.PendingSHA, TaskId: r.TaskID, Verdict: r.Verdict,
		Reason: r.Reason, Comment: r.Comment, Instructions: r.Instructions, ForUser: r.ForUser, Report: r.Report, GoAhead: r.GoAhead, UpdateInPlace: r.UpdateInPlace, Posted: r.Posted, Note: r.Note,
		CreatedAt: unixOrZero(r.CreatedAt), UpdatedAt: unixOrZero(r.UpdatedAt), VerdictAt: unixOrZero(r.VerdictAt),
	}
}

func detailPB(d reviews.Detail, at time.Time) *orchdv1.PRDetail {
	out := &orchdv1.PRDetail{
		Url: d.URL, Title: d.Title, Body: d.Body, Author: d.Author, State: d.State, IsDraft: d.IsDraft,
		BaseRef: d.Base, HeadRef: d.HeadRef, HeadSha: d.Head,
		Additions: clampInt32(d.Additions), Deletions: clampInt32(d.Deletions), ChangedFiles: clampInt32(d.ChangedFiles),
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Mergeable: d.Mergeable, ReviewDecision: d.ReviewDecision,
		CiState: d.CI, Labels: d.Labels, RequestedReviewers: d.RequestedReviewers, FetchedAt: at.Unix(),
	}
	for _, r := range d.Reviews {
		out.Reviews = append(out.Reviews, &orchdv1.PRReviewerState{Login: r.Login, State: r.State})
	}
	for _, c := range d.Checks {
		out.Checks = append(out.Checks, &orchdv1.PRCheck{Name: c.Name, Result: c.Result})
	}
	for _, f := range d.Files {
		out.Files = append(out.Files, &orchdv1.PRFile{Path: f.Path, Additions: clampInt32(f.Additions), Deletions: clampInt32(f.Deletions)})
	}
	return out
}

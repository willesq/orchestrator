package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/landing"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// activityLimit is how many steps of a session the app is sent.
const activityLimit = 200

// landingRetention is how long a proposal's bare repository is kept.
const landingRetention = 30 * 24 * time.Hour

// newLandingService wires landing to the ledger and the shared GitHub identity.
func newLandingService(store *landing.Store, tasks taskReader, gh *github.Service, ghp *ghProfiles) *landing.Service {
	svc := &landing.Service{
		Store: store,
		Dir:   filepath.Join(paths.DataDir(), "landings"),
		Home:  userHome(),
		GH:    ghp.Routed(),
		Git:   landing.Git,
		Login: func(ctx context.Context) (string, error) {
			id, err := gh.Identity(ctx, false)
			return id.Login, err
		},
		Task: func(ctx context.Context, id string) (landing.Task, bool, error) {
			t, ok, err := tasks.Get(ctx, id)
			return landing.Task{Body: t.Body, Result: t.Result, Status: t.Status}, ok, err
		},
	}
	if ghp != nil {
		// A space's PRs are its account's, and pushes to them run as it.
		svc.AccountFor = func(ctx context.Context, owner string) (landing.Account, error) {
			env, login, err := ghp.account(ctx, owner, svc.Login)
			return landing.Account{Login: login, Env: env}, err
		}
	}
	svc.Prune(landingRetention, time.Now())
	return svc
}

// Reason: landing is the user's gate on what an agent proposed, so an
// agent must not reach it; a transcript may quote other tasks' secrets.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_GetTaskActivity_FullMethodName, "read other sessions' activity",
	spaceDecision{spaceRecords, "a task's transcript; another space's task has none (SP-026)"}))

func (s *server) GetTaskActivity(ctx context.Context, req *orchdv1.GetTaskActivityRequest) (*orchdv1.TaskActivity, error) {
	if req.GetId() == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "task id required")
	}
	// Reason: another space's task reads as one with no transcript
	// (SP-026), as a task that never ran does.
	if !s.seesTask(ctx, s.recordScope(ctx, orchdv1.Orchd_GetTaskActivity_FullMethodName), req.GetId()) {
		return &orchdv1.TaskActivity{}, nil
	}
	activity, err := agentrun.ReadActivity(s.home, req.GetId(), activityLimit)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "read the session's transcript: %v", err)
	}
	//nolint:gosec // every count is bounded by the transcript tail read
	out := &orchdv1.TaskActivity{
		Available: activity.Available, Total: int32(activity.Total), FilesRead: int32(activity.FilesRead),
		Commands: int32(activity.Commands), Failed: int32(activity.Failed),
		StartedAt: activity.StartedAt, LastAt: activity.LastAt, Pushed: activity.Pushed, Risky: int32(activity.Risky),
	}
	for _, step := range activity.Steps {
		//nolint:gosec // as above
		out.Steps = append(out.Steps, &orchdv1.TaskActivityStep{
			At: step.At, Kind: step.Kind, Text: step.Text, Risk: step.Risk, Tool: step.Tool, Title: step.Title,
			Detail: step.Detail, Commands: step.Commands, Failed: step.Failed, Output: step.Output,
			DurationMs: step.DurationMS, Additions: int32(step.Additions), Deletions: int32(step.Deletions),
			Seq: int32(step.Seq), Finished: step.Finished, CommandTotal: int32(step.CommandTotal),
		})
	}
	for _, tool := range activity.Tools {
		out.Tools = append(out.Tools, &orchdv1.TaskActivityTool{Name: tool.Name, Count: int32(tool.Count)}) //nolint:gosec // as above
	}
	for _, file := range activity.Files {
		//nolint:gosec // as above
		out.FilesEdited = append(out.FilesEdited, &orchdv1.TaskActivityFile{Path: file.Path, Additions: int32(file.Additions), Deletions: int32(file.Deletions)})
	}
	return out, nil
}

func (s *server) landingService() (*landing.Service, error) {
	if s.landing == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "PR landings unavailable")
	}
	return s.landing, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_GetPRLanding_FullMethodName, "read PR landings",
	spaceDecision{spaceRecords, "a PR landing, by its task's and PR's spaces (SP-026)"}))

func (s *server) GetPRLanding(ctx context.Context, req *orchdv1.PRLandingRequest) (*orchdv1.PRLanding, error) {
	svc, err := s.landingService()
	if err != nil {
		return nil, err
	}
	// Reason: another space's landing reads as a task that does not exist
	// (SP-026); the task is checked first, so its repo is not fetched.
	scope := s.recordScope(ctx, orchdv1.Orchd_GetPRLanding_FullMethodName)
	if !s.seesTask(ctx, scope, req.GetTaskId()) {
		return landingResult(landing.Landing{}, landing.ErrNotFound)
	}
	l, err := svc.Get(ctx, req.GetTaskId())
	if err == nil && !s.seesLanding(ctx, scope, l.TaskID, l.URL) {
		return landingResult(landing.Landing{}, landing.ErrNotFound)
	}
	return landingResult(l, err)
}

// seesLanding reports whether scope reads the landing of task id's PR at
// url: the task must be in its spaces, and so must the PR's owner when
// the task named a PR.
func (s *server) seesLanding(ctx context.Context, scope readScope, id, url string) bool {
	return (url == "" || scope.seesGitHubURL(url)) && s.seesTask(ctx, scope, id)
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ListPRLandings_FullMethodName, "read PR landings",
	spaceDecision{spaceRecords, "PR landings, by their tasks' and PRs' spaces (SP-026)"}))

func (s *server) ListPRLandings(ctx context.Context, _ *orchdv1.ListPRLandingsRequest) (*orchdv1.ListPRLandingsResponse, error) {
	svc, err := s.landingService()
	if err != nil {
		return nil, err
	}
	records, err := svc.Store.List(ctx)
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	out := &orchdv1.ListPRLandingsResponse{}
	scope := s.recordScope(ctx, orchdv1.Orchd_ListPRLandings_FullMethodName)
	for _, r := range records {
		if !s.seesLanding(ctx, scope, r.TaskID, r.URL) {
			continue
		}
		out.Decisions = append(out.Decisions, &orchdv1.PRLandingDecision{TaskId: r.TaskID, Url: r.URL, State: r.State})
	}
	return out, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_LandPR_FullMethodName, "push proposed commits",
	spaceDecision{spaceNone, "pushes proposed commits"}))

func (s *server) LandPR(ctx context.Context, req *orchdv1.LandPRRequest) (*orchdv1.PRLanding, error) {
	svc, err := s.landingService()
	if err != nil {
		return nil, err
	}
	return landingResult(svc.Land(ctx, req.GetTaskId(), req.GetSha()))
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_UndoPRLanding_FullMethodName, "push proposed commits",
	spaceDecision{spaceNone, "undoes a landing"}))

func (s *server) UndoPRLanding(ctx context.Context, req *orchdv1.PRLandingRequest) (*orchdv1.PRLanding, error) {
	svc, err := s.landingService()
	if err != nil {
		return nil, err
	}
	return landingResult(svc.Undo(ctx, req.GetTaskId()))
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_DiscardPRLanding_FullMethodName, "decide PR landings",
	spaceDecision{spaceNone, "discards a landing"}))

func (s *server) DiscardPRLanding(ctx context.Context, req *orchdv1.DiscardPRLandingRequest) (*orchdv1.PRLanding, error) {
	svc, err := s.landingService()
	if err != nil {
		return nil, err
	}
	return landingResult(svc.Discard(ctx, req.GetTaskId(), req.GetRestore()))
}

func landingResult(l landing.Landing, err error) (*orchdv1.PRLanding, error) {
	switch {
	case err == nil:
		return landingPB(l), nil
	case errors.Is(err, landing.ErrNotFound):
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	case errors.Is(err, landing.ErrRefused):
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	// A failed push or gh call: GitHub's answer is what the user needs.
	return nil, grpcstatus.Error(codes.Unavailable, err.Error())
}

func landingPB(l landing.Landing) *orchdv1.PRLanding {
	out := &orchdv1.PRLanding{
		TaskId: l.TaskID, Url: l.URL, State: l.State, Problem: l.Problem, Path: l.Path, Branch: l.Branch,
		BaseSha: l.BaseSHA, Sha: l.SHA, TotalCommits: int32(l.TotalCommits), Diff: l.Diff, //nolint:gosec // a commit count
		TotalFiles: int32(l.TotalFiles), Additions: int32(l.Additions), Deletions: int32(l.Deletions), //nolint:gosec // counts
		BinaryFiles:   int32(l.BinaryFiles), //nolint:gosec // a count
		DiffTruncated: l.DiffTruncated, CanLand: l.CanLand, Blocked: l.Blocked, CanUndo: l.CanUndo,
		RevertSha: l.RevertSHA, DecidedAt: l.DecidedAt,
	}
	for _, c := range l.Commits {
		out.Commits = append(out.Commits, &orchdv1.PRLandingCommit{Sha: c.SHA, Subject: c.Subject, Author: c.Author})
	}
	for _, f := range l.Files {
		out.Files = append(out.Files, &orchdv1.PRFile{Path: f.Path, Additions: int32(f.Additions), Deletions: int32(f.Deletions)}) //nolint:gosec // line counts
	}
	return out
}

package daemon

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func (s *server) githubService() (*github.Service, error) {
	if s.github == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "GitHub scopes unavailable")
	}
	return s.github, nil
}

func (s *server) GetGitHubIdentity(ctx context.Context, req *orchdv1.GetGitHubIdentityRequest) (*orchdv1.GitHubIdentity, error) {
	svc, err := s.githubService()
	if err != nil {
		return nil, err
	}
	id, err := svc.Identity(ctx, req.GetRefresh())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Unavailable, "GitHub: %v", err)
	}
	out := &orchdv1.GitHubIdentity{Login: id.Login, Orgs: id.Orgs}
	for _, t := range id.Teams {
		out.Teams = append(out.Teams, &orchdv1.GitHubTeam{Org: t.Org, Slug: t.Slug, Name: t.Name})
	}
	return out, nil
}

func (s *server) ListGitHubScopes(ctx context.Context, req *orchdv1.ListGitHubScopesRequest) (*orchdv1.ListGitHubScopesResponse, error) {
	svc, err := s.githubService()
	if err != nil {
		return nil, err
	}
	scopes, ghErr, err := svc.Scopes(ctx, req.GetRefresh())
	if err != nil {
		return nil, err
	}
	out := &orchdv1.ListGitHubScopesResponse{}
	if ghErr != nil {
		out.Warning = "Could not reach GitHub: " + ghErr.Error()
	}
	_, op := operator(ctx)
	for _, sc := range scopes {
		// Reason: an org a space reaches through its own gh profile is
		// that job's, so only the operator sees it (SP-074).
		if !op && s.ghProfiles.ownsProfile(sc.Owner) {
			continue
		}
		out.Scopes = append(out.Scopes, scopePB(sc))
	}
	return out, nil
}

func (s *server) SetGitHubScope(ctx context.Context, req *orchdv1.SetGitHubScopeRequest) (*orchdv1.SetGitHubScopeResponse, error) {
	svc, err := s.githubService()
	if err != nil {
		return nil, err
	}
	in := req.GetScope()
	if in == nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "scope is required")
	}
	// Only discovered scopes (the user's account, their orgs) can be changed,
	// and their kind stays what discovery found.
	current, err := svc.Store.Get(ctx, in.GetOwner())
	if errors.Is(err, github.ErrNotFound) {
		return nil, grpcstatus.Errorf(codes.NotFound, "%q is not your account or an org you belong to", in.GetOwner())
	}
	if err != nil {
		return nil, err
	}
	// An org the user has left (or a renamed login) keeps its row but cannot be
	// turned on; when GitHub is unreachable the stored row is trusted. The
	// check runs as the login the owner's space names (SP-074).
	if in.GetEnabled() && !current.Enabled {
		member, mErr := svc.Member(ctx, current.Owner)
		switch {
		case refusedProfile(mErr):
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "%s: %v", current.Owner, mErr)
		case mErr == nil && !member:
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "%s: %v", current.Owner, github.ErrNotMember)
		}
	}
	next := current
	next.Enabled = in.GetEnabled()
	next.Items = in.GetItems()
	next.RepoMode = in.GetRepoMode()
	next.Repos = in.GetRepos()
	next.EverythingRepos = in.GetEverythingRepos()
	stored, err := svc.Store.Set(ctx, next)
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	// Sweep now rather than at the next poll, so the change shows within
	// seconds; a failure only means waiting for the regular poll.
	_ = handoff.RequestPRSweep(s.handoffDir)
	return &orchdv1.SetGitHubScopeResponse{Scope: scopePB(stored)}, nil
}

// RequestGitHubSweep leaves the watcher a request; prSweepDue decides when it
// runs, so repeated calls add up to at most one sweep a minute.
func (s *server) RequestGitHubSweep(context.Context, *orchdv1.RequestGitHubSweepRequest) (*orchdv1.RequestGitHubSweepResponse, error) {
	if err := handoff.RequestPRSweep(s.handoffDir); err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "request PR sweep: %v", err)
	}
	return &orchdv1.RequestGitHubSweepResponse{}, nil
}

func (s *server) ListGitHubRepos(ctx context.Context, req *orchdv1.ListGitHubReposRequest) (*orchdv1.ListGitHubReposResponse, error) {
	svc, err := s.githubService()
	if err != nil {
		return nil, err
	}
	if !github.ValidLogin(req.GetOwner()) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid GitHub login %q", req.GetOwner())
	}
	// Reason: listing as a space's own gh profile uses that job's account,
	// so agents get the answer they get for an org that is not theirs, and
	// cannot tell the two apart (SP-074).
	if _, op := operator(ctx); !op && s.ghProfiles.ownsProfile(req.GetOwner()) {
		return nil, grpcstatus.Errorf(codes.NotFound, "%s: %v", req.GetOwner(), github.ErrNotMember)
	}
	repos, err := svc.Repos(ctx, req.GetOwner())
	if errors.Is(err, github.ErrNotMember) {
		return nil, grpcstatus.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Unavailable, "GitHub: %v", err)
	}
	out := &orchdv1.ListGitHubReposResponse{}
	for _, r := range repos {
		out.Repos = append(out.Repos, &orchdv1.GitHubRepo{Name: r.Name, Private: r.Private, Archived: r.Archived})
	}
	return out, nil
}

func scopePB(s github.Scope) *orchdv1.GitHubScope {
	return &orchdv1.GitHubScope{
		Owner: s.Owner, Kind: s.Kind, Enabled: s.Enabled, Items: s.Items,
		RepoMode: s.RepoMode, Repos: s.Repos, EverythingRepos: s.EverythingRepos, UpdatedAt: s.UpdatedAt,
	}
}

// watchFrom reports the user's login and scopes to the PR watcher. Without
// GitHub the scopes cannot be checked against the user's memberships, so the
// sweep fails and the cache is kept.
func watchFrom(svc *github.Service, ghp *ghProfiles) prs.WatchSource {
	return func(ctx context.Context) (prs.Watch, error) {
		if svc == nil {
			return prs.Watch{}, errors.New("GitHub scopes unavailable")
		}
		accounts, route, err := ghp.watchAccounts(ctx)
		if err != nil {
			return prs.Watch{}, err
		}
		scopes, ghErr, err := svc.Scopes(ctx, false)
		if err != nil {
			return prs.Watch{}, err
		}
		if ghErr != nil {
			return prs.Watch{}, ghErr
		}
		id, err := svc.Identity(ctx, false)
		if err != nil {
			return prs.Watch{}, err
		}
		w := prs.Watch{Login: id.Login, Scopes: scopes, Accounts: accounts, Route: route}
		if ghp != nil {
			w.CheckOwner = docsOwnerCheck(ghp.spaces)
		}
		return w, nil
	}
}

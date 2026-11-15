package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// refreshSecs reads docs.default_refresh, falling back to 6 hours.
func refreshSecs(d string) int64 {
	if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
		return int64(parsed.Seconds())
	}
	return int64((6 * time.Hour).Seconds())
}

func docOwnerPB(o docs.Owner) *orchdv1.DocOwner {
	mode := o.RepoMode
	if mode == "" {
		mode = docs.ReposAll
	}
	return &orchdv1.DocOwner{
		Owner: o.Owner, RefreshSecs: o.RefreshSecs, Scope: o.Scope, Tags: o.Tags,
		LastSynced: o.LastSynced, Status: o.Status, Error: o.Error,
		Enabled: o.Enabled, RepoMode: mode, SelectedRepos: o.Repos,
	}
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ListDocsOwners_FullMethodName, "read docs owners (use ListDocs)",
	spaceDecision{spaceRecords, "docs owners and their repo choices, by their owner's space (SP-026, RC-700)"}))

func (s *server) ListDocsOwners(ctx context.Context, _ *orchdv1.ListDocsOwnersRequest) (*orchdv1.ListDocsOwnersResponse, error) {
	if s.docsReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs registry unavailable")
	}
	owners, err := s.docsReg.ListOwners(ctx)
	if err != nil {
		return nil, err
	}
	out := &orchdv1.ListDocsOwnersResponse{}
	scope := s.recordScope(ctx, orchdv1.Orchd_ListDocsOwners_FullMethodName)
	for _, o := range owners {
		// Reason: an owner and its repos are its org's space's (SP-026).
		if !scope.seesOwner(o.Owner) {
			continue
		}
		pb := docOwnerPB(o)
		repos, err := s.docsReg.ListRepos(ctx, o.Owner)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			pb.Repos = append(pb.Repos, &orchdv1.DocRepo{
				Repo: r.Repo, LastSynced: r.LastSynced, Files: clampInt32(r.Files), Status: r.Status, Error: r.Error,
			})
		}
		out.Owners = append(out.Owners, pb)
	}
	return out, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ListDocsOwnerRepos_FullMethodName, "ask orchd to list repos on GitHub",
	spaceDecision{spaceRecords, "an owner's repos on GitHub; another space's owner is NotFound (SP-026, RC-700)"}))

func (s *server) ListDocsOwnerRepos(ctx context.Context, req *orchdv1.ListDocsOwnerReposRequest) (*orchdv1.ListDocsOwnerReposResponse, error) {
	if s.docsGH == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs sync unavailable")
	}
	if !github.ValidLogin(req.GetOwner()) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid GitHub login %q", req.GetOwner())
	}
	// Reason: another space's owner reads as one GitHub does not show
	// this caller (SP-026).
	if !s.recordScope(ctx, orchdv1.Orchd_ListDocsOwnerRepos_FullMethodName).seesOwner(req.GetOwner()) {
		return nil, grpcstatus.Errorf(codes.NotFound, "GitHub: %s: %v", req.GetOwner(), github.ErrNotMember)
	}
	gh := s.docsGH
	if s.ghProfiles != nil {
		// A space's org is listed as its account, or not at all.
		env, err := s.ghProfiles.Env(ctx, req.GetOwner())
		if err != nil {
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "GitHub: %v", err)
		}
		if env != nil {
			gh = docs.NewProfileGitHub(env)
		}
	}
	repos, err := gh.ListRepos(ctx, req.GetOwner())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Unavailable, "GitHub: %v", err)
	}
	out := &orchdv1.ListDocsOwnerReposResponse{}
	for _, r := range repos {
		out.Repos = append(out.Repos, &orchdv1.DocsOwnerRepo{Name: r.Name, Archived: r.IsArchived, Fork: r.IsFork})
	}
	return out, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_SaveDocsOwner_FullMethodName, "change which docs are synced",
	spaceDecision{spaceNone, "saves a docs owner, only from its org's space (SP-026)"}))

func (s *server) SaveDocsOwner(ctx context.Context, req *orchdv1.SaveDocsOwnerRequest) (*orchdv1.DocOwner, error) {
	if s.docsReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs registry unavailable")
	}
	owner := req.GetOwner()
	if !github.ValidLogin(owner) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid GitHub login %q", owner)
	}
	// Reason: an owner's docs are its org's space's, so only that space's
	// callers change what is synced (SP-026); to anyone else it reads as
	// not registered.
	if !changesRepo(ctx, s.recordScope(ctx, orchdv1.Orchd_SaveDocsOwner_FullMethodName), owner+"/") {
		return nil, notDocsOwner(owner)
	}
	choice := docs.Choice{Enabled: req.GetEnabled(), RepoMode: req.GetRepoMode(), Repos: req.GetRepos(), RefreshSecs: req.GetRefreshSecs()}
	if err := choice.Validate(); err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	var saved docs.Owner
	err := s.whileNoDocsSync(func() error {
		var err error
		saved, err = s.saveDocsOwner(ctx, owner, choice)
		return err
	})
	if err != nil {
		return nil, err
	}
	log.Printf("docs: owner %s saved by the operator (enabled %v, repos %s)", owner, saved.Enabled, saved.RepoMode)
	return docOwnerPB(saved), nil
}

func (s *server) saveDocsOwner(ctx context.Context, owner string, choice docs.Choice) (docs.Owner, error) {
	saved, err := s.docsReg.SetChoice(ctx, owner, choice)
	if !errors.Is(err, docs.ErrNoOwner) {
		return saved, err
	}
	refresh := choice.RefreshSecs
	if refresh <= 0 {
		refresh = s.docsRefresh
	}
	if err := s.docsReg.AddOwner(ctx, docs.Owner{Owner: owner, RefreshSecs: refresh}); err != nil {
		return docs.Owner{}, err
	}
	return s.docsReg.SetChoice(ctx, owner, choice)
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_RemoveDocsOwner_FullMethodName, "change which docs are synced",
	spaceDecision{spaceNone, "removes a docs owner, only from its org's space; a purge is Orch.app's under a policy (SP-026, D-77)"}))

func (s *server) RemoveDocsOwner(ctx context.Context, req *orchdv1.RemoveDocsOwnerRequest) (*orchdv1.PurgeDocsResponse, error) {
	if s.docsReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs registry unavailable")
	}
	owner := req.GetOwner()
	if !github.ValidLogin(owner) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid GitHub login %q", owner)
	}
	if !changesRepo(ctx, s.recordScope(ctx, orchdv1.Orchd_RemoveDocsOwner_FullMethodName), owner+"/") {
		return nil, notDocsOwner(owner)
	}
	// Reason: a purge deletes synced docs, which under a policy only
	// Orch.app may do, as for PurgeDocs (D-77).
	if c := callerOf(ctx); req.GetPurge() && s.spacesConfigured() && (c.kind != callerOperator || c.operatorBy != operatorApp) {
		return nil, grpcstatus.Error(codes.PermissionDenied,
			"only Orch.app can purge synced docs while a spaces policy is applied (D-77); remove the owner without purging, or ask the user")
	}
	found, err := s.findDocsOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	owner = found.Owner
	if req.GetPurge() && !req.GetConfirm() {
		return nil, errPurgeUnconfirmed
	}
	out := &orchdv1.PurgeDocsResponse{}
	err = s.whileNoDocsSync(func() error {
		// Purge before forgetting the owner, so a failed purge leaves it
		// registered and the removal can be retried.
		if req.GetPurge() {
			if err := s.purgeDocs(ctx, owner, nil, out); err != nil {
				return err
			}
		}
		return s.docsReg.RemoveOwner(ctx, owner)
	})
	if err != nil {
		return nil, err
	}
	log.Printf("docs: owner %s removed by the operator (%d docs deleted)", owner, out.PurgedSources)
	return out, nil
}

var errPurgeUnconfirmed = grpcstatus.Error(codes.FailedPrecondition,
	"refusing to delete indexed docs without confirm=true: this is permanent and cannot be undone")

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_PurgeDocs_FullMethodName, "delete from the RAG store",
	spaceDecision{spaceNone, "deletes synced docs; under a policy Orch.app's alone (D-77)"}))

func (s *server) PurgeDocs(ctx context.Context, req *orchdv1.PurgeDocsRequest) (*orchdv1.PurgeDocsResponse, error) {
	if s.docsReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs registry unavailable")
	}
	if !req.GetConfirm() {
		return nil, errPurgeUnconfirmed
	}
	owner := req.GetOwner()
	if !github.ValidLogin(owner) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid GitHub login %q", owner)
	}
	for _, r := range req.GetRepos() {
		if !repoNameRe.MatchString(r) {
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid repo name %q", r)
		}
	}
	out := &orchdv1.PurgeDocsResponse{}
	err := s.whileNoDocsSync(func() error {
		o, err := s.findDocsOwner(ctx, owner)
		if err != nil {
			return err
		}
		owner = o.Owner
		repos := req.GetRepos()
		covered := func(r string) bool { return o.Enabled && o.Covers(r) }
		// The next sync would only index a covered repo again.
		if i := slices.IndexFunc(repos, covered); i >= 0 {
			return grpcstatus.Errorf(codes.FailedPrecondition,
				"%s/%s is still synced; opt it out before deleting its docs", owner, repos[i])
		}
		return s.purgeDocs(ctx, owner, func(repo string) bool {
			if covered(repo) {
				return false
			}
			return len(repos) == 0 || slices.ContainsFunc(repos, func(r string) bool { return strings.EqualFold(r, repo) })
		}, out)
	})
	if err != nil {
		return nil, err
	}
	log.Printf("docs: %d docs of %s deleted by the operator", out.PurgedSources, owner)
	return out, nil
}

func (s *server) findDocsOwner(ctx context.Context, owner string) (docs.Owner, error) {
	owners, err := s.docsReg.ListOwners(ctx)
	if err != nil {
		return docs.Owner{}, err
	}
	if i := slices.IndexFunc(owners, func(o docs.Owner) bool { return strings.EqualFold(o.Owner, owner) }); i >= 0 {
		return owners[i], nil
	}
	return docs.Owner{}, grpcstatus.Errorf(codes.NotFound, "%s is not a docs owner", owner)
}

// purgeDocs deletes the indexed docs of the owner's repos that match (all when
// match is nil), and forgets each repo once its docs are gone.
func (s *server) purgeDocs(ctx context.Context, owner string, match func(repo string) bool, out *orchdv1.PurgeDocsResponse) error {
	if s.rag == nil {
		return grpcstatus.Error(codes.Unavailable, "the RAG store is unavailable")
	}
	indexed, err := s.docsReg.ListDocs(ctx)
	if err != nil {
		return err
	}
	prefix := "repo://" + owner + "/"
	touched := map[string]bool{}
	for _, d := range indexed {
		rest, ok := strings.CutPrefix(d.URI, prefix)
		if !ok {
			continue
		}
		repo, _, ok := strings.Cut(rest, "/")
		if !ok || (match != nil && !match(repo)) {
			continue
		}
		n, err := s.rag.DeleteSource(ctx, d.URI)
		if err != nil {
			log.Printf("orchd: DELETE %q failed: %v", d.URI, err)
			return err
		}
		// Forget the hash with the doc: left behind, a later sync of the repo
		// would take the file as unchanged and never index it again.
		if err := s.docsReg.DeleteHash(ctx, d.URI); err != nil {
			return err
		}
		out.PurgedSources++
		out.PurgedChunks += clampInt32(n)
		touched[repo] = true
	}
	for repo := range touched {
		if err := s.docsReg.ForgetRepo(ctx, owner, repo); err != nil {
			return err
		}
	}
	return nil
}

// whileNoDocsSync runs change only while no docs sync can be writing the same
// rows, and refuses with what is running otherwise. A sync started from a stale
// choice would overwrite what the operator just decided.
func (s *server) whileNoDocsSync(change func() error) error {
	if s.docsSync == nil {
		return change()
	}
	err := s.docsSync.Request(change)
	if !errors.Is(err, docs.ErrBusy) {
		return err
	}
	p := s.docsSync.Progress()
	what := "looking up due owners"
	switch {
	case p.Owner != "" && p.Total > 0:
		what = fmt.Sprintf("%s, %d of %d repos", p.Owner, p.Done, p.Total)
	case p.Owner != "":
		what = p.Owner + ", listing repos"
	}
	return grpcstatus.Errorf(codes.Aborted, "a docs sync is running (%s); try again when it finishes", what)
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/prunecap"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// docsSyncer is the docs scheduler as the RPCs see it.
type docsSyncer interface {
	Progress() docs.Progress
	Running() bool
	Request(prepare func() error) error
}

func (s *server) SyncDocs(ctx context.Context, req *orchdv1.SyncDocsRequest) (*orchdv1.DocsSyncState, error) {
	if s.docsSync == nil || s.docsReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs sync unavailable")
	}
	owner := req.GetOwner()
	if req.GetConfirmPrune() && owner == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "confirm_prune needs an owner")
	}
	if owner != "" {
		if !github.ValidLogin(owner) {
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid GitHub login %q", owner)
		}
		if err := s.requireDocsOwner(ctx, owner); err != nil {
			return nil, err
		}
	}
	// One sync at a time, and no queue: while one runs the request is
	// refused, and the running sync is not interrupted. The caches are
	// cleared and the owner marked due only once no sync can be writing them.
	err := s.docsSync.Request(func() error {
		if req.GetConfirmPrune() {
			if s.prune == nil {
				return grpcstatus.Error(codes.Unavailable, "prune guard unavailable")
			}
			granted, err := s.prune.Confirm(ctx, prunecap.DocsKey(owner))
			if errors.Is(err, prunecap.ErrNothingRefused) {
				return grpcstatus.Errorf(codes.FailedPrecondition, "%s has no refused prune to confirm", owner)
			}
			if err != nil {
				return err
			}
			// A refused repo kept its new HEAD; forget it so this sync walks
			// the repo again and uses the grant.
			for _, p := range granted {
				if repo, ok := docsRepoOf(p, owner); ok {
					if err := s.docsReg.ForgetRepoSHA(ctx, owner, repo); err != nil {
						return err
					}
				}
			}
			log.Printf("docs: confirmed for an hour, by the operator: the refused prunes of %s (%s)", owner, strings.Join(granted, ", "))
		}
		if req.GetForce() {
			if err := s.docsReg.ClearCaches(ctx, owner); err != nil {
				return err
			}
		}
		return s.docsReg.MarkDue(ctx, owner)
	})
	if errors.Is(err, docs.ErrBusy) {
		p := s.docsSync.Progress()
		what := "looking up due owners"
		switch {
		case p.Owner != "" && p.Total > 0:
			what = fmt.Sprintf("%s, %d of %d repos", p.Owner, p.Done, p.Total)
		case p.Owner != "":
			what = p.Owner + ", listing repos"
		}
		return nil, grpcstatus.Errorf(codes.Aborted,
			"a docs sync is already running (%s, started %s ago); this request was not queued",
			what, time.Since(p.StartedAt).Round(time.Second))
	}
	if err != nil {
		return nil, err
	}
	log.Printf("docs: sync requested (owner %q, force %v)", owner, req.GetForce())
	return docsSyncState(s.docsSync.Progress()), nil
}

func (s *server) GetDocsSync(context.Context, *orchdv1.GetDocsSyncRequest) (*orchdv1.DocsSyncState, error) {
	if s.docsSync == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs sync unavailable")
	}
	return docsSyncState(s.docsSync.Progress()), nil
}

func (s *server) requireDocsOwner(ctx context.Context, owner string) error {
	owners, err := s.docsReg.ListOwners(ctx)
	if err != nil {
		return err
	}
	for _, o := range owners {
		if o.Owner == owner {
			return nil
		}
	}
	return grpcstatus.Errorf(codes.NotFound, "%s is not a docs owner; add it with: orch docs add %s", owner, owner)
}

func docsSyncState(p docs.Progress) *orchdv1.DocsSyncState {
	return &orchdv1.DocsSyncState{
		Running: p.Running, Owner: p.Owner, StartedAt: unixOrZero(p.StartedAt), FinishedAt: unixOrZero(p.FinishedAt),
		Done: clampInt32(p.Done), Total: clampInt32(p.Total), Repo: p.Repo,
	}
}

// docsRepoOf reads the repo from a docs prefix, repo://<owner>/<repo>/.
func docsRepoOf(prefix, owner string) (string, bool) {
	rest, ok := strings.CutPrefix(prefix, "repo://"+owner+"/")
	if !ok {
		return "", false
	}
	repo, _, ok := strings.Cut(rest, "/")
	return repo, ok && repo != ""
}

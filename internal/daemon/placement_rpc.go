package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/handoff"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// maxPlacementTarget bounds a target: a path is at most PATH_MAX, and URLs
// longer than this are not worth explaining.
const maxPlacementTarget = 4096

// errPlacementHidden is the one refusal an agent gets, whatever the reason,
// so it learns nothing about the space a target is in.
var errPlacementHidden = grpcstatus.Error(codes.PermissionDenied,
	"agents can explain only targets placed in a public space; run 'orch spaces why' in your own terminal for any other")

func (s *server) ExplainPlacement(ctx context.Context, req *orchdv1.ExplainPlacementRequest) (*orchdv1.PlacementExplanation, error) {
	if s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	target := strings.TrimSpace(req.GetTarget())
	if target == "" || len(target) > maxPlacementTarget {
		return nil, grpcstatus.Error(codes.InvalidArgument, "name a target: an absolute path, an http(s) URL, owner/repo, or slack:<id> / teams:<id>")
	}
	_, isOperator := operator(ctx)
	o, kind, err := s.placementOrigin(ctx, target)
	if err != nil {
		return nil, err
	}
	res, err := s.spaces.Current()
	var p spaces.Placement
	if err == nil {
		p, err = s.spaces.PlaceUnder(ctx, res, o)
	}
	switch {
	case err != nil && !isOperator:
		// Reason: an error raised only for some targets (a bound org's
		// binding read) would tell an agent more than the refusal does.
		log.Printf("orchd: ExplainPlacement: %v", err)
		return nil, errPlacementHidden
	case errors.Is(err, spaces.ErrBroken):
		return nil, grpcstatus.Error(codes.FailedPrecondition, "the applied spaces policy does not verify, so nothing is placed; apply a revision again")
	case err != nil:
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	// Reason: until sessions are pinned, orchd cannot tell an agent's own
	// space, so an agent sees only answers that name nothing but public
	// spaces, which every space reads anyway (INV-01).
	if !isOperator && !publicOnly(res, p) {
		return nil, errPlacementHidden
	}
	out := placementProto(p)
	out.TargetKind = kind
	if p.PublicPath != "" && isOperator {
		// Reason: the pattern is policy text an agent has no need of, and a
		// folder name in it may hold a term (SP-076.1.a4).
		out.Reason += fmt.Sprintf(" (public_paths pattern %q)", p.PublicPath)
	}
	if kind == "path" && isOperator {
		so := o
		so.System = spaces.SystemSession
		sp, err := s.spaces.PlaceUnder(ctx, res, so)
		if err != nil {
			return nil, grpcstatus.Error(codes.Internal, err.Error())
		}
		out.SessionSpace, out.SessionReads = sp.Space, sp.ReadAs
	}
	return out, nil
}

// publicOnly reports whether an agent may see p: every space it names is
// public, and any remote's owner is bound to a public space. Reason: the
// remote can come from a .git the agent wrote, so an answer that differs
// between an unbound owner and one bound to a non-public space would reveal
// which orgs the other spaces hold. Without a policy everything is shared.
func publicOnly(res *spaces.Resolved, p spaces.Placement) bool {
	if !res.Configured() {
		return true
	}
	if p.Remote != nil && p.Remote.Space == "" {
		return false
	}
	for _, name := range p.Spaces() {
		if sp, ok := res.Space(name); !ok || sp.Kind != spaces.Public {
			return false
		}
	}
	return true
}

// placementOrigin turns a target into the origin orchd would record for it.
// orchd reads a path's checkout and a repo's owner and visibility itself, so
// a caller cannot claim them.
func (s *server) placementOrigin(ctx context.Context, target string) (spaces.Origin, string, error) {
	switch {
	case strings.HasPrefix(target, "/"):
		o := pathOrigin(ctx, s.repos, target)
		// A folder's public_paths match is what the files directly in it
		// get (SP-076).
		if fi, err := os.Stat(o.Path); err == nil && fi.IsDir() {
			o.IsDir = true
		}
		return o, "path", nil
	case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"):
		return spaces.Origin{System: spaces.SystemWeb, URL: target, SourceURI: target}, "url", nil
	case strings.HasPrefix(target, "slack:"), strings.HasPrefix(target, "teams:"):
		provider, rest, _ := strings.Cut(target, ":")
		tenant, _, _ := strings.Cut(strings.TrimPrefix(rest, "//"), "/")
		if tenant == "" {
			break
		}
		return spaces.Origin{System: spaces.System(provider), Tenant: tenant}, "chat", nil
	}
	if owner, name, ok := strings.Cut(target, "/"); ok && github.ValidLogin(owner) && github.ValidRepo(name) {
		return spaces.Origin{System: spaces.SystemGitHub, Remote: s.repos.lookup(ctx, owner, name)}, "repo", nil
	}
	return spaces.Origin{}, "", grpcstatus.Error(codes.InvalidArgument,
		"target must be an absolute path, an http(s) URL, owner/repo, or slack:<id> / teams:<id>")
}

// pathOrigin is the origin orchd records for a local path: the path and,
// when it is in a GitHub checkout, that repo as GitHub reports it.
func pathOrigin(ctx context.Context, repos *repoLookups, path string) spaces.Origin {
	path = filepath.Clean(path)
	o := spaces.Origin{System: spaces.SystemFS, Path: path}
	c, ok := handoff.CheckoutAt(path)
	if !ok {
		c, ok = handoff.FindCheckout(path)
	}
	// An anonymous checkout's .git could borrow any repo's identity.
	if ok && !c.Anonymous && c.Owner != "" {
		o.Remote = repos.lookup(ctx, c.Owner, c.Repo)
	}
	return o
}

func placementProto(p spaces.Placement) *orchdv1.PlacementExplanation {
	out := &orchdv1.PlacementExplanation{Rule: string(p.Rule), Reason: p.Reason, Outcome: string(p.Outcome), Space: p.Space}
	for _, r := range p.Roots {
		out.Roots = append(out.Roots, &orchdv1.PlacementRoot{Root: r.Root, Space: r.Space, Kind: string(r.Kind)})
	}
	if r := p.Remote; r != nil {
		out.Remote = &orchdv1.PlacementRemote{
			Owner: r.Repo.Owner, Repo: r.Repo.Name, Visibility: string(r.Repo.Visibility),
			Space: r.Space, Kind: string(r.Kind), Effect: r.Effect,
		}
	}
	return out
}

// Bounds on repo lookups for placement: each spends the user's GitHub rate
// limit, and any caller can ask.
const (
	repoLookupTTL     = 10 * time.Minute
	repoLookupEntries = 512
	repoLookupsPerMin = 30
	// unansweredGrace is how long after GitHub last said a repo is public
	// a lookup that gets no answer still counts as a blip (RC-402). Reason:
	// any caller can spend the shared lookup budget, so without a limit an
	// agent could keep a repo that went private in public for as long as it
	// keeps the budget spent.
	unansweredGrace = 6 * time.Hour
)

// repoLookups caches GitHub's answer for a repo's current owner and
// visibility. Without an answer the repo keeps the owner its remote names
// and an unknown visibility, which is never placed in public.
type repoLookups struct {
	gh  github.Runner
	now func() time.Time

	mu      sync.Mutex
	entries map[string]repoEntry
	// public is when GitHub last said each repo is public, by key; it
	// bounds the grace an unanswered lookup gets.
	public map[string]time.Time
	window time.Time
	spent  int
}

type repoEntry struct {
	repo spaces.Repo
	at   time.Time
}

func newRepoLookups(gh github.Runner) *repoLookups {
	return &repoLookups{gh: gh, now: time.Now, entries: map[string]repoEntry{}, public: map[string]time.Time{}}
}

type cachedRepoLookupsKey struct{}

// withCachedRepoLookups makes repo lookups under ctx answer from the cache
// alone, so placing paths a session chose spends none of GitHub's budget.
func withCachedRepoLookups(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedRepoLookupsKey{}, true)
}

func (l *repoLookups) lookup(ctx context.Context, owner, name string) *spaces.Repo {
	fallback := &spaces.Repo{Owner: owner, Name: name}
	// Invalid names are refused before they spend the lookup budget.
	if l == nil || l.gh == nil || !github.ValidLogin(owner) || !github.ValidRepo(name) {
		return fallback
	}
	key := strings.ToLower(owner + "/" + name)
	l.mu.Lock()
	now := l.now()
	if e, ok := l.entries[key]; ok && now.Sub(e.at) < repoLookupTTL {
		l.mu.Unlock()
		r := e.repo
		return &r
	}
	// Reason (RC-402): only a refusal is GitHub's answer; no answer at
	// all must not take a listed folder's public copy out of public, for
	// a while after GitHub last said the repo is public.
	unchecked := fallback
	if at, ok := l.public[key]; ok && now.Sub(at) < unansweredGrace {
		unchecked = &spaces.Repo{Owner: owner, Name: name, Unchecked: true}
	}
	if ctx.Value(cachedRepoLookupsKey{}) != nil {
		l.mu.Unlock()
		return unchecked
	}
	if now.Sub(l.window) >= time.Minute {
		l.window, l.spent = now, 0
	}
	if l.spent >= repoLookupsPerMin {
		l.mu.Unlock()
		return unchecked
	}
	l.spent++
	l.mu.Unlock()

	info, err := github.LookupRepo(ctx, l.gh, owner, name)
	if err != nil {
		if errors.Is(github.MarkNoAccess(err), github.ErrNoAccess) {
			return fallback
		}
		return unchecked
	}
	r := spaces.Repo{Owner: info.Owner, Name: info.Name, Visibility: spaces.VisibilityPublic}
	if info.Private {
		r.Visibility = spaces.VisibilityPrivate
	}
	l.mu.Lock()
	if len(l.entries) >= repoLookupEntries {
		clear(l.entries)
	}
	l.entries[key] = repoEntry{repo: r, at: now}
	if len(l.public) >= repoLookupEntries {
		clear(l.public) // fails closed: unanswered lookups then leave
	}
	if r.Visibility == spaces.VisibilityPublic {
		l.public[key] = now
	} else {
		delete(l.public, key)
	}
	l.mu.Unlock()
	return &r
}

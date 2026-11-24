package daemon

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/logs"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/reviews"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
)

// repoSpaces are a repo of each GitHub owner readPolicy binds, and one of
// an owner no space binds, by the space its records belong to.
var repoSpaces = map[string]string{
	"acme/api":    "acme",
	"globex/x":    "globex",
	"oss-org/lib": "oss",
	"stranger/z":  spaces.Quarantine,
}

func prURLOf(repo string) string { return "https://github.com/" + repo + "/pull/1" }

// spaceViews are callers and the spaces whose records each may see.
var spaceViews = []struct {
	name string
	ctx  context.Context
	sees []string
}{
	{"a session in acme", inSpace("acme"), []string{"acme", "oss"}},
	{"a session in globex", inSpace("globex"), []string{"globex", "oss"}},
	{"a session in personal", inSpace("personal"), []string{"oss", "personal"}},
	{"a task of globex", taskOf("globex"), []string{"globex", "oss"}},
	{"an unpinned harness", as(none("no pin")), []string{"oss"}},
	{"the operator with no evidence", as(resolvedCaller{kind: callerOperator}), []string{"oss"}},
	{"the operator", appOperatorCtx, []string{"acme", "globex", "oss", spaces.Quarantine}},
}

// reposSeen lists the repos of repoSpaces whose space is in sees.
func reposSeen(sees []string) []string {
	var out []string
	for repo, sp := range repoSpaces {
		if slices.Contains(sees, sp) {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

func sorted(in []string) []string {
	slices.Sort(in)
	return in
}

// policyServer is a server with readPolicy applied.
func policyServer(t *testing.T) *server {
	t.Helper()
	s := spacesServer(t)
	applyReadPolicy(t, s)
	return s
}

// TestCachedPRsKeepSpacesApart is SP-026.1.a1 for ListPRs and
// ListGitHubItems, and SP-026.1.a2's first half: a cached PR's space is its
// repo owner's, matched as GitHub matches logins, and an owner no space
// binds is the operator's alone (quarantine). With no policy every caller
// sees every PR (INV-10).
func TestCachedPRsKeepSpacesApart(t *testing.T) {
	s := policyServer(t)
	s.prStateFile = filepath.Join(t.TempDir(), "prs.json")
	state := prs.State{PRs: map[string]prs.PR{}}
	for repo := range repoSpaces {
		// The owner as GitHub may spell it.
		repo = strings.Replace(repo, "acme", "ACME", 1)
		state.PRs[prURLOf(repo)] = prs.PR{URL: prURLOf(repo), Repo: repo, Kind: prs.KindPR, Title: repo + " title", Reasons: []string{prs.ReasonAuthored}}
	}
	if err := prs.SaveState(s.prStateFile, state); err != nil {
		t.Fatal(err)
	}
	for _, v := range spaceViews {
		t.Run(v.name, func(t *testing.T) {
			list, err := s.ListPRs(v.ctx, &orchdv1.ListPRsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, pr := range list.GetPrs() {
				got = append(got, strings.ToLower(pr.GetRepo()))
			}
			if want := reposSeen(v.sees); !slices.Equal(sorted(got), want) {
				t.Errorf("ListPRs = %v, want %v", got, want)
			}
			items, err := s.ListGitHubItems(v.ctx, &orchdv1.ListGitHubItemsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			got = nil
			for _, it := range items.GetItems() {
				got = append(got, strings.ToLower(it.GetRepo()))
			}
			if want := reposSeen(v.sees); !slices.Equal(sorted(got), want) {
				t.Errorf("ListGitHubItems = %v, want %v", got, want)
			}
		})
	}
	s.spaces = nil
	if list, err := s.ListPRs(inSpace("acme"), &orchdv1.ListPRsRequest{}); err != nil || len(list.GetPrs()) != len(repoSpaces) {
		t.Errorf("with no policy a session sees %d of %d PRs, %v", len(list.GetPrs()), len(repoSpaces), err)
	}
}

// TestPRDetailKeepsSpacesApart is SP-026.1.a1 for GetPRDetail: another
// space's PR is NotFound, and is neither read from the detail cache (keyed
// by URL alone, RC-351) nor fetched into it for that caller.
func TestPRDetailKeepsSpacesApart(t *testing.T) {
	s := policyServer(t)
	calls := 0
	s.details = newDetailCache(func(context.Context, ...string) ([]byte, error) {
		calls++
		return []byte(`{"title":"secret plan","body":"details"}`), nil
	})
	acme := prURLOf("acme/api")
	if d, err := s.GetPRDetail(appOperatorCtx, &orchdv1.GetPRDetailRequest{Url: acme}); err != nil || d.GetTitle() != "secret plan" {
		t.Fatalf("operator GetPRDetail = %v, %v", d, err)
	}
	for _, v := range spaceViews {
		t.Run(v.name, func(t *testing.T) {
			for repo, sp := range repoSpaces {
				before := calls
				d, err := s.GetPRDetail(v.ctx, &orchdv1.GetPRDetailRequest{Url: prURLOf(repo)})
				switch {
				case slices.Contains(v.sees, sp) && err != nil:
					t.Errorf("%s: %v", repo, err)
				case !slices.Contains(v.sees, sp) && (grpcstatus.Code(err) != codes.NotFound || d != nil || calls != before):
					t.Errorf("%s: %v, %v (gh calls %d → %d)", repo, d, err, before, calls)
				}
			}
		})
	}
}

// TestPRReviewsKeepSpacesApart is SP-026.1.a1 for PR reviews: a review,
// its verdict and report are its PR owner's space's.
func TestPRReviewsKeepSpacesApart(t *testing.T) {
	s := policyServer(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := reviews.NewStore(conn)
	for repo := range repoSpaces {
		if err := store.Put(context.Background(), &reviews.Review{URL: prURLOf(repo), Repo: repo, Number: 1, Mode: reviews.ModeVerdict,
			State: reviews.StateClosed, Verdict: "approve", Report: repo + " report"}); err != nil {
			t.Fatal(err)
		}
	}
	s.reviews = &reviews.Watcher{Store: store}
	for _, v := range spaceViews {
		t.Run(v.name, func(t *testing.T) {
			list, err := s.ListPRReviews(v.ctx, &orchdv1.ListPRReviewsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range list.GetReviews() {
				got = append(got, r.GetRepo())
			}
			if want := reposSeen(v.sees); !slices.Equal(sorted(got), want) {
				t.Errorf("ListPRReviews = %v, want %v", got, want)
			}
		})
	}
}

// TestReviewTaskRunsInItsPRsSpace is SP-026.1.a2's second half: a review
// task orchd dispatches records its PR owner's space, so it reads that
// space's memory and its record is that space's; an owner no space binds
// reads public memory only, and with no policy nothing is recorded.
func TestReviewTaskRunsInItsPRsSpace(t *testing.T) {
	s := policyServer(t)
	var recorded string
	w := newReviewWatcher(nil, spaceDispatcher{got: &recorded}, nil, nil, nil, &ghProfiles{spaces: s.spaces})
	for repo, want := range repoSpaces {
		recorded = "unset"
		if _, err := w.Dispatch(context.Background(), repo, "review "+repo, reviews.Checkout{}); err != nil {
			t.Fatal(err)
		}
		if recorded != want {
			t.Errorf("review of %s records space %q, want %q", repo, recorded, want)
		}
	}
	if got := readableFrom(mustResolved(t, s), spaces.Quarantine); !slices.Equal(got, []string{"oss"}) {
		t.Errorf("a review of an unbound owner reads %v, want public only", got)
	}
	// The babysitter stamps each cached item, and so its events, the same way.
	if got := watchedOwnerSpace(&ghProfiles{spaces: s.spaces}, "Globex"); got != "globex" {
		t.Errorf("cached globex item space = %q", got)
	}
	for _, ghp := range []*ghProfiles{nil, {}} {
		w := newReviewWatcher(nil, spaceDispatcher{got: &recorded}, nil, nil, nil, ghp)
		if _, err := w.Dispatch(context.Background(), "acme/api", "x", reviews.Checkout{}); err != nil || recorded != "" {
			t.Errorf("with no policy the review records %q, %v", recorded, err)
		}
	}
}

func mustResolved(t *testing.T, s *server) *spaces.Resolved {
	t.Helper()
	res, err := s.spaces.Current()
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestGitHubRecordsKeepSpacesApart is RC-700 for the GitHub RPCs: an agent
// sees the orgs, teams, scopes and repos of its spaces' owners only.
func TestGitHubRecordsKeepSpacesApart(t *testing.T) {
	s := policyServer(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.github = github.NewService(github.NewStore(conn), func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "user/orgs"):
			return []byte("acme\nglobex\noss-org\n"), nil
		case strings.Contains(joined, "user/teams"):
			return []byte(`{"org":"acme","slug":"a","name":"A"}` + "\n" + `{"org":"globex","slug":"g","name":"G"}`), nil
		case strings.Contains(joined, ".login"):
			return []byte("me\n"), nil
		case strings.Contains(joined, "/repos"):
			return []byte(`{"name":"repo","private":true,"archived":false}`), nil
		}
		return nil, nil
	})
	s.handoffDir = t.TempDir()
	id, err := s.GetGitHubIdentity(inSpace("acme"), &orchdv1.GetGitHubIdentityRequest{})
	if err != nil || !slices.Equal(id.GetOrgs(), []string{"acme", "oss-org"}) || len(id.GetTeams()) != 1 || id.GetTeams()[0].GetOrg() != "acme" {
		t.Errorf("acme's identity = %v, %v", id, err)
	}
	if id, _ := s.GetGitHubIdentity(appOperatorCtx, &orchdv1.GetGitHubIdentityRequest{}); len(id.GetOrgs()) != 3 || len(id.GetTeams()) != 2 {
		t.Errorf("the operator's identity = %v", id)
	}
	scopes, err := s.ListGitHubScopes(inSpace("globex"), &orchdv1.ListGitHubScopesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range scopes.GetScopes() {
		if sc.GetOwner() == "acme" || sc.GetOwner() == "me" {
			t.Errorf("globex sees the scope of %s", sc.GetOwner())
		}
	}
	if _, err := s.ListGitHubRepos(inSpace("globex"), &orchdv1.ListGitHubReposRequest{Owner: "acme"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("globex lists acme's repos: %v", err)
	}
	if _, err := s.ListGitHubRepos(inSpace("acme"), &orchdv1.ListGitHubReposRequest{Owner: "acme"}); err != nil {
		t.Errorf("acme cannot list its repos: %v", err)
	}
}

// fakeSyncProgress reports a docs sync in progress.
type fakeSyncProgress struct{ fakeDocsSync }

// TestDocsAndLogRecordsKeepSpacesApart is RC-700 for the docs owners, the
// docs sync's progress, registered sources and orchd's log.
func TestDocsAndLogRecordsKeepSpacesApart(t *testing.T) {
	s := policyServer(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.docsReg = docs.NewRegistry(conn)
	for _, owner := range []string{"acme", "globex", "oss-org"} {
		if err := s.docsReg.AddOwner(context.Background(), docs.Owner{Owner: owner, RefreshSecs: 3600}); err != nil {
			t.Fatal(err)
		}
	}
	owners, err := s.ListDocsOwners(inSpace("acme"), &orchdv1.ListDocsOwnersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range owners.GetOwners() {
		got = append(got, o.GetOwner())
	}
	if !slices.Equal(sorted(got), []string{"acme", "oss-org"}) {
		t.Errorf("acme's docs owners = %v", got)
	}
	s.docsGH = docs.NewGitHub()
	if _, err := s.ListDocsOwnerRepos(inSpace("acme"), &orchdv1.ListDocsOwnerReposRequest{Owner: "globex"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("acme lists globex's repos: %v", err)
	}
	sync := &fakeSyncProgress{}
	sync.p = docs.Progress{Running: true, Owner: "globex", Repo: "x", Done: 3, Total: 9}
	s.docsSync = sync
	if p, err := s.GetDocsSync(inSpace("acme"), &orchdv1.GetDocsSyncRequest{}); err != nil || p.GetOwner() != "" || p.GetRunning() {
		t.Errorf("acme sees globex's sync: %v, %v", p, err)
	}
	if p, err := s.GetDocsSync(inSpace("globex"), &orchdv1.GetDocsSyncRequest{}); err != nil || p.GetRepo() != "x" {
		t.Errorf("globex cannot see its sync: %v, %v", p, err)
	}

	s.sourcesReg = sources.NewRegistry(conn)
	src := sources.Source{Kind: sources.KindWeb, URI: "https://globex.example/wiki"}
	src.ID = sources.ID(src.Kind, src.URI)
	if err := s.sourcesReg.Add(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.ListRegisteredSources(inSpace("acme"), &orchdv1.ListRegisteredSourcesRequest{}); err != nil || len(rep.GetSources()) != 0 {
		t.Errorf("a session lists registered sources: %v, %v", rep, err)
	}
	if rep, err := s.ListRegisteredSources(appOperatorCtx, &orchdv1.ListRegisteredSourcesRequest{}); err != nil || len(rep.GetSources()) != 1 {
		t.Errorf("the operator's registered sources: %v, %v", rep, err)
	}

	ring := logs.NewRing(10)
	ring.Add("orchd", "note://globex/memo stored")
	s.system = newSystemProbe(ring)
	if _, err := s.TailLogs(inSpace("acme"), &orchdv1.TailLogsRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("a session reads orchd's log: %v", err)
	}
	if rep, err := s.TailLogs(appOperatorCtx, &orchdv1.TailLogsRequest{}); err != nil || len(rep.GetEntries()) != 1 {
		t.Errorf("the operator's log: %v, %v", rep, err)
	}
}

// syncedRag holds one source a sync keeps, in space.
type syncedRag struct {
	fakeRag
	space string
}

func (r syncedRag) Copy(context.Context, string) (rag.StoredCopy, bool, error) {
	return rag.StoredCopy{Writer: rag.WriterDocs, Space: r.space}, true, nil
}

// TestIngestSaysNothingOfAnotherSpacesSync is RC-701 for Ingest: a caller
// that does not read the space holding a synced copy gets the refusal any
// other space's copy gets, not which sync keeps it.
func TestIngestSaysNothingOfAnotherSpacesSync(t *testing.T) {
	s := policyServer(t)
	s.rag = syncedRag{space: "globex"}
	req := &orchdv1.IngestRequest{SourceUri: "repo://globex/x/README.md", Text: "x"}
	_, err := s.Ingest(inSpace("acme"), req)
	if err == nil || err.Error() != errReplaceRefused.Error() {
		t.Errorf("acme's write = %v", err)
	}
	if _, err := s.Ingest(inSpace("globex"), req); err == nil || !strings.Contains(err.Error(), "kept by the docs sync") {
		t.Errorf("globex's write = %v", err)
	}
	s.spaces = nil
	if _, err := s.Ingest(inSpace("acme"), req); err == nil || !strings.Contains(err.Error(), "kept by the docs sync") {
		t.Errorf("with no policy = %v", err)
	}
}

// TestRecordScopeHelpers covers the edges of the record checks.
func TestRecordScopeHelpers(t *testing.T) {
	s := policyServer(t)
	sc := s.recordScope(inSpace("acme"), orchdv1.Orchd_ListPRs_FullMethodName)
	for url, want := range map[string]bool{
		"https://github.com/acme/api/pull/1": true, "https://github.com/globex/x/pull/1": false,
		"https://example.com/acme/api": false, "https://github.com/": false,
	} {
		if sc.seesGitHubURL(url) != want {
			t.Errorf("seesGitHubURL(%q) = %v", url, !want)
		}
	}
	if sc.seesSpace("") || !(readScope{}).seesGitHubURL("x") || callerSpace(as(resolvedCaller{kind: callerTask}).Value(resolvedKey{}).(resolvedCaller)) != "" {
		t.Error("an unplaced record, an unfiltered scope or an untracked task is misjudged")
	}
	if orgSpaceOf(nil, "acme") != "" {
		t.Error("with no policy an owner has a space")
	}
	ts := (&server{}).taskSpaces()
	if ts.of(context.Background(), "task-x") != "" {
		t.Error("a task with no runs has a space")
	}
	if got := (&server{}).tasksFor(context.Background(), readScope{spaces: []string{}}, []queue.Task{{ID: "a"}}); len(got) != 0 {
		t.Errorf("a scope reading nothing kept %v", got)
	}
}

// TestReviewWatchesOnlyFromTheirSpace: a review watch dispatches a task
// that reads its PR owner's space with the caller's instructions and
// returns the review's report, so only that space's callers (and the
// operator) start, change or stop one; anyone else is told the PR does
// not exist.
func TestReviewWatchesOnlyFromTheirSpace(t *testing.T) {
	s, _, _ := reviewsServer(t)
	p := policyServer(t)
	s.spaces, s.enforcement = p.spaces, p.enforcement
	acme := "https://github.com/acme/api/pull/7"
	watch := func(ctx context.Context, url string) error {
		_, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: url, Mode: reviews.ModeVerdict,
			SetInstructions: true, Instructions: "quote your memory"})
		return err
	}
	for _, ctx := range []context.Context{inSpace("globex"), inSpace("oss"), as(none("no pin")), inSpace(spaces.Quarantine), taskOf("globex")} {
		if err := watch(ctx, acme); grpcstatus.Code(err) != codes.NotFound {
			t.Errorf("a caller outside acme watched acme's PR: %v", err)
		}
	}
	if err := watch(inSpace(spaces.Quarantine), "https://github.com/stranger/z/pull/1"); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("a quarantined session watched an unbound owner's PR: %v", err)
	}
	if err := watch(inSpace("acme"), acme); err != nil {
		t.Fatalf("acme cannot watch its own PR: %v", err)
	}
	for name, call := range map[string]func(context.Context) error{
		"SetPRReviewUpdateInPlace": func(ctx context.Context) error {
			_, err := s.SetPRReviewUpdateInPlace(ctx, &orchdv1.SetPRReviewUpdateInPlaceRequest{Url: acme, UpdateInPlace: true})
			return err
		},
		"StopPRReview": func(ctx context.Context) error {
			_, err := s.StopPRReview(ctx, &orchdv1.StopPRReviewRequest{Url: acme})
			return err
		},
	} {
		if err := call(inSpace("globex")); grpcstatus.Code(err) != codes.NotFound || err.Error() != errNoPR.Error() {
			t.Errorf("%s from globex = %v", name, err)
		}
		if err := call(inSpace("acme")); err != nil {
			t.Errorf("%s from acme = %v", name, err)
		}
	}
	if err := watch(appOperatorCtx, "https://github.com/globex/x/pull/2"); err != nil {
		t.Errorf("the operator cannot watch a PR: %v", err)
	}
}

// TestSyncDocsSaysNothingOfAnotherSpace: SyncDocs answers for another
// space's owner as for one not registered, and names neither the owner nor
// the counts of a sync of another space, running or just requested.
func TestSyncDocsSaysNothingOfAnotherSpace(t *testing.T) {
	s := policyServer(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.docsReg = docs.NewRegistry(conn)
	if err := s.docsReg.AddOwner(context.Background(), docs.Owner{Owner: "globex", RefreshSecs: 3600}); err != nil {
		t.Fatal(err)
	}
	sync := &fakeSyncProgress{}
	s.docsSync = sync
	_, err = s.SyncDocs(inSpace("acme"), &orchdv1.SyncDocsRequest{Owner: "globex"})
	_, missing := s.SyncDocs(inSpace("acme"), &orchdv1.SyncDocsRequest{Owner: "initech"})
	if grpcstatus.Code(err) != codes.NotFound || strings.Replace(err.Error(), "globex", "initech", 2) != missing.Error() {
		t.Errorf("acme syncing globex = %v; a missing owner = %v", err, missing)
	}
	sync.p = docs.Progress{Owner: "globex", Repo: "x", Done: 1, Total: 4}
	if p, err := s.SyncDocs(inSpace("acme"), &orchdv1.SyncDocsRequest{}); err != nil || p.GetOwner() != "" || p.GetTotal() != 0 {
		t.Errorf("acme's sync request shows %v, %v", p, err)
	}
	sync.p.Running = true
	_, err = s.SyncDocs(inSpace("acme"), &orchdv1.SyncDocsRequest{})
	if grpcstatus.Code(err) != codes.Aborted || strings.Contains(err.Error(), "globex") || strings.Contains(err.Error(), "of 4") {
		t.Errorf("a busy sync told acme: %v", err)
	}
	if _, err := s.SyncDocs(inSpace("globex"), &orchdv1.SyncDocsRequest{}); err == nil || !strings.Contains(err.Error(), "globex, 1 of 4") {
		t.Errorf("a busy sync told globex: %v", err)
	}
}

// deletingRag records which deletes ran.
type deletingRag struct {
	fakeRag
	all     *[]string
	inSpace *[]string
}

func (r deletingRag) DeleteSource(_ context.Context, uri string) (int, error) {
	*r.all = append(*r.all, uri)
	return 3, nil
}

func (r deletingRag) DeleteSourceIn(_ context.Context, uri, space string) (int, error) {
	*r.inSpace = append(*r.inSpace, space+":"+uri)
	return 1, nil
}

// TestDeleteSourceOnlyInTheSessionsSpace is D-77 (RC-1200, RC-701): under a
// policy a session deletes a source only from its own space's collection
// and is told only that count; a caller with no space of its own, or pinned
// to quarantine, deletes nothing; Orch.app deletes from every space; with no
// policy a delete reaches every space, as before.
func TestDeleteSourceOnlyInTheSessionsSpace(t *testing.T) {
	s := policyServer(t)
	var all, in []string
	s.rag = deletingRag{all: &all, inSpace: &in}
	req := &orchdv1.DeleteSourceRequest{SourceUri: "note://x", Confirm: true}
	if rep, err := s.DeleteSource(inSpace("acme"), req); err != nil || rep.GetDeleted() != 1 {
		t.Fatalf("acme's delete = %v, %v", rep, err)
	}
	for _, ctx := range []context.Context{inSpace(spaces.Quarantine), as(none("no pin")), as(resolvedCaller{kind: callerOperator}), taskOf("acme")} {
		if _, err := s.DeleteSource(ctx, req); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("a caller with no space of its own deleted: %v", err)
		}
	}
	if rep, err := s.DeleteSource(appOperatorCtx, req); err != nil || rep.GetDeleted() != 3 {
		t.Fatalf("Orch.app's delete = %v, %v", rep, err)
	}
	if len(in) != 1 || in[0] != "acme:note://x" || len(all) != 1 {
		t.Errorf("deletes: in a space %v, everywhere %v", in, all)
	}
	s.rag = fakeRag{}
	if _, err := s.DeleteSource(inSpace("acme"), req); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("a store that cannot delete by space = %v", err)
	}
	s.spaces = nil
	s.rag = deletingRag{all: &all, inSpace: &in}
	if rep, err := s.DeleteSource(inSpace("acme"), req); err != nil || rep.GetDeleted() != 3 {
		t.Errorf("with no policy = %v, %v", rep, err)
	}
}

// TestOwnerSettingsOnlyFromTheirSpace: what is synced or monitored for a
// GitHub owner is its space's to change: another space's caller is told
// the owner is not registered (or not the user's), a purge is Orch.app's
// alone under a policy (D-77), and the owner's own space may change it.
func TestOwnerSettingsOnlyFromTheirSpace(t *testing.T) {
	s := policyServer(t)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.docsReg = docs.NewRegistry(conn)
	ctx := context.Background()
	if err := s.docsReg.AddOwner(ctx, docs.Owner{Owner: "globex", RefreshSecs: 3600}); err != nil {
		t.Fatal(err)
	}
	save := &orchdv1.SaveDocsOwnerRequest{Owner: "globex", Enabled: false, RepoMode: docs.ReposAll}
	if _, err := s.SaveDocsOwner(inSpace("acme"), save); grpcstatus.Code(err) != codes.NotFound || err.Error() != notDocsOwner("globex").Error() {
		t.Errorf("acme saved globex's docs choice: %v", err)
	}
	if _, err := s.RemoveDocsOwner(inSpace("acme"), &orchdv1.RemoveDocsOwnerRequest{Owner: "globex"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("acme removed globex's docs owner: %v", err)
	}
	if _, err := s.RemoveDocsOwner(inSpace("globex"), &orchdv1.RemoveDocsOwnerRequest{Owner: "globex", Purge: true, Confirm: true}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("a session purged its docs: %v", err)
	}
	if _, err := s.SaveDocsOwner(inSpace("globex"), save); err != nil {
		t.Errorf("globex cannot save its docs choice: %v", err)
	}
	if _, err := s.RemoveDocsOwner(inSpace("globex"), &orchdv1.RemoveDocsOwnerRequest{Owner: "globex"}); err != nil {
		t.Errorf("globex cannot remove its docs owner: %v", err)
	}
	s.github = github.NewService(github.NewStore(conn), func(context.Context, ...string) ([]byte, error) { return nil, nil })
	_, err = s.SetGitHubScope(inSpace("acme"), &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{Owner: "globex", Enabled: true}})
	if grpcstatus.Code(err) != codes.NotFound || !strings.Contains(err.Error(), "is not your account") {
		t.Errorf("acme changed globex's monitoring: %v", err)
	}
}

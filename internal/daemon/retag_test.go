package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
)

// retagEnv is the plan's example policy (oss public, personal private, acme
// and globex isolated) over a memQdrant, with a server that can retag.
func retagEnv(t *testing.T) writesEnv {
	t.Helper()
	e := newWritesEnv(t, orgNodes{})
	e.s.migrator = e.svc
	return e
}

// retagAs asks orchd, as the operator, to move uri to space.
func (e writesEnv) retagAs(t *testing.T, uri, space string, confirm bool) *orchdv1.RetagSourceResponse {
	t.Helper()
	resp, err := e.s.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: uri, Space: space, Confirm: confirm})
	if err != nil {
		t.Fatalf("retag %s to %s: %v", uri, space, err)
	}
	return resp
}

// ingestAs writes a client note as ctx's caller.
func (e writesEnv) ingestAs(ctx context.Context, uri, text string) error {
	_, err := e.s.Ingest(ctx, &orchdv1.IngestRequest{SourceUri: uri, Text: text})
	return err
}

// TestRetaggedSourceStaysForLaterWrites: SP-071.2.a1. After the operator
// retags a source, a later docs sync, sources refresh and client re-ingest
// of the same source_uri land in the retagged space.
func TestRetaggedSourceStaysForLaterWrites(t *testing.T) {
	ctx := context.Background()

	t.Run("docs sync", func(t *testing.T) {
		e := retagEnv(t)
		reg := docs.NewRegistry(mustDB(t))
		sync := func(sha string) {
			t.Helper()
			res := docs.SyncOwner(ctx, reg, docs.Owner{Owner: "acme"}, e.svc, docs.SyncOpts{
				RepoCache: t.TempDir(), GH: docsGH{{Name: "app", HeadSHA: sha, Private: true, OwnerNodeID: "O_acme"}},
				Git: &docsGit{}, CheckOwner: docsOwnerCheck(e.s.spaces),
			})
			if res.Status != "ok" || res.Synced != 1 {
				t.Fatalf("sync = %+v", res)
			}
		}
		const uri = "repo://acme/app/README.md"
		sync("h1")
		e.placedAs(t, uri, "acme", rag.OriginGitHub)
		e.retagAs(t, uri, "personal", true)
		e.placedAs(t, uri, "personal", rag.OriginGitHub)
		sync("h2")
		e.placedAs(t, uri, "personal", rag.OriginGitHub)
		if n := len(e.q.storedIn("c__acme", uri)); n != 0 {
			t.Fatalf("the sync put %d chunk(s) back in acme", n)
		}
	})

	t.Run("sources refresh", func(t *testing.T) {
		e := retagEnv(t)
		repo := filepath.Join(e.code, "company1", "app")
		defaultCheckout(t, repo, "acme", "app")
		refresh := func(body string) {
			t.Helper()
			writeFile(t, filepath.Join(repo, "README.md"), body)
			if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: repo, Owner: "acme"}, e.svc,
				sources.RefreshOpts{CheckOwner: docsOwnerCheck(e.s.spaces)}); err != nil || st != "ok" {
				t.Fatalf("refresh = %q, %v", st, err)
			}
		}
		uri := sources.CheckoutURI(filepath.Join(repo, "README.md"))
		refresh("# app")
		e.placedAs(t, uri, "acme", rag.OriginFS)
		e.retagAs(t, uri, "globex", true)
		refresh("# app, changed")
		e.placedAs(t, uri, "globex", rag.OriginFS)
		if n := len(e.q.storedIn("c__acme", uri)); n != 0 {
			t.Fatalf("the refresh put %d chunk(s) back in acme", n)
		}
		// A web page under no bound prefix is quarantined; retagged, its
		// refreshes go to the space the operator chose.
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a page")) }))
		defer page.Close()
		web := sources.Source{Kind: sources.KindWeb, URI: page.URL}
		if st, err := sources.Refresh(ctx, web, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
			t.Fatalf("web refresh = %q, %v", st, err)
		}
		e.placedAs(t, page.URL, spaces.Quarantine, rag.OriginWeb)
		// Out of quarantine needs confirmation: content of unknown origin.
		if _, err := e.s.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: page.URL, Space: "personal"}); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Fatalf("an unconfirmed retag out of quarantine: %v", err)
		}
		e.retagAs(t, page.URL, "personal", true)
		if st, err := sources.Refresh(ctx, web, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
			t.Fatalf("web refresh = %q, %v", st, err)
		}
		e.placedAs(t, page.URL, "personal", rag.OriginWeb)
	})

	t.Run("client re-ingest", func(t *testing.T) {
		e := retagEnv(t)
		operator := context.WithValue(ctx, resolvedKey{}, resolvedCaller{kind: callerOperator})
		// The operator's note is quarantined (no writer space) until retagged.
		if err := e.ingestAs(operator, "manual://note", "the operator's note"); err != nil {
			t.Fatal(err)
		}
		e.placedAs(t, "manual://note", spaces.Quarantine, rag.OriginClient)
		e.retagAs(t, "manual://note", "personal", true)
		if err := e.ingestAs(operator, "manual://note", "the operator's note, edited"); err != nil {
			t.Fatal(err)
		}
		e.placedAs(t, "manual://note", "personal", rag.OriginClient)
		// A session of the target space writes there too.
		personal := sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev})
		if err := e.ingestAs(personal, "manual://note", "edited in a personal session"); err != nil {
			t.Fatal(err)
		}
		e.placedAs(t, "manual://note", "personal", rag.OriginClient)
	})
}

// TestRetagDoesNotLetAnotherSpaceWriteIn: RC-850 option (a). A session of
// another space re-ingesting a retagged URI is refused, and the retagged
// copy is left as it was.
func TestRetagDoesNotLetAnotherSpaceWriteIn(t *testing.T) {
	ctx := context.Background()
	e := retagEnv(t)
	acme := sessionCtx(ctx, sessionPin{SessionID: "s-a", Space: "acme", Revision: e.rev})
	if err := e.ingestAs(acme, "manual://plan", "acme's plan"); err != nil {
		t.Fatal(err)
	}
	e.retagAs(t, "manual://plan", "globex", true)
	for name, c := range map[string]context.Context{
		"the space it came from": acme,
		"another space":          sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}),
		"unidentified":           ctx,
	} {
		if err := e.ingestAs(c, "manual://plan", "overwritten"); grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s wrote into the retagged source: %v", name, err)
		}
	}
	e.placedAs(t, "manual://plan", "globex", rag.OriginClient)
	globex := sessionCtx(ctx, sessionPin{SessionID: "s-g", Space: "globex", Revision: e.rev})
	if err := e.ingestAs(globex, "manual://plan", "globex's edit"); err != nil {
		t.Fatalf("the target space could not write its source: %v", err)
	}
	// A relabel's placer keeps a retagged source stored where the retag put
	// it, whoever wrote it; a note of the same URI stored elsewhere (written
	// after the retagged one was deleted) follows the rules a write does.
	w := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos, relabel: true}
	for stored, want := range map[string]string{"globex": "globex", "acme": "acme"} {
		m := rag.Meta{SourceURI: "manual://plan", Writer: rag.WriterClient, Space: stored,
			Origin: rag.Origin{System: rag.OriginClient, Caller: string(callerSession), WriterSpace: "acme"}}
		if err := w.place(ctx, &m); err != nil || m.Space != want {
			t.Fatalf("relabel placed an acme note stored in %s in %q, %v; want %s", stored, m.Space, err, want)
		}
	}
}

// TestRetagIsNotBorrowedByUnplacedSessionOrFileWrites: a session record or
// an edited file's hand-off that is placed in quarantine (no pin, a path
// under no root) stays there even when its URI was retagged: a session ID or
// a path any process can name must not borrow the retag's space. Placed
// ones follow the retag.
func TestRetagIsNotBorrowedByUnplacedSessionOrFileWrites(t *testing.T) {
	ctx := context.Background()
	e := retagEnv(t)
	for uri, space := range map[string]string{"session://s-x/transcript": "acme", "file:///elsewhere/notes.md": "acme",
		"file://" + filepath.Join(e.code, "willesq", "notes.md"): "globex"} {
		if err := e.s.spaces.SaveRetag(ctx, uri, space, nil, true, "op"); err != nil {
			t.Fatal(err)
		}
	}
	// An unrooted file beside a .git naming acme's org: placed only by a
	// remote any agent can write, so it stays in quarantine.
	steered, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defaultCheckout(t, steered, "acme", "app")
	steeredDoc := filepath.Join(steered, "NOTES.md")
	writeFile(t, steeredDoc, "# notes")
	if err := e.s.spaces.SaveRetag(ctx, "file://"+steeredDoc, "acme", nil, true, "op"); err != nil {
		t.Fatal(err)
	}
	w := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}
	for _, c := range []struct {
		m    rag.Meta
		want string
	}{
		{rag.Meta{SourceURI: "session://s-x/transcript", Writer: rag.WriterSession, Origin: rag.Origin{System: rag.OriginSession, Session: "s-x"}}, spaces.Quarantine},
		{rag.Meta{SourceURI: "file:///elsewhere/notes.md", Writer: rag.WriterFile, Origin: rag.Origin{System: rag.OriginFS, Path: "/elsewhere/notes.md"}}, spaces.Quarantine},
		{rag.Meta{SourceURI: "file://" + filepath.Join(e.code, "willesq", "notes.md"), Writer: rag.WriterFile,
			Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(e.code, "willesq", "notes.md")}}, "globex"},
		{rag.Meta{SourceURI: "file://" + steeredDoc, Writer: rag.WriterFile, Origin: rag.Origin{System: rag.OriginFS, Path: steeredDoc}}, spaces.Quarantine},
	} {
		m := c.m
		if err := w.place(ctx, &m); err != nil || m.Space != c.want {
			t.Fatalf("%s placed in %q, %v; want %s", c.m.SourceURI, m.Space, err, c.want)
		}
	}
}

// TestRetagSourceRefusals: SP-071.2.a2. A retag is refused for agent
// callers; a retag into public, or out of an isolated space, without
// confirmation is refused; refusals change nothing and record no override.
func TestRetagSourceRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("agent callers", func(t *testing.T) {
		e := retagEnv(t)
		if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}), "manual://note", "a note"); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]context.Context{
			"harness session":  callerCtx(callerAuth{pid: 950, harness: "claude"}),
			"queued task":      taskCtx("t-1"),
			"started by orchd": callerCtx(callerAuth{pid: 79, spawned: true}),
			"unidentified":     ctx,
		} {
			_, err := e.s.RetagSource(c, &orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "globex", Confirm: true})
			if grpcstatus.Code(err) != codes.PermissionDenied {
				t.Fatalf("%s: %v", name, err)
			}
		}
		e.placedAs(t, "manual://note", "personal", rag.OriginClient)
		if over, _ := e.s.spaces.RetagOf(ctx, "manual://note"); over != "" {
			t.Fatalf("a refused retag recorded %q", over)
		}
	})
	t.Run("into public without confirm", func(t *testing.T) {
		e := retagEnv(t)
		if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}), "manual://note", "a note"); err != nil {
			t.Fatal(err)
		}
		_, err := e.s.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "oss"})
		if grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Fatalf("an unconfirmed retag into public: %v", err)
		}
		e.placedAs(t, "manual://note", "personal", rag.OriginClient)
		if over, _ := e.s.spaces.RetagOf(ctx, "manual://note"); over != "" {
			t.Fatalf("a refused retag recorded %q", over)
		}
		e.retagAs(t, "manual://note", "oss", true)
		e.placedAs(t, "manual://note", "oss", rag.OriginClient)
	})
	t.Run("out of isolated without confirm", func(t *testing.T) {
		e := retagEnv(t)
		if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-a", Space: "acme", Revision: e.rev}), "manual://plan", "acme's plan"); err != nil {
			t.Fatal(err)
		}
		for _, to := range []string{"personal", spaces.Quarantine, "globex"} {
			_, err := e.s.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: "manual://plan", Space: to})
			if grpcstatus.Code(err) != codes.FailedPrecondition {
				t.Fatalf("an unconfirmed retag out of acme to %s: %v", to, err)
			}
		}
		e.placedAs(t, "manual://plan", "acme", rag.OriginClient)
		// Staying in its isolated space needs nothing to confirm.
		if resp := e.retagAs(t, "manual://plan", "acme", false); resp.GetMoved() {
			t.Fatalf("a retag in place moved: %v", resp)
		}
	})
}

// TestRetagSourceRequestChecks covers the other refusals and expect_from.
func TestRetagSourceRequestChecks(t *testing.T) {
	ctx := context.Background()
	e := retagEnv(t)
	if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}), "manual://note", "a note"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		req  *orchdv1.RetagSourceRequest
		code codes.Code
	}{
		"no source":     {&orchdv1.RetagSourceRequest{Space: "personal"}, codes.InvalidArgument},
		"long source":   {&orchdv1.RetagSourceRequest{SourceUri: string(make([]byte, maxRetagURI+1)), Space: "personal"}, codes.InvalidArgument},
		"unknown space": {&orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "initech"}, codes.InvalidArgument},
		"not stored":    {&orchdv1.RetagSourceRequest{SourceUri: "manual://none", Space: "personal"}, codes.NotFound},
		"not from":      {&orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "personal", ExpectFrom: spaces.Quarantine}, codes.FailedPrecondition},
	} {
		if _, err := e.s.RetagSource(operatorCtx, c.req); grpcstatus.Code(err) != c.code {
			t.Fatalf("%s: %v, want %s", name, err, c.code)
		}
	}
	if over, _ := e.s.spaces.RetagOf(ctx, "manual://note"); over != "" {
		t.Fatalf("a refused retag recorded %q", over)
	}
	resp, err := e.s.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "quarantine", ExpectFrom: "personal"})
	if err != nil || !resp.GetMoved() || !slices.Equal(resp.GetFrom(), []string{"personal"}) || resp.GetChunks() == 0 {
		t.Fatalf("expect_from retag = %v, %v", resp, err)
	}
	e.placedAs(t, "manual://note", spaces.Quarantine, rag.OriginClient)

	migrating.Lock()
	_, err = e.s.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "personal"})
	migrating.Unlock()
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("beside a migration: %v", err)
	}
	if _, err := (&server{}).RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "personal"}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unavailable: %v", err)
	}
	noPolicy := spacesServerWith(t, orgNodes{})
	noPolicy.migrator = e.svc
	if _, err := noPolicy.RetagSource(operatorCtx, &orchdv1.RetagSourceRequest{SourceUri: "manual://note", Space: "personal"}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no policy: %v", err)
	}
}

// TestRetagResumesAtStart: a retag recorded but not finished (orchd exited
// mid-move) is finished at the next start; a retag whose source is gone is
// closed.
func TestRetagResumesAtStart(t *testing.T) {
	ctx := context.Background()
	e := retagEnv(t)
	if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}), "manual://note", "a note"); err != nil {
		t.Fatal(err)
	}
	for uri, space := range map[string]string{"manual://note": "globex", "manual://gone": "acme"} {
		if err := e.s.spaces.SaveRetag(ctx, uri, space, []string{"personal"}, false, "operator (process 42)"); err != nil {
			t.Fatal(err)
		}
	}
	migrating.Lock()
	busy := e.s.resumePendingRetags(ctx)
	migrating.Unlock()
	if busy {
		t.Fatal("resume ran beside a migration")
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { e.s.resumeRetags(cctx, 0); close(done) }()
	<-done
	e.placedAs(t, "manual://note", "globex", rag.OriginClient)
	if pending, err := e.s.spaces.PendingRetags(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after resume = %v, %v", pending, err)
	}
	if !(&server{}).resumePendingRetags(ctx) {
		t.Fatal("a server with no store did not finish")
	}
	cancel()
	(&server{}).resumeRetags(cctx, 0)
}

// TestRetagResumeFollowsTheCurrentPolicy: an interrupted retag to a space
// the policy no longer has sends the source to quarantine; one that now
// needs confirmation the operator did not give is left pending.
func TestRetagResumeFollowsTheCurrentPolicy(t *testing.T) {
	ctx := context.Background()
	e := retagEnv(t)
	if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}), "manual://gone-space", "a note"); err != nil {
		t.Fatal(err)
	}
	if err := e.ingestAs(sessionCtx(ctx, sessionPin{SessionID: "s-a", Space: "acme", Revision: e.rev}), "manual://acme", "acme's note"); err != nil {
		t.Fatal(err)
	}
	for uri, space := range map[string]string{"manual://gone-space": "initech", "manual://acme": "personal"} {
		if err := e.s.spaces.SaveRetag(ctx, uri, space, nil, false, "op"); err != nil {
			t.Fatal(err)
		}
	}
	if !e.s.resumePendingRetags(ctx) {
		t.Fatal("resume did not finish")
	}
	e.placedAs(t, "manual://gone-space", spaces.Quarantine, rag.OriginClient)
	e.placedAs(t, "manual://acme", "acme", rag.OriginClient)
	pending, err := e.s.spaces.PendingRetags(ctx)
	if err != nil || len(pending) != 1 || pending[0].SourceURI != "manual://acme" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
)

// writesEnv is the plan's example policy (placementServer) with a real
// rag.Service whose writes the server's placer places.
type writesEnv struct {
	s    *server
	code string
	svc  *rag.Service
	q    *memQdrant
	rev  int64
}

func newWritesEnv(t *testing.T, nodes orgNodes) writesEnv {
	t.Helper()
	s, code, _ := placementServerWith(t, nodes)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.pins = newPinRegistry(conn)
	fakeTranscriptRoots(t)
	svc, q := placedRAG(t, &writePlacer{spaces: s.spaces, pins: s.pins, repos: s.repos})
	s.rag = svc
	_, rev, err := s.spaces.CurrentRevision()
	if err != nil || rev == 0 {
		t.Fatalf("applied revision = %d, %v", rev, err)
	}
	return writesEnv{s: s, code: code, svc: svc, q: q, rev: rev}
}

// placedAs asserts every chunk of uri is in space under the applied
// revision, with an origin of system, and returns that origin.
func (e writesEnv) placedAs(t *testing.T, uri, space, system string) map[string]any {
	t.Helper()
	pts := e.q.stored(uri)
	if len(pts) == 0 {
		t.Fatalf("%s: nothing stored (have %v)", uri, e.q.uris())
	}
	var origin map[string]any
	for _, p := range pts {
		if p["space"] != space {
			t.Fatalf("%s: space = %v, want %s", uri, p["space"], space)
		}
		if v, _ := p["labels_v"].(float64); int64(v) != e.rev {
			t.Fatalf("%s: labels_v = %v, want %d", uri, p["labels_v"], e.rev)
		}
		origin, _ = p["origin"].(map[string]any)
		if origin["system"] != system {
			t.Fatalf("%s: origin = %v, want system %s", uri, origin, system)
		}
	}
	return origin
}

// defaultCheckout makes dir a checkout of github.com/owner/name on its
// default branch, main.
func defaultCheckout(t *testing.T, dir, owner, name string) {
	t.Helper()
	oid := strings.Repeat("ab", 20) + "\n"
	for rel, body := range map[string]string{
		"HEAD":                     "ref: refs/heads/main\n",
		"refs/heads/main":          oid,
		"refs/remotes/origin/main": oid,
		"refs/remotes/origin/HEAD": "ref: refs/remotes/origin/main\n",
		"config": "[remote \"origin\"]\n\turl = git@github.com:" + owner + "/" + name + ".git\n" +
			"[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n",
	} {
		writeFile(t, filepath.Join(dir, ".git", rel), body)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// docsGH lists fixed repos; docsGit writes one README per repo.
type docsGH []docs.Repo

func (g docsGH) ListRepos(context.Context, string) ([]docs.Repo, error) { return g, nil }

type docsGit struct{ synced []string }

func (g *docsGit) Sync(_ context.Context, _, repo, dest string) error {
	g.synced = append(g.synced, repo)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "README.md"), []byte("# "+repo+" docs"), 0o644)
}

// fakeTranscriptRoots moves the harness transcript folders into a temporary
// home for the test.
func fakeTranscriptRoots(t *testing.T) (claude, codex string) {
	t.Helper()
	home := t.TempDir()
	claude, codex = filepath.Join(home, ".claude", "projects"), filepath.Join(home, ".codex", "sessions")
	prev := transcriptRoots
	transcriptRoots = func() (string, string) { return claude, codex }
	t.Cleanup(func() { transcriptRoots = prev })
	return claude, codex
}

// sessionTranscript writes a one-turn Claude Code transcript of session id
// where Claude Code keeps it.
func sessionTranscript(t *testing.T, id string) string {
	t.Helper()
	claude, _ := transcriptRoots()
	tr := filepath.Join(claude, "-Users-u-code", id+".jsonl")
	writeFile(t, tr, `{"type":"user","timestamp":"2026-11-12T09:00:00Z","message":{"role":"user","content":"plan the release"}}`+"\n")
	return tr
}

// TestWritersStampOriginSpaceAndLabels is SP-023.1.a1: every IngestText
// writer stores chunks carrying its origin, the space orchd placed them in
// and the applied revision (labels_v). The sixth writer, the RAG store's own
// migration copy, is internal/rag TestMigrationKeepsPlacement.
func TestWritersStampOriginSpaceAndLabels(t *testing.T) {
	ctx := context.Background()

	t.Run("docs sync", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		reg := docs.NewRegistry(mustDB(t))
		res := docs.SyncOwner(ctx, reg, docs.Owner{Owner: "acme"}, e.svc, docs.SyncOpts{
			RepoCache: t.TempDir(), GH: docsGH{{Name: "app", HeadSHA: "h1", Private: true, OwnerNodeID: "O_acme"}},
			Git: &docsGit{}, CheckOwner: docsOwnerCheck(e.s.spaces),
		})
		if res.Status != "ok" || res.Synced != 1 {
			t.Fatalf("sync = %+v", res)
		}
		o := e.placedAs(t, "repo://acme/app/README.md", "acme", rag.OriginGitHub)
		if o["repo_owner"] != "acme" || o["repo_name"] != "app" || o["visibility"] != "private" || o["owner_node_id"] != "O_acme" {
			t.Fatalf("origin = %v", o)
		}
	})

	t.Run("sources refresh", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		repo := filepath.Join(e.code, "company1", "app")
		defaultCheckout(t, repo, "acme", "app")
		writeFile(t, filepath.Join(repo, "README.md"), "# app")
		if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: repo, Owner: "acme"}, e.svc,
			sources.RefreshOpts{CheckOwner: docsOwnerCheck(e.s.spaces)}); err != nil || st != "ok" {
			t.Fatalf("refresh = %q, %v", st, err)
		}
		o := e.placedAs(t, sources.CheckoutURI(filepath.Join(repo, "README.md")), "acme", rag.OriginFS)
		if o["path"] != filepath.Join(repo, "README.md") {
			t.Fatalf("origin = %v", o)
		}
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a page")) }))
		defer page.Close()
		if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindWeb, URI: page.URL}, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
			t.Fatalf("web refresh = %q, %v", st, err)
		}
		// Under no bound prefix and ingested by no session: quarantine.
		if o := e.placedAs(t, page.URL, spaces.Quarantine, rag.OriginWeb); o["url"] != page.URL {
			t.Fatalf("web origin = %v", o)
		}
	})

	t.Run("session hand-off", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		pin := sessionPin{SessionID: "s-acme", Harness: "claude", Process: procstat.Identity{PID: 7, Started: time.Unix(1, 0)},
			FirstCwd: filepath.Join(e.code, "company1", "app"), Space: "acme", Revision: e.rev, PinnedAt: time.Now().UTC()}
		if err := e.s.pins.insert(ctx, pin); err != nil {
			t.Fatal(err)
		}
		if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s-acme", Summary: "x", Cwd: e.code}); err != nil {
			t.Fatal(err)
		}
		if err := storeSession(ctx, e.svc, handoff.Session{ID: "s-acme", TranscriptPath: sessionTranscript(t, "s-acme"), Record: true}); err != nil {
			t.Fatal(err)
		}
		// The pin decides, not the record's directory (a public root).
		for _, uri := range []string{"session://s-acme/transcript", "session://s-acme/record"} {
			if o := e.placedAs(t, uri, "acme", rag.OriginSession); o["session"] != "s-acme" {
				t.Fatalf("%s origin = %v", uri, o)
			}
		}
	})

	t.Run("file hand-off", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		repo := filepath.Join(e.code, "company1", "app")
		checkout(t, repo, "acme", "app")
		doc := filepath.Join(repo, "NOTES.md")
		writeFile(t, doc, "# notes")
		if err := storeFile(ctx, e.svc, handoff.File{Path: doc}, "", nil); err != nil {
			t.Fatal(err)
		}
		if o := e.placedAs(t, "file://"+doc, "acme", rag.OriginFS); o["path"] != doc {
			t.Fatalf("origin = %v", o)
		}
	})

	t.Run("summaries", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		dir := filepath.Join(e.code, "willesq")
		if err := e.s.pins.insert(ctx, sessionPin{SessionID: "s-sum", Harness: "codex", Process: procstat.Identity{PID: 8, Started: time.Unix(1, 0)},
			FirstCwd: dir, Space: "personal", Revision: e.rev, PinnedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s-sum", Cwd: dir}); err != nil {
			t.Fatal(err)
		}
		// Codex names its transcript rollout-<time>-<id>.jsonl.
		_, codex := transcriptRoots()
		tr := filepath.Join(codex, "2026", "11", "12", "rollout-2026-11-12T09-00-00-s-sum.jsonl")
		writeFile(t, tr, `{"type":"user","message":{"role":"user","content":"plan"}}`+"\n")
		summarizeOne(ctx, fakeSummarizer{summary: "the summary"}, e.svc, summaryJob{SessionID: "s-sum", TranscriptPath: tr}, time.Minute)
		if o := e.placedAs(t, "session://s-sum/record", "personal", rag.OriginSession); o["path"] != dir || o["session"] != "s-sum" {
			t.Fatalf("origin = %v", o)
		}
	})

	t.Run("client ingest", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		pin := sessionPin{SessionID: "s-p", Space: "personal", Revision: e.rev}
		if _, err := e.s.Ingest(sessionCtx(ctx, pin), &orchdv1.IngestRequest{SourceUri: "manual://note", Text: "a note"}); err != nil {
			t.Fatal(err)
		}
		o := e.placedAs(t, "manual://note", "personal", rag.OriginClient)
		if o["caller"] != string(callerSession) || o["writer_space"] != "personal" {
			t.Fatalf("origin = %v", o)
		}
	})
}

func mustDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestClientCannotChooseItsSpace is SP-023.1.a3: a space claimed in a tag,
// scope or type is ignored. A session's note lands in its pinned space;
// any other caller's, or a session pinned to a space the policy lacks, in
// quarantine.
func TestClientCannotChooseItsSpace(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	ctx := context.Background()
	claim := func(uri string) *orchdv1.IngestRequest {
		return &orchdv1.IngestRequest{SourceUri: uri, Text: "claims globex", Scope: "globex", SourceType: "space:globex",
			Tags: []string{"space:globex", "space=globex"}}
	}
	cases := []struct {
		name, uri, space string
		ctx              context.Context
	}{
		{"pinned session", "manual://pinned", "acme", sessionCtx(ctx, sessionPin{SessionID: "a", Space: "acme"})},
		{"session pinned with no space", "manual://nospace", spaces.Quarantine, sessionCtx(ctx, sessionPin{SessionID: "b"})},
		{"session pinned to an unknown space", "manual://gone", spaces.Quarantine, sessionCtx(ctx, sessionPin{SessionID: "c", Space: "dropped"})},
		{"session pinned to public", "manual://public", spaces.Quarantine, sessionCtx(ctx, sessionPin{SessionID: "d", Space: "oss"})},
		{"queued task", "manual://task", spaces.Quarantine, context.WithValue(ctx, resolvedKey{}, resolvedCaller{kind: callerTask})},
		{"operator", "manual://operator", spaces.Quarantine, context.WithValue(ctx, resolvedKey{}, resolvedCaller{kind: callerOperator})},
		{"unidentified", "https://docs.oss.dev/page", spaces.Quarantine, ctx},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := e.s.Ingest(c.ctx, claim(c.uri)); err != nil {
				t.Fatal(err)
			}
			e.placedAs(t, c.uri, c.space, rag.OriginClient)
		})
	}
	// A page under a public prefix, stored by a client, is still the
	// client's: placed by its writer, never by the URL it names.
	if o := e.placedAs(t, "https://docs.oss.dev/page", spaces.Quarantine, rag.OriginClient); o["url"] != "https://docs.oss.dev/page" {
		t.Fatalf("origin = %v", o)
	}
}

// TestPlacementOutcomes covers the placer's other answers: conflicts are not
// indexed, a broken policy quarantines, no policy shares.
func TestPlacementOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	dir := filepath.Join(e.code, "company1", "x") // acme root, globex remote: both isolated
	checkout(t, dir, "globex", "x")
	m := rag.Meta{SourceURI: "file://" + dir + "/a.md", Origin: rag.Origin{System: rag.OriginFS, Path: dir + "/a.md"}}
	if _, err := e.svc.IngestText(ctx, "conflicting", m); !errors.Is(err, errWriteConflict) {
		t.Fatalf("conflict = %v", err)
	}
	if len(e.q.stored(m.SourceURI)) != 0 {
		t.Fatal("a conflicting write was indexed")
	}
	p := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}
	for _, c := range []struct {
		o     rag.Origin
		space string
	}{
		{rag.Origin{System: "bogus"}, spaces.Quarantine},
		{rag.Origin{System: rag.OriginFS}, spaces.Quarantine},
		{rag.Origin{System: rag.OriginSession, Session: "unpinned"}, spaces.Quarantine},
		// Only a pin places a session's records: a directory is a file's word.
		{rag.Origin{System: rag.OriginSession, Path: filepath.Join(e.code, "willesq")}, spaces.Quarantine},
		{rag.Origin{System: rag.OriginWeb, URL: "https://docs.oss.dev/guide"}, "oss"},
		{rag.Origin{System: rag.OriginGitHub, RepoOwner: "oss-org", RepoName: "lib", Visibility: "public"}, "oss"},
		{rag.Origin{System: rag.OriginGitHub, RepoOwner: "oss-org", RepoName: "secret"}, spaces.Quarantine},
	} {
		// The writers that may place content in public: the sources refresh
		// for pages, the docs sync for GitHub (SP-030).
		m := rag.Meta{SourceURI: "x", Origin: c.o, Writer: map[string]string{rag.OriginWeb: rag.WriterSources, rag.OriginGitHub: rag.WriterDocs}[c.o.System]}
		if err := p.place(ctx, &m); err != nil || m.Space != c.space || m.LabelsV != e.rev {
			t.Errorf("%+v placed in %q (labels_v %d), %v; want %s", c.o, m.Space, m.LabelsV, err, c.space)
		}
	}

	// No policy applied: everything is shared, as before spaces (INV-10).
	none := spacesServer(t)
	m = rag.Meta{Origin: rag.Origin{System: rag.OriginClient}}
	if err := (&writePlacer{spaces: none.spaces}).place(ctx, &m); err != nil || m.Space != spaces.Shared || m.LabelsV != 0 {
		t.Fatalf("no policy = %q %d, %v", m.Space, m.LabelsV, err)
	}
	if err := (&writePlacer{}).place(ctx, &m); err != nil || m.Space != spaces.Shared {
		t.Fatalf("no spaces engine = %q, %v", m.Space, err)
	}
}

// TestBrokenPolicyQuarantinesWrites: an applied revision that no longer
// verifies places nothing anywhere but quarantine.
func TestBrokenPolicyQuarantinesWrites(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	conn := mustDB(t)
	if _, err := conn.Exec(`UPDATE spaces_revisions SET doc = '{"spaces":[]}'`); err != nil {
		t.Fatal(err)
	}
	broken, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	m := rag.Meta{Origin: rag.Origin{System: rag.OriginClient, WriterSpace: "acme"}}
	if err := (&writePlacer{spaces: broken}).place(context.Background(), &m); err != nil || m.Space != spaces.Quarantine {
		t.Fatalf("broken policy = %q, %v", m.Space, err)
	}
	if _, err := e.svc.IngestText(context.Background(), "x", rag.Meta{SourceURI: "manual://x"}); err == nil {
		t.Fatal("a write with no origin was stored")
	}
}

// TestIdentityChangeStopsWrites is SP-023.1.a4 end to end: a docs sync of
// several repos re-checks the org between repos, and once the org's identity
// changes nothing more of it is written; sources skip it too.
func TestIdentityChangeStopsWrites(t *testing.T) {
	ctx := context.Background()
	nodes := orgNodes{"acme": "O_acme"}
	e := newWritesEnv(t, nodes)
	git := &docsGit{}
	check := docsOwnerCheck(e.s.spaces)
	calls := 0
	res := docs.SyncOwner(ctx, docs.NewRegistry(mustDB(t)), docs.Owner{Owner: "acme"}, e.svc, docs.SyncOpts{
		RepoCache: t.TempDir(), Git: git,
		GH: docsGH{{Name: "one", HeadSHA: "h1", Private: true}, {Name: "two", HeadSHA: "h2", Private: true}},
		CheckOwner: func(ctx context.Context, owner string) error {
			calls++
			if calls == 2 {
				nodes["acme"] = "O_impostor" // the org changed hands mid-sync
			}
			return check(ctx, owner)
		},
	})
	if res.Status != "error" || res.Synced != 1 || res.NotTried != 1 || !strings.Contains(res.Reason, "not confirmed") {
		t.Fatalf("sync = %+v", res)
	}
	if len(e.q.stored("repo://acme/two/README.md")) != 0 || len(e.q.stored("repo://acme/one/README.md")) == 0 {
		t.Fatalf("stored = %v", e.q.uris())
	}
	repo := filepath.Join(e.code, "company1", "app")
	defaultCheckout(t, repo, "acme", "app")
	writeFile(t, filepath.Join(repo, "README.md"), "# app")
	st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: repo}, e.svc, sources.RefreshOpts{CheckOwner: check})
	if err != nil || !strings.HasPrefix(st, "skipped: not indexed") {
		t.Fatalf("refresh = %q, %v", st, err)
	}
	if len(e.q.stored(sources.CheckoutURI(filepath.Join(repo, "README.md")))) != 0 {
		t.Fatal("sources wrote a changed org's docs")
	}
}

// TestSessionWritesNeedTheirOwnPinAndTranscript: a hand-off entry or summary
// request names a session and a transcript, and any process of the user can
// write one. Only a pinned session whose transcript carries its ID is placed
// in its space; anything else goes to quarantine, so a session can neither
// pull another space's transcript into its own nor push into another.
func TestSessionWritesNeedTheirOwnPinAndTranscript(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	for id, space := range map[string]string{"s-x": "personal", "s-y": "acme"} {
		if err := e.s.pins.insert(ctx, sessionPin{SessionID: id, Harness: "claude", Process: procstat.Identity{PID: 9, Started: time.Unix(1, 0)},
			FirstCwd: e.code, Space: space, Revision: e.rev, PinnedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	// s-x names itself with s-y's transcript: not its own, so quarantined.
	if err := storeSession(ctx, e.svc, handoff.Session{ID: "s-x", TranscriptPath: sessionTranscript(t, "s-y")}); err != nil {
		t.Fatal(err)
	}
	if o := e.placedAs(t, "session://s-x/transcript", spaces.Quarantine, rag.OriginSession); o["session"] != nil {
		t.Fatalf("an untied transcript kept its session: %v", o)
	}
	// A session orchd never pinned, whatever its record says.
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s-new", Cwd: filepath.Join(e.code, "company1")}); err != nil {
		t.Fatal(err)
	}
	if err := storeSession(ctx, e.svc, handoff.Session{ID: "s-new", TranscriptPath: sessionTranscript(t, "s-new"), Record: true}); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"session://s-new/transcript", "session://s-new/record"} {
		e.placedAs(t, uri, spaces.Quarantine, rag.OriginSession)
	}
	claude, codex := transcriptRoots()
	proj := filepath.Join(claude, "-Users-u-code")
	elsewhere := t.TempDir()
	for path, want := range map[string]bool{
		filepath.Join(proj, "abc.jsonl"):                                            true,
		filepath.Join(codex, "2026", "11", "rollout-2026-11-12T09-00-00-abc.jsonl"): true,
		filepath.Join(codex, "rollout-2026-11-12T09-00-00-x-abc.jsonl"):             false, // abc is only part of the ID
		filepath.Join(codex, "rollout-2026-abc.jsonl"):                              false,
		filepath.Join(proj, "xabc.jsonl"):                                           false,
		filepath.Join(codex, "rollout-abcd.jsonl"):                                  false,
		filepath.Join(proj, "abc.jsonl.bak"):                                        false,
		filepath.Join(claude, "abc.jsonl"):                                          false, // not in a project folder
		filepath.Join(proj, "deeper", "abc.jsonl"):                                  false,
		filepath.Join(proj, "rollout-1-abc.jsonl"):                                  false, // a Codex name in Claude's folder
		filepath.Join(elsewhere, "abc.jsonl"):                                       false, // named right, kept elsewhere
		filepath.Join(elsewhere, "rollout-1-abc.jsonl"):                             false,
	} {
		writeFile(t, path, "{}")
		if got := transcriptOf("abc", path); got != want {
			t.Errorf("transcriptOf(abc, %s) = %v", path, got)
		}
	}
	if err := os.Symlink(filepath.Join(elsewhere, "abc.jsonl"), filepath.Join(proj, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if transcriptOf("link", filepath.Join(proj, "link.jsonl")) || transcriptOf("", filepath.Join(proj, ".jsonl")) ||
		transcriptOf("gone", filepath.Join(proj, "gone.jsonl")) || transcriptOf("abc", "-Users-u-code/abc.jsonl") {
		t.Fatal("a link, an empty ID, a missing file or a relative path was taken as a session's transcript")
	}
	// Named after another session but kept outside its harness's folder.
	fake := filepath.Join(elsewhere, "s-y.jsonl")
	writeFile(t, fake, `{"type":"user","message":{"role":"user","content":"planted"}}`+"\n")
	if err := storeSession(ctx, e.svc, handoff.Session{ID: "s-y", TranscriptPath: fake}); err != nil {
		t.Fatal(err)
	}
	e.placedAs(t, "session://s-y/transcript", spaces.Quarantine, rag.OriginSession)
}

// TestEditedFilePlacedOnlyByItsRemoteIsQuarantined: an agent can write a
// .git naming any org beside a file it edits, so an edited file under no
// root is not placed by the remote alone.
func TestEditedFilePlacedOnlyByItsRemoteIsQuarantined(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	scratch, err := filepath.EvalSymlinks(t.TempDir()) // under no root
	if err != nil {
		t.Fatal(err)
	}
	defaultCheckout(t, scratch, "acme", "app")
	doc := filepath.Join(scratch, "PLAN.md")
	writeFile(t, doc, "# plan")
	if err := storeFile(ctx, e.svc, handoff.File{Path: doc}, "", nil); err != nil {
		t.Fatal(err)
	}
	e.placedAs(t, "file://"+doc, spaces.Quarantine, rag.OriginFS)
	// Registered as a source (any session may register one), the same
	// folder is not placed by its remote either.
	if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: scratch}, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
		t.Fatalf("refresh = %q, %v", st, err)
	}
	e.placedAs(t, sources.CheckoutURI(doc), spaces.Quarantine, rag.OriginFS)
	// Under a root, a source's stricter remote still places it, as it
	// places a session started there; an edited file there waits.
	moved := filepath.Join(e.code, "moved") // oss root, acme remote
	defaultCheckout(t, moved, "oss-org", "moved")
	writeFile(t, filepath.Join(moved, "README.md"), "# moved")
	p := &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}
	for writer, want := range map[string]string{rag.WriterSources: "acme", rag.WriterFile: spaces.Quarantine} {
		m := rag.Meta{SourceURI: "x", Writer: writer, Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(moved, "README.md")}}
		if err := p.place(ctx, &m); err != nil || m.Space != want {
			t.Fatalf("%s write placed in %q, %v; want %s", writer, m.Space, err, want)
		}
	}
}

// TestClientCannotReplaceAnotherSpacesSource: a write replaces every chunk
// under its URI, so a client may replace a source only where its own write
// would go.
func TestClientCannotReplaceAnotherSpacesSource(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	acme := sessionCtx(ctx, sessionPin{SessionID: "a", Space: "acme"})
	if _, err := e.s.Ingest(acme, &orchdv1.IngestRequest{SourceUri: "manual://plan", Text: "acme plan"}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]context.Context{
		"another space": sessionCtx(ctx, sessionPin{SessionID: "p", Space: "personal"}),
		"operator":      context.WithValue(ctx, resolvedKey{}, resolvedCaller{kind: callerOperator}),
		"unidentified":  ctx,
	} {
		_, err := e.s.Ingest(c, &orchdv1.IngestRequest{SourceUri: "manual://plan", Text: "overwritten"})
		if grpcstatus.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "acme") {
			t.Fatalf("%s replaced acme's source: %v", name, err)
		}
	}
	if _, err := e.s.Ingest(acme, &orchdv1.IngestRequest{SourceUri: "manual://plan", Text: "acme plan v2"}); err != nil {
		t.Fatalf("the same space could not replace its own source: %v", err)
	}
	e.placedAs(t, "manual://plan", "acme", rag.OriginClient)
	// A placer that fails refuses rather than replaces.
	broken := &server{rag: e.svc}
	if err := broken.mayReplace(ctx, rag.Origin{System: "bogus"}, "acme"); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mayReplace = %v", err)
	}
}

// TestForeignSummaryRequestIsQuarantined: a session may ask for another
// session's summary, but only a session's own request is tied to it.
func TestForeignSummaryRequestIsQuarantined(t *testing.T) {
	ctx := context.Background()
	s := &server{summaries: newSummaryQueue()}
	claude, _ := fakeTranscriptRoots(t)
	tr := filepath.Join(claude, "p", "s-y.jsonl")
	writeFile(t, tr, "{}")
	for _, c := range []struct {
		ctx     context.Context
		foreign bool
	}{
		{sessionCtx(ctx, sessionPin{SessionID: "s-x"}), true},
		{sessionCtx(ctx, sessionPin{SessionID: "s-y"}), false},
		{context.WithValue(ctx, resolvedKey{}, resolvedCaller{kind: callerOperator}), false},
	} {
		if resp, err := s.SummarizeSession(c.ctx, &orchdv1.SummarizeSessionRequest{SessionId: "s-y", TranscriptPath: tr}); err != nil || !resp.GetAccepted() {
			t.Fatalf("SummarizeSession = %v, %v", resp, err)
		}
		j := <-s.summaries.jobs
		if j.Foreign != c.foreign {
			t.Fatalf("job = %+v, want foreign %v", j, c.foreign)
		}
		if tied := tiedTranscript(j); (tied == "") != c.foreign {
			t.Fatalf("tied transcript = %q", tied)
		}
	}
}

func TestTranscriptRootsFollowTheHarnessHomes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "relative/ignored")
	if c, x := transcriptRoots(); c != filepath.Join(home, ".claude", "projects") || x != filepath.Join(home, ".codex", "sessions") {
		t.Fatalf("roots = %s, %s", c, x)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	t.Setenv("CODEX_HOME", "/cfg/codex")
	if c, x := transcriptRoots(); c != "/cfg/claude/projects" || x != "/cfg/codex/sessions" {
		t.Fatalf("roots with overrides = %s, %s", c, x)
	}
}

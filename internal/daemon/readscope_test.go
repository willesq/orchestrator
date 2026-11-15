package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/hook"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// readSpaces are the spaces readEnv seeds: two jobs (acme opts in to
// reading personal, globex does not), a public space and a private one.
var readSpaces = []string{"acme", "globex", "oss", "personal"}

// chunksPerSpace is how many chunks each space holds (SP-025.1.a1).
const chunksPerSpace = 200

// readEnv is a server with the readPolicy applied, a real rag.Service over
// an in-memory Qdrant holding chunksPerSpace chunks per space, synced docs
// in three owners' spaces, and a pin registry.
type readEnv struct {
	s    *server
	q    *searchQdrant
	code string
	tbl  procTable
	// labels is the applied revision every seeded chunk is labelled under.
	labels float64
}

func readPolicy(code string) string {
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "acme", Kind: spaces.Isolated, GitHubOrgs: []string{"acme"}, Roots: []string{code + "/acme"}, Reads: []string{"personal"}},
		{Name: "globex", Kind: spaces.Isolated, GitHubOrgs: []string{"globex"}, Roots: []string{code + "/globex"}},
		{Name: "oss", Kind: spaces.Public, GitHubOrgs: []string{"oss-org"}},
		{Name: "personal", Kind: spaces.Private, Roots: []string{code + "/personal"}},
	}, PublicRootSessions: "personal"}
	doc, _ := json.Marshal(p)
	return string(doc)
}

func memoURI(space string, i int) string { return fmt.Sprintf("note://%s/memo-%03d", space, i) }

func memoTitle(space string, i int) string { return fmt.Sprintf("%s memo %d", space, i) }

func memoText(space string, i int) string {
	return fmt.Sprintf("%s confidential memo %d: the %s ledger code is %s-%04d and the vault phrase is %s-%d-orchid",
		space, i, space, strings.ToUpper(space), i*7, space, i)
}

// docURIs are the synced docs each owner has, in its org's space.
var docURIs = map[string]string{
	"acme":    "repo://acme/app/README.md",
	"globex":  "repo://globex/x/README.md",
	"oss-org": "repo://oss-org/lib/README.md",
}

var ownerSpace = map[string]string{"acme": "acme", "globex": "globex", "oss-org": "oss"}

// chunkSeeder stores one chunk as payload says, in collection.
type chunkSeeder interface {
	seed(collection, id string, payload map[string]any)
}

// newReadEnv is readEnv over the in-memory Qdrant.
func newReadEnv(t *testing.T) readEnv {
	t.Helper()
	q, url := newSearchQdrant(t)
	e := newReadEnvOn(t, newSearchRAG(url), q)
	e.q = q
	return e
}

// newReadEnvOn is readEnv reading through svc, with chunks stored by seeder.
func newReadEnvOn(t *testing.T, svc *rag.Service, q chunkSeeder) readEnv {
	t.Helper()
	s := spacesServer(t)
	withEnforcement(t, s)
	code, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"acme/repo", "globex/repo", "personal"} {
		if err := os.MkdirAll(filepath.Join(code, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := draft(t, s, readPolicy(code))
	if len(d.GetProblems()) > 0 {
		t.Fatalf("policy problems: %v", d.GetProblems())
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	// Every chunk is labelled under the applied revision, as a write
	// under it is (INV-08).
	labels := float64(d.GetRevision())
	s.repos = newRepoLookups((&fakeRepos{repos: map[string][3]string{}}).run)
	tbl := withPins(t, s, code+"/acme/repo", code+"/globex/repo")

	s.rag, s.documents = svc, svc
	for _, sp := range readSpaces {
		for i := range chunksPerSpace {
			q.seed("c__"+sp, fmt.Sprintf("%s-%d", sp, i), map[string]any{
				"text": memoText(sp, i), "source_uri": memoURI(sp, i), "source_type": "manual", "scope": "work",
				"tags": []any{"team-" + sp, "common"}, "chunk_index": 0.0, "space": sp, "heading": memoTitle(sp, i), "writer": "client",
				"labels_v": labels,
			})
		}
	}
	// Content no policy reader may see: quarantine, and chunks stored
	// before spaces in the base collection.
	for i := range 10 {
		q.seed("c__quarantine", fmt.Sprintf("q-%d", i), map[string]any{"text": memoText("quarantine", i),
			"source_uri": memoURI("quarantine", i), "source_type": "manual", "chunk_index": 0.0, "space": "quarantine", "labels_v": labels})
		q.seed("c", fmt.Sprintf("legacy-%d", i), map[string]any{"text": memoText("legacy", i),
			"source_uri": memoURI("legacy", i), "source_type": "manual", "chunk_index": 0.0})
	}

	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dr := docs.NewRegistry(conn)
	ctx := context.Background()
	for owner, uri := range docURIs {
		repo := strings.Split(strings.TrimPrefix(uri, "repo://"), "/")[1]
		if err := dr.AddOwner(ctx, docs.Owner{Owner: owner, RefreshSecs: 3600}); err != nil {
			t.Fatal(err)
		}
		if err := dr.SetRepoState(ctx, docs.RepoState{Owner: owner, Repo: repo, HeadSHA: sha, Files: 1, Status: "ok"}); err != nil {
			t.Fatal(err)
		}
		if err := dr.SetHash(ctx, uri, "h", 200); err != nil {
			t.Fatal(err)
		}
		sp := ownerSpace[owner]
		q.seed("c__"+sp, "doc-"+owner, map[string]any{"text": owner + " readme: how to build " + repo,
			"source_uri": uri, "source_type": "repo_doc", "chunk_index": 0.0, "space": sp, "heading": owner + " README", "labels_v": labels})
	}
	s.docsReg = dr
	return readEnv{s: s, code: code, tbl: tbl, labels: labels}
}

// as returns a context resolved to c.
func as(c resolvedCaller) context.Context {
	return context.WithValue(context.Background(), resolvedKey{}, c)
}

func inSpace(space string) context.Context {
	return as(resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-" + space, Space: space}})
}

// spaceLeaks lists every hit, URI or title in rep that comes from a space not in
// allowed.
func spaceLeaks(allowed []string, texts ...string) []string {
	var out []string
	for _, txt := range texts {
		for _, sp := range append(slices.Clone(readSpaces), "quarantine", "legacy", "globex", "acme") {
			if !slices.Contains(allowed, sp) && strings.Contains(strings.ToLower(txt), sp) {
				out = append(out, txt)
				break
			}
		}
	}
	return out
}

// readAll runs every read path from ctx with query, and returns every
// text, URI and title they gave back.
func (e readEnv) readAll(t *testing.T, ctx context.Context, query, uri string) []string {
	t.Helper()
	var got []string
	for _, req := range []*orchdv1.SearchRequest{
		{Query: query, TopK: 100},
		{Query: query, TopK: 100, Mode: "dense"},
		{Query: query, TopK: 100, Ranking: "plain", PerSource: -1},
		{Query: query, TopK: 100, SourceUri: uri},
		{Query: query, TopK: 100, Tags: []string{"team-globex"}},
		{Query: query, TopK: 100, Scope: "work", AllTags: []string{"common"}},
	} {
		rep, err := e.s.Search(ctx, req)
		if err != nil {
			t.Fatalf("Search(%v): %v", req, err)
		}
		for _, h := range rep.GetHits() {
			got = append(got, h.GetText(), h.GetSourceUri(), h.GetHeading())
		}
	}
	for _, req := range []*orchdv1.ListSourcesRequest{{}, {UriPrefix: "note://globex/"}, {SourceType: "repo_doc"}} {
		rep, err := e.s.ListSources(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		for _, src := range rep.GetSources() {
			got = append(got, src.GetSourceUri())
		}
	}
	doc, err := e.s.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: uri})
	switch {
	case grpcstatus.Code(err) == codes.NotFound:
	case err != nil:
		t.Fatal(err)
	default:
		got = append(got, doc.GetText(), doc.GetSourceUri())
	}
	docsRep, err := e.s.ListDocs(ctx, &orchdv1.ListDocsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range docsRep.GetFiles() {
		got = append(got, f.GetSourceUri())
	}
	for _, o := range docsRep.GetOwners() {
		got = append(got, o.GetOwner())
		for _, r := range o.GetRepos() {
			got = append(got, o.GetOwner()+"/"+r.GetRepo())
		}
	}
	return got
}

// clientFor serves e.s over an in-memory connection whose every call
// resolves to c, as orchd's resolver would for that caller.
func (e readEnv) clientFor(t *testing.T, c resolvedCaller) orchdv1.OrchdClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	inject := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(context.WithValue(ctx, resolvedKey{}, c), req)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(inject))
	orchdv1.RegisterOrchdServer(gs, e.s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return orchdv1.NewOrchdClient(conn)
}

// explained is every hit auto-RAG (and orch rag why) considered for
// prompt, and the block it would inject.
func explained(t *testing.T, client orchdv1.OrchdClient, prompt, cwd string) []string {
	t.Helper()
	cfg := hook.LoadAutoRAG()
	cfg.Gate, cfg.MinScore, cfg.MinScoreStatement, cfg.MinPromptChars = "off", 0, 0, 1
	ex := hook.Explain(context.Background(), cfg, client, prompt, cwd)
	if len(ex.Errors) > 0 {
		t.Fatalf("explain errors: %v", ex.Errors)
	}
	// The block quotes the prompt, so only the URIs it injects are read
	// from it.
	var got []string
	for _, w := range strings.Fields(ex.Block) {
		if strings.Contains(w, "://") {
			got = append(got, w)
		}
	}
	for _, d := range ex.Decisions {
		got = append(got, d.Hit.GetText(), d.Hit.GetSourceUri(), d.Hit.GetHeading())
	}
	return got
}

// TestReadPathsKeepSpacesApart is SP-025.1.a1: from a session in acme,
// every read path queried with the verbatim text of globex's chunks
// returns no hit, URI or title from globex, and never reads globex's
// collection; acme still reads itself, personal (opted in) and public.
func TestReadPathsKeepSpacesApart(t *testing.T) {
	e := newReadEnv(t)
	acme := inSpace("acme")
	allowed := []string{"acme", "personal", "oss"}
	cross := 0
	for i := range chunksPerSpace {
		got := e.readAll(t, acme, memoText("globex", i), memoURI("globex", i))
		if l := spaceLeaks(allowed, got...); len(l) > 0 {
			cross += len(l)
			t.Errorf("globex chunk %d leaked: %q", i, l[:min(3, len(l))])
		}
	}
	got := e.readAll(t, acme, "acme confidential memo 3 ledger", memoURI("acme", 3))
	for _, want := range []string{memoURI("acme", 3), memoURI("personal", 3), memoURI("oss", 3), docURIs["acme"], docURIs["oss-org"], "acme/app", "oss-org/lib"} {
		if !slices.Contains(got, want) {
			t.Errorf("acme's reads lack %s", want)
		}
	}
	// The clients built on Search: auto-RAG, session recall and orch rag
	// why (the same selection), through a real gRPC connection.
	client := e.clientFor(t, resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-a", Space: "acme"}})
	for _, prompt := range []string{memoText("globex", 7), "what did I work on yesterday?", "globex ledger code GLOBEX-0049 vault phrase"} {
		// Reason: a cwd in globex's folder must not widen the read either.
		if l := spaceLeaks(allowed, explained(t, client, prompt, e.code+"/globex/repo")...); len(l) > 0 {
			cross += len(l)
			t.Errorf("auto-RAG for %q leaked %q", prompt, l[:min(3, len(l))])
		}
	}
	for _, c := range []string{"c__globex", "c__quarantine", "c"} {
		if n := e.q.readsOf(c); n != 0 {
			t.Errorf("%s was read %d times", c, n)
		}
	}
	t.Logf("cross-space results across all read paths: %d (%d globex chunks queried)", cross, chunksPerSpace)
}

// TestPublicReadEverywhere is SP-025.1.a2: public chunks reach sessions in
// every space, a session with no space, an unidentified caller and an
// operator with no evidence of the user; only public and their own spaces.
func TestPublicReadEverywhere(t *testing.T) {
	e := newReadEnv(t)
	callers := map[string]context.Context{
		"acme": inSpace("acme"), "globex": inSpace("globex"), "personal": inSpace("personal"),
		"no space":          inSpace(""),
		"quarantined pin":   inSpace(spaces.Quarantine),
		"unidentified":      as(none("unidentified process")),
		"detached operator": as(resolvedCaller{kind: callerOperator}),
	}
	for name, ctx := range callers {
		rep, err := e.s.Search(ctx, &orchdv1.SearchRequest{Query: memoText("oss", 11), TopK: 100})
		if err != nil {
			t.Fatal(err)
		}
		var uris []string
		for _, h := range rep.GetHits() {
			uris = append(uris, h.GetSourceUri())
		}
		if !slices.Contains(uris, memoURI("oss", 11)) {
			t.Errorf("%s: public chunk missing from %v", name, uris[:min(5, len(uris))])
		}
		doc, err := e.s.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: memoURI("oss", 11)})
		if err != nil || !strings.Contains(doc.GetText(), "oss confidential memo 11") {
			t.Errorf("%s: GetDocument of a public source = %v, %v", name, doc, err)
		}
	}
	for _, name := range []string{"no space", "quarantined pin", "unidentified", "detached operator"} {
		if l := spaceLeaks([]string{"oss"}, e.readAll(t, callers[name], memoText("acme", 1), memoURI("acme", 1))...); len(l) > 0 {
			t.Errorf("%s read beyond public: %q", name, l[:min(3, len(l))])
		}
	}
}

// TestCallerFiltersOnlyNarrow is SP-025.1.a3: tags, source_uri, scope and a
// prefix narrow within the caller's spaces but never add one.
func TestCallerFiltersOnlyNarrow(t *testing.T) {
	e := newReadEnv(t)
	acme := inSpace("acme")
	for _, req := range []*orchdv1.SearchRequest{
		{Query: memoText("globex", 5), SourceUri: memoURI("globex", 5)},
		{Query: memoText("globex", 5), Tags: []string{"team-globex"}},
		{Query: memoText("globex", 5), AllTags: []string{"team-globex", "common"}},
		{Query: memoText("globex", 5), Scope: "work", SourceType: "manual", Tags: []string{"team-globex"}},
	} {
		rep, err := e.s.Search(acme, req)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(rep.GetHits()); n != 0 {
			t.Errorf("filter %v added %d hits from another space", req, n)
		}
	}
	rep, err := e.s.Search(acme, &orchdv1.SearchRequest{Query: "memo", TopK: 100, Tags: []string{"team-acme"}, PerSource: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.GetHits()) == 0 {
		t.Fatal("a tag filter on acme's own content found nothing")
	}
	for _, h := range rep.GetHits() {
		if !strings.HasPrefix(h.GetSourceUri(), "note://acme/") {
			t.Errorf("tag team-acme returned %s", h.GetSourceUri())
		}
	}
	list, err := e.s.ListSources(acme, &orchdv1.ListSourcesRequest{UriPrefix: "note://globex/"})
	if err != nil || len(list.GetSources()) != 0 || list.GetTotalSources() != 0 {
		t.Fatalf("ListSources by globex's prefix = %v, %v", list, err)
	}
	if _, err := e.s.GetDocument(acme, &orchdv1.GetDocumentRequest{SourceUri: memoURI("globex", 5)}); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("GetDocument of globex's source = %v, want NotFound", err)
	}
}

// captureLog collects orchd's log lines for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMemoryFilterOffIsLogged is SP-025.1.a4: with memory filtering switched
// off, cross-space results come back and every such response is logged as
// not enforced; switched on again, they stop.
func TestMemoryFilterOffIsLogged(t *testing.T) {
	e := newReadEnv(t)
	setSwitch(t, e.s, "memory", spaces.SwitchOff, 0)
	logs := captureLog(t)
	acme := inSpace("acme")
	got := e.readAll(t, acme, memoText("globex", 9), memoURI("globex", 9))
	if !slices.Contains(got, memoURI("globex", 9)) {
		t.Fatal("with memory filtering off, globex's chunk was not returned")
	}
	// readAll makes 6 searches, 3 listings, a document and a docs list.
	if n := strings.Count(logs.String(), "not enforced"); n != 11 {
		t.Fatalf("%d responses logged as not enforced, want 11:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "Search for session sess-acme: not enforced") {
		t.Fatalf("log does not name the RPC and caller:\n%s", logs.String())
	}
	setSwitch(t, e.s, "memory", spaces.SwitchOn, 0)
	if l := spaceLeaks([]string{"acme", "personal", "oss"}, e.readAll(t, acme, memoText("globex", 9), memoURI("globex", 9))...); len(l) > 0 {
		t.Fatalf("switched on again, acme read %q", l[:min(3, len(l))])
	}
}

// TestJobReadsPersonalOnlyWhenOptedIn is SP-025.1.a5 (1 of 2): a job
// session reads personal only when its space lists it (D-16).
func TestJobReadsPersonalOnlyWhenOptedIn(t *testing.T) {
	e := newReadEnv(t)
	for space, want := range map[string]bool{"acme": true, "globex": false} {
		got := e.readAll(t, inSpace(space), memoText("personal", 4), memoURI("personal", 4))
		if slices.Contains(got, memoURI("personal", 4)) != want {
			t.Errorf("%s reads personal: %v, want %v", space, !want, want)
		}
	}
}

// TestJobNeverReadsAnotherJob is SP-025.1.a5 (2 of 2): neither job reads
// the other, whether or not it opted in to personal; personal reads
// neither job.
func TestJobNeverReadsAnotherJob(t *testing.T) {
	e := newReadEnv(t)
	for reader, other := range map[string]string{"acme": "globex", "globex": "acme", "personal": "acme"} {
		got := e.readAll(t, inSpace(reader), memoText(other, 2), memoURI(other, 2))
		for _, g := range got {
			if strings.Contains(g, other) {
				t.Errorf("%s read %s's %q", reader, other, g)
				break
			}
		}
	}
	if l := spaceLeaks([]string{"personal", "oss"}, e.readAll(t, inSpace("personal"), memoText("globex", 2), memoURI("globex", 2))...); len(l) > 0 {
		t.Errorf("personal read %q", l[:min(3, len(l))])
	}
}

// TestDetachedHarnessReadsNoMore is SP-025.1.a6 (D-38): a session in acme
// cannot start a harness in globex's folder (the SP-041 guard refuses
// it); a fake claude detached from its session and pinned in globex's
// folder gets no pin and reads public only; one nested in the session
// takes the session's own pin. Neither reads beyond acme's set.
func TestDetachedHarnessReadsNoMore(t *testing.T) {
	e := newReadEnv(t)
	if _, err := e.s.PinSession(inClaude, pinReq("sess-acme", e.code+"/acme/repo", "")); err != nil {
		t.Fatal(err)
	}
	// The precondition: the enforcing guard refuses the start.
	res, err := e.s.spaces.Current()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := spaces.NewGuardPolicy(res, nil)
	if err != nil {
		t.Fatal(err)
	}
	cwd := e.code + "/acme/repo"
	for _, cmd := range []string{
		"cd " + e.code + "/globex/repo && nohup claude -p hi >/dev/null 2>&1 &",
		"(cd " + e.code + "/globex/repo; setsid claude &)",
	} {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		ev := spaces.GuardEvent{Kind: spaces.EventPreToolUse, Tool: "Bash", Cwd: cwd, Targets: spaces.Targets("Bash", in, cwd)}
		if d := spaces.Decide(pol, spaces.GuardPin{Found: true, Space: "acme"}, spaces.GuardEnforcement{}, ev); d.Verdict != spaces.Refuse {
			t.Fatalf("guard let %q through: %+v", cmd, d)
		}
	}

	// Had one started anyway, detached (its parent exited, so launchd
	// adopted it), it takes no pin...
	detachedStart := claudeStart.Add(40 * time.Second)
	e.tbl.started[430], e.tbl.cwd[430] = detachedStart, e.code+"/globex/repo"
	detached := callerAuth{pid: 905, harness: "claude", harnessProc: procstat.Identity{PID: 430, Started: detachedStart}, harnessOrphan: true}
	if _, err := e.s.PinSession(callerCtx(detached), pinReq("sess-detached", e.code+"/globex/repo", "")); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("detached harness pinned: %v", err)
	}
	r := &callerResolver{pins: e.s.pins}
	c := r.resolve(callerCtx(detached))
	if c.kind != callerNone {
		t.Fatalf("detached harness resolved to %+v", c)
	}
	if l := spaceLeaks([]string{"oss"}, e.readAll(t, as(c), memoText("globex", 1), memoURI("globex", 1))...); len(l) > 0 {
		t.Fatalf("detached harness read %q", l[:min(3, len(l))])
	}

	// ...and one still inside the session copies the session's pin.
	nestedStart := claudeStart.Add(50 * time.Second)
	e.tbl.started[420], e.tbl.cwd[420] = nestedStart, e.code+"/globex/repo"
	nested := callerAuth{pid: 906, harness: "claude", harnessProc: procstat.Identity{PID: 420, Started: nestedStart},
		outerHarnesses: []procstat.Identity{{PID: 400, Started: claudeStart}}}
	pin, err := e.s.PinSession(callerCtx(nested), pinReq("sess-nested", e.code+"/globex/repo", ""))
	if err != nil || pin.GetSpace() != "acme" {
		t.Fatalf("nested harness pin = %v, %v; want acme's", pin, err)
	}
	c = r.resolve(callerCtx(nested))
	if c.kind != callerSession || c.pin.Space != "acme" {
		t.Fatalf("nested harness resolved to %+v", c)
	}
	if l := spaceLeaks([]string{"acme", "personal", "oss"}, e.readAll(t, as(c), memoText("globex", 1), memoURI("globex", 1))...); len(l) > 0 {
		t.Fatalf("nested harness read %q", l[:min(3, len(l))])
	}
}

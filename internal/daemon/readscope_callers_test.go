package daemon

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/workflows"
)

// operatorTree is a fake process table for D-71. launchd is 1.
//
//	Terminal 30 → zsh 31 → orch 32     a command the user typed
//	script 35 under launchd → orch 37  a pty an agent opened (RC-705)
//	orch 40 under launchd              a double fork that called setsid
//	Orch 50 under launchd              Orch.app, or a program posing as it
type operatorTree struct {
	parents map[int]int
	progs   map[int]procstat.Program
}

func newOperatorTree() operatorTree {
	return operatorTree{
		parents: map[int]int{30: 1, 31: 30, 32: 31, 35: 1, 37: 35, 40: 1, 50: 1},
		progs: map[int]procstat.Program{
			30: {Name: "Terminal"}, 31: {Name: "zsh"}, 32: {Name: "orch"}, 35: {Name: "script"}, 37: {Name: "orch"},
			40: {Name: "orch"}, 50: {Name: "Orch", Path: "/Applications/Orch.app/Contents/MacOS/Orch"},
		},
	}
}

func (tr operatorTree) creds(pid int, app *appVerifier) peerCredentials {
	return peerCredentials{
		self: 1000,
		parent: func(p int) (int, error) {
			if pp, ok := tr.parents[p]; ok {
				return pp, nil
			}
			return 0, errors.New("no such process")
		},
		program: func(p int) (procstat.Program, error) {
			if pr, ok := tr.progs[p]; ok {
				return pr, nil
			}
			return procstat.Program{}, errors.New("no such process")
		},
		peerPID: func(net.Conn) (int, error) { return pid, nil },
		app:     app,
	}
}

// signedConn is a connection whose peer's code signature the fake verifier
// reports.
type signedConn struct {
	net.Conn
	sig codeSignature
	err error
}

var genuineCDHash = []byte{1, 2, 3, 4}

// fakeVerifier recognizes signatures from signedConn, with the installed
// bundle at /Applications/Orch.app signed with genuineCDHash.
func fakeVerifier(installed ...string) *appVerifier {
	if len(installed) == 0 {
		installed = []string{"/Applications/Orch.app"}
	}
	return &appVerifier{
		running: func(c net.Conn, req string) (codeSignature, error) {
			sc := c.(signedConn)
			if req != appRequirement {
				return codeSignature{}, errors.New("wrong requirement")
			}
			return sc.sig, sc.err
		},
		static: func(path, req string) (codeSignature, error) {
			if path != "/Applications/Orch.app" || req != appRequirement {
				return codeSignature{}, errors.New("not signed")
			}
			return codeSignature{Path: path, Flags: codeRuntime, CDHash: genuineCDHash}, nil
		},
		exists: func(p string) bool { return slices.Contains(installed, p) },
		dirs:   []string{"/Applications", "/home/u/Applications"},
	}
}

func appSig(path string, flags uint32, cdhash []byte) signedConn {
	return signedConn{sig: codeSignature{Path: path, Flags: flags, CDHash: cdhash}}
}

// TestOnlyOrchAppIsTheOperator is SP-025.1.a7 under D-71: with a policy
// applied, a caller with no harness above it reads every space only when
// it is Orch.app, recognized by its code signature (RC-706). A command typed
// at a terminal, a process with a pty of its own (RC-705), a detached
// process, a copy of the app elsewhere, the app without the hardened runtime
// or running other code than its bundle's, and an app installed in both
// folders read public memory only.
func TestOnlyOrchAppIsTheOperator(t *testing.T) {
	e := newReadEnv(t)
	tr := newOperatorTree()
	r := &callerResolver{pins: e.s.pins, needsEvidence: policyApplied(e.s.spaces)}
	genuine := appSig("/Applications/Orch.app", codeRuntime|0x2, genuineCDHash)
	for _, tt := range []struct {
		name     string
		pid      int
		conn     net.Conn
		verifier *appVerifier
		app      bool
	}{
		{"Orch.app", 50, genuine, fakeVerifier(), true},
		{"Orch.app by its executable", 50, appSig("/Applications/Orch.app/Contents/MacOS/Orch", codeRuntime, genuineCDHash), fakeVerifier(), true},
		{"typed at a terminal", 32, signedConn{err: errors.New("not signed")}, fakeVerifier(), false},
		{"a pty of its own (RC-705)", 37, signedConn{err: errors.New("not signed")}, fakeVerifier(), false},
		{"double fork with setsid", 40, nil, fakeVerifier(), false},
		{"a signed copy elsewhere", 50, appSig("/tmp/x/Orch.app", codeRuntime, genuineCDHash), fakeVerifier(), false},
		{"without the hardened runtime", 50, appSig("/Applications/Orch.app", 0x2, genuineCDHash), fakeVerifier(), false},
		{"other code than its bundle's", 50, appSig("/Applications/Orch.app", codeRuntime, []byte{9}), fakeVerifier(), false},
		{"no code hash", 50, appSig("/Applications/Orch.app", codeRuntime, nil), fakeVerifier(), false},
		{"installed in both folders", 50, genuine, fakeVerifier("/Applications/Orch.app", "/home/u/Applications/Orch.app"), false},
		{"not installed", 50, genuine, fakeVerifier("/nowhere"), false},
		{"a build that cannot check signatures", 50, genuine, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			peerCtx := callerCtx(tr.creds(tt.pid, tt.verifier).identify(tt.conn))
			c := r.resolve(peerCtx)
			if c.kind != callerOperator || (c.operatorBy == operatorApp) != tt.app || !c.evidenceNeeded {
				t.Fatalf("resolved %+v, want the operator (app %v) needing evidence", c, tt.app)
			}
			got := e.readAll(t, as(c), memoText("globex", 3), memoURI("globex", 3))
			if tt.app {
				for _, want := range []string{memoURI("globex", 3), memoURI("acme", 3), memoURI("quarantine", 3), memoURI("legacy", 3)} {
					if !slices.Contains(got, want) {
						t.Errorf("Orch.app does not read every collection: lacks %s", want)
					}
				}
				return
			}
			if l := spaceLeaks([]string{"oss"}, got...); len(l) > 0 {
				t.Fatalf("read beyond public: %q", l[:min(3, len(l))])
			}
		})
	}
	// The static bundle that fails its own check, or lacks the runtime, is
	// not the app either.
	v := fakeVerifier()
	v.static = func(string, string) (codeSignature, error) { return codeSignature{}, errors.New("modified") }
	if v.isApp(genuine) {
		t.Fatal("an app whose bundle fails its check is Orch.app")
	}
	v.static = func(p, _ string) (codeSignature, error) { return codeSignature{Path: p, CDHash: genuineCDHash}, nil }
	if v.isApp(genuine) {
		t.Fatal("an app whose bundle lacks the hardened runtime is Orch.app")
	}
	if fakeVerifier().isApp(nil) {
		t.Fatal("no connection is Orch.app")
	}
	if nv := newAppVerifier("/home/u"); nv != nil && !slices.Equal(nv.dirs, []string{"/Applications", "/home/u/Applications"}) {
		t.Fatalf("install folders %v", nv.dirs)
	}
	if nv := newAppVerifier(""); nv != nil && !slices.Equal(nv.dirs, []string{"/Applications"}) {
		t.Fatalf("install folders without a home %v", nv.dirs)
	}
}

// TestOperatorOnlyRPCsNeedOrchApp is RC-707 under D-71: while a policy is
// applied, operator-only RPCs are refused to any caller but Orch.app, so
// a detached process cannot switch memory filtering off, read or apply the
// policy; with no policy the process tree alone is the operator, as before
// (INV-10).
func TestOperatorOnlyRPCsNeedOrchApp(t *testing.T) {
	e := newReadEnv(t)
	tr := newOperatorTree()
	r := &callerResolver{pins: e.s.pins, needsEvidence: policyApplied(e.s.spaces)}
	resolved := func(pid int, c net.Conn) context.Context {
		ctx := callerCtx(tr.creds(pid, fakeVerifier()).identify(c))
		return context.WithValue(ctx, resolvedKey{}, r.resolve(ctx))
	}
	calls := map[string]func(context.Context) error{
		"SetEnforcement": func(ctx context.Context) error {
			_, err := e.s.SetEnforcement(ctx, &orchdv1.SetEnforcementRequest{Layer: "memory", State: spaces.SwitchOff})
			return err
		},
		"DiffSpaces": func(ctx context.Context) error {
			_, err := e.s.DiffSpaces(ctx, &orchdv1.DiffSpacesRequest{})
			return err
		},
		"ApplySpaces": func(ctx context.Context) error {
			_, err := e.s.ApplySpaces(ctx, &orchdv1.ApplySpacesRequest{ReviewedRevision: 1 << 40})
			return err
		},
	}
	for name, call := range calls {
		for _, pid := range []int{32, 37, 40} {
			err := call(resolved(pid, signedConn{err: errors.New("not signed")}))
			if grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "not Orch.app") {
				t.Errorf("%s from process %d = %v, want refused", name, pid, err)
			}
		}
		if err := call(resolved(50, appSig("/Applications/Orch.app", codeRuntime, genuineCDHash))); grpcstatus.Code(err) == codes.PermissionDenied {
			t.Errorf("%s from Orch.app refused: %v", name, err)
		}
	}
	if sw := switchesOf(t, e.s, operatorCtx)["memory"]; sw.GetState() != spaces.SwitchOff {
		t.Fatalf("Orch.app could not switch memory filtering off: %v", sw)
	}
	// With no policy, a terminal is the operator, as before spaces.
	r.needsEvidence = policyApplied(nil)
	for name, call := range calls {
		if err := call(resolved(32, nil)); grpcstatus.Code(err) == codes.PermissionDenied {
			t.Errorf("with no policy %s from a terminal = %v", name, err)
		}
	}
	if (&callerResolver{}).needEvidence() || (*callerResolver)(nil).needEvidence() {
		t.Fatal("a resolver without a policy check needs evidence")
	}
}

// spaceDispatcher records the space each dispatch carries.
type spaceDispatcher struct{ got *string }

func (d spaceDispatcher) Dispatch(ctx context.Context, _, _, _ string, _ workflows.Route) (string, error) {
	*d.got = queue.ReadSpaceOf(ctx)
	return "task-1", nil
}

// TestTasksReadTheirDispatchersSpaces is SP-025.1.a8 (D-46): a task reads
// what its dispatching session may read and writes to that space; a task
// the operator dispatched takes its working directory's placement; a task
// orchd does not track, or one dispatched by anyone else, reads public only.
func TestTasksReadTheirDispatchersSpaces(t *testing.T) {
	e := newReadEnv(t)
	var recorded string
	e.s.disp = spaceDispatcher{got: &recorded}
	terminal := as(resolvedCaller{kind: callerOperator, operatorBy: operatorApp})
	for _, tt := range []struct {
		name string
		ctx  context.Context
		cwd  string
		want string
	}{
		{"a session in acme", inSpace("acme"), e.code + "/globex/repo", "acme"},
		{"a session in globex", inSpace("globex"), "", "globex"},
		{"the operator in acme's folder", terminal, e.code + "/acme/repo", "acme"},
		{"the operator in personal's folder", terminal, e.code + "/personal", "personal"},
		{"the operator with no directory", terminal, "", spaces.Quarantine},
		{"the operator with no evidence", as(resolvedCaller{kind: callerOperator}), e.code + "/acme/repo", ""},
		{"an unpinned harness", as(none("no pin")), e.code + "/acme/repo", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorded = "unset"
			if _, err := e.s.Dispatch(tt.ctx, &orchdv1.DispatchRequest{Body: "x", Cwd: tt.cwd}); err != nil {
				t.Fatal(err)
			}
			if recorded != tt.want {
				t.Fatalf("task records space %q, want %q", recorded, tt.want)
			}
		})
	}
	// The recorded space reaches the run orchd resolves a task's calls to.
	run := agentTask(workflows.AgentTaskInput{ID: "task-1", Space: "acme"})
	if run.Space != "acme" {
		t.Fatalf("runner task space = %q", run.Space)
	}
	task := func(space string) resolvedCaller {
		return resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-1", Space: space}}
	}
	got := e.readAll(t, as(task("acme")), memoText("personal", 6), memoURI("personal", 6))
	if !slices.Contains(got, memoURI("personal", 6)) {
		t.Error("acme's task cannot read personal, which acme lists")
	}
	if l := spaceLeaks([]string{"acme", "personal", "oss"}, e.readAll(t, as(task("acme")), memoText("globex", 6), memoURI("globex", 6))...); len(l) > 0 {
		t.Errorf("acme's task read %q", l[:min(3, len(l))])
	}
	for name, c := range map[string]resolvedCaller{
		"untracked task":                 {kind: callerTask},
		"task with no space":             task(""),
		"task of the operator's $TMPDIR": task(spaces.Quarantine),
	} {
		if l := spaceLeaks([]string{"oss"}, e.readAll(t, as(c), memoText("acme", 6), memoURI("acme", 6))...); len(l) > 0 {
			t.Errorf("%s read %q", name, l[:min(3, len(l))])
		}
	}
	// Its writes go to that space.
	m := rag.Meta{SourceURI: "note://task", Writer: rag.WriterClient, Origin: clientOrigin(as(task("acme")), "note://task")}
	if err := (&writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}).place(context.Background(), &m); err != nil || m.Space != "acme" {
		t.Fatalf("task write placed in %q, %v; want acme", m.Space, err)
	}
	// With no policy a dispatch records nothing.
	e.s.spaces = nil
	recorded = "unset"
	if _, err := e.s.Dispatch(inSpace("acme"), &orchdv1.DispatchRequest{Body: "x"}); err != nil || recorded != "" {
		t.Fatalf("with no policy the task records %q, %v", recorded, err)
	}
}

// TestNoPolicyReadsAsBefore is INV-10: with no policy applied, or none
// with spaces, every caller reads every collection, as before spaces.
func TestNoPolicyReadsAsBefore(t *testing.T) {
	for _, c := range []resolvedCaller{{kind: callerOperator}, none("x"), {kind: callerSession}, {kind: callerTask}} {
		if sc := (&server{}).readScope(as(c), orchdv1.Orchd_Search_FullMethodName); sc.filtered() {
			t.Fatalf("no engine: %+v reads %v", c, sc.spaces)
		}
		s := spacesServer(t)
		if sc := s.readScope(as(c), orchdv1.Orchd_Search_FullMethodName); sc.filtered() {
			t.Fatalf("no policy: %+v reads %v", c, sc.spaces)
		}
		applyPolicy(t, s, `{"spaces":[]}`)
		if sc := s.readScope(as(c), orchdv1.Orchd_Search_FullMethodName); sc.filtered() {
			t.Fatalf("an empty policy: %+v reads %v", c, sc.spaces)
		}
	}
}

// TestPayloadLayerAndFailures: a chunk whose payload names another space
// is not read from a readable space's collection (the second layer), and a
// store failure reaches a filtered reader without the collection's name.
func TestPayloadLayerAndFailures(t *testing.T) {
	e := newReadEnv(t)
	e.q.seed("c__acme", "stray", map[string]any{"text": memoText("globex", 900), "source_uri": memoURI("globex", 900),
		"source_type": "manual", "chunk_index": 0.0, "space": "globex", "labels_v": e.labels})
	if l := spaceLeaks([]string{"acme", "personal", "oss"}, e.readAll(t, inSpace("acme"), memoText("globex", 900), memoURI("globex", 900))...); len(l) > 0 {
		t.Fatalf("a stray globex chunk in acme's collection was read: %q", l[:min(3, len(l))])
	}
	e.s.rag = rag.NewService(wordEmbedder{}, rag.NewQdrantStore("http://127.0.0.1:1", "c", ""), 200, 0)
	e.s.documents = e.s.rag.(*rag.Service)
	logs := captureReadLog(t)
	for name, call := range map[string]func(context.Context) error{
		"Search": func(ctx context.Context) error {
			_, err := e.s.Search(ctx, &orchdv1.SearchRequest{Query: "x"})
			return err
		},
		"ListSources": func(ctx context.Context) error {
			_, err := e.s.ListSources(ctx, &orchdv1.ListSourcesRequest{})
			return err
		},
		"GetDocument": func(ctx context.Context) error {
			_, err := e.s.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: "note://x"})
			return err
		},
		"ListDocs": func(ctx context.Context) error {
			_, err := e.s.ListDocs(ctx, &orchdv1.ListDocsRequest{})
			return err
		},
	} {
		err := call(inSpace("acme"))
		if grpcstatus.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "c__") {
			t.Errorf("%s for a filtered reader = %v", name, err)
		}
		if err := call(as(resolvedCaller{kind: callerOperator, operatorBy: operatorApp})); err == nil || grpcstatus.Code(err) == codes.Unavailable {
			t.Errorf("%s for the operator = %v, want the store's own error", name, err)
		}
	}
	if !strings.Contains(logs.String(), "reading the memory store") {
		t.Fatalf("store failures not logged:\n%s", logs.String())
	}
}

// TestReadScopeFailsClosed: a policy that does not verify reads nothing;
// a reader that cannot read by space finds no document for a filtered
// caller.
func TestReadScopeFailsClosed(t *testing.T) {
	e := newReadEnv(t)
	e.s.documents = fakeDocuments{doc: rag.Document{Text: "globex secret", Chunks: 1}}
	if _, err := e.s.GetDocument(inSpace("acme"), &orchdv1.GetDocumentRequest{SourceUri: "note://x"}); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("GetDocument through a reader without spaces = %v, want NotFound", err)
	}
	if doc, err := e.s.GetDocument(as(resolvedCaller{kind: callerOperator, operatorBy: operatorApp}), &orchdv1.GetDocumentRequest{SourceUri: "note://x"}); err != nil || doc.GetText() != "globex secret" {
		t.Fatalf("the operator's GetDocument = %v, %v", doc, err)
	}
	conn := mustDB(t)
	if _, err := conn.Exec(`UPDATE spaces_revisions SET doc = '{"spaces":[]}'`); err != nil {
		t.Fatal(err)
	}
	broken, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	e.s.spaces = broken
	for _, c := range []context.Context{inSpace("acme"), as(resolvedCaller{kind: callerOperator, operatorBy: operatorApp})} {
		if sc := e.s.readScope(c, orchdv1.Orchd_Search_FullMethodName); sc.spaces == nil || len(sc.spaces) != 0 {
			t.Fatalf("a broken policy reads %v", sc.spaces)
		}
	}
	rep, err := e.s.Search(inSpace("acme"), &orchdv1.SearchRequest{Query: memoText("acme", 1)})
	if err != nil || len(rep.GetHits()) != 0 {
		t.Fatalf("Search under a broken policy = %v, %v", rep, err)
	}
}

// TestReaderLabel: orchd's log names a reader by kind and ID only.
func TestReaderLabel(t *testing.T) {
	for c, want := range map[*resolvedCaller]string{
		{kind: callerSession, pin: sessionPin{SessionID: "s 1"}}: "session s?1",
		{kind: callerTask}: "an untracked queued task",
		{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-9"}}: "task task-9",
		{kind: callerOperator}:                          "the operator (not Orch.app)",
		{kind: callerOperator, operatorBy: operatorApp}: "the operator",
		{kind: callerNone}:                              "an unidentified caller",
	} {
		if got := readerLabel(*c); got != want {
			t.Errorf("readerLabel(%+v) = %q, want %q", *c, got, want)
		}
	}
}

// TestNarrowDocsHidesTheSyncOfAnUnlistedOwner: the docs sync's progress
// names an owner only when that owner is listed, and for an owner listed
// for only some repos, only a listed repo and no counts.
func TestNarrowDocsHidesTheSyncOfAnUnlistedOwner(t *testing.T) {
	e := newReadEnv(t)
	res, err := e.s.spaces.Current()
	if err != nil {
		t.Fatal(err)
	}
	scope := readScope{spaces: readableFrom(res, "acme"), res: res}
	for _, tt := range []struct {
		owner, repo, wantOwner, wantRepo string
		total                            int32
	}{
		{"globex", "x", "", "", 0},
		{"acme", "app", "acme", "app", 9},
		{"oss-org", "lib", "oss-org", "lib", 9},
		{"stranger", "shown", "stranger", "shown", 0},
		{"stranger", "hidden", "stranger", "", 0},
	} {
		out := &orchdv1.ListDocsResponse{
			Owners: []*orchdv1.DocOwner{{Owner: "acme"}, {Owner: "globex", Repos: []*orchdv1.DocRepo{{Repo: "x"}}}, {Owner: "oss-org"},
				{Owner: "stranger", Error: "hidden: not found", Repos: []*orchdv1.DocRepo{{Repo: "shown"}, {Repo: "hidden"}}}},
			Files: []*orchdv1.DocFile{{Owner: "stranger", Repo: "shown"}},
			Sync:  &orchdv1.DocsSyncState{Running: true, Owner: tt.owner, Repo: tt.repo, Done: 3, Total: 9},
		}
		narrowDocs(out, scope)
		sy := out.GetSync()
		if sy.GetOwner() != tt.wantOwner || sy.GetRepo() != tt.wantRepo || sy.GetTotal() != tt.total || len(out.GetOwners()) != 3 {
			t.Errorf("sync of %s/%s: owners %v, sync %v", tt.owner, tt.repo, out.GetOwners(), sy)
		}
		for _, o := range out.GetOwners() {
			if o.GetOwner() == "stranger" && (o.GetError() != "" || len(o.GetRepos()) != 1) {
				t.Errorf("a partly listed owner kept %v", o)
			}
		}
	}
}

// TestStaleLabelsAreUnreadable is INV-08 on every read path (RC-750): a
// chunk in a readable space whose labels_v is not the applied revision's,
// or that has none, is not read until it is relabelled; the operator
// still sees it, to relabel or retag it.
func TestStaleLabelsAreUnreadable(t *testing.T) {
	e := newReadEnv(t)
	stale := map[string]any{"none": nil, "earlier": e.labels - 1, "later": e.labels + 5}
	for name, v := range stale {
		p := map[string]any{"text": "acme stale note " + name, "source_uri": "note://acme/stale-" + name,
			"source_type": "manual", "chunk_index": 0.0, "space": "acme", "heading": "stale " + name}
		if v != nil {
			p["labels_v"] = v
		}
		e.q.seed("c__acme", "stale-"+name, p)
	}
	e.q.seed("c__acme", "doc-stale", map[string]any{"text": "acme stale readme", "source_uri": "repo://acme/old/README.md",
		"source_type": "repo_doc", "chunk_index": 0.0, "space": "acme", "labels_v": e.labels - 1})
	if err := e.s.docsReg.SetRepoState(context.Background(), docs.RepoState{Owner: "acme", Repo: "old", HeadSHA: sha, Files: 1, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.docsReg.SetHash(context.Background(), "repo://acme/old/README.md", "h", 200); err != nil {
		t.Fatal(err)
	}
	contains := func(got []string, sub string) bool {
		return slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, sub) })
	}
	for name := range stale {
		got := e.readAll(t, inSpace("acme"), "acme stale note "+name, "note://acme/stale-"+name)
		if contains(got, "stale") || contains(got, "repo://acme/old") {
			t.Errorf("acme read a chunk labelled %s: %q", name, got)
		}
		op := e.readAll(t, as(resolvedCaller{kind: callerOperator, operatorBy: operatorApp}), "acme stale note "+name, "note://acme/stale-"+name)
		if !contains(op, "note://acme/stale-"+name) {
			t.Errorf("the operator does not see the chunk labelled %s", name)
		}
	}
	// Relabelled under the applied revision, it is read again.
	e.q.seed("c__acme", "stale-earlier", map[string]any{"text": "acme stale note earlier", "source_uri": "note://acme/stale-earlier",
		"source_type": "manual", "chunk_index": 0.0, "space": "acme", "labels_v": e.labels})
	if got := e.readAll(t, inSpace("acme"), "acme stale note earlier", "note://acme/stale-earlier"); !slices.Contains(got, "note://acme/stale-earlier") {
		t.Error("a relabelled chunk is still unreadable")
	}
}

// TestUnprovenOperatorGetsTaskLimits is RC-707 under D-71 at the
// interceptor: while a policy is applied, a caller only its process tree
// shows to be the operator (a terminal, a pty of its own, a detached
// process) is refused every RPC queued tasks are refused, before the
// handler runs; Orch.app and sessions are not, and with no policy nothing
// changes.
func TestUnprovenOperatorGetsTaskLimits(t *testing.T) {
	e := newReadEnv(t)
	tr := newOperatorTree()
	r := &callerResolver{pins: e.s.pins, needsEvidence: policyApplied(e.s.spaces)}
	ran := 0
	handler := func(context.Context, any) (any, error) { ran++; return nil, nil }
	call := func(pid int, c net.Conn, method string) error {
		ctx := callerCtx(tr.creds(pid, fakeVerifier()).identify(c))
		_, err := r.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		return err
	}
	stream := func(pid int, c net.Conn, method string) error {
		ctx := callerCtx(tr.creds(pid, fakeVerifier()).identify(c))
		return r.stream(nil, fakeStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: method},
			func(any, grpc.ServerStream) error { ran++; return nil })
	}
	terminal, app := signedConn{err: errors.New("not signed")}, appSig("/Applications/Orch.app", codeRuntime, genuineCDHash)
	for _, m := range []string{orchdv1.Orchd_SetConfig_FullMethodName, orchdv1.Orchd_Ingest_FullMethodName,
		orchdv1.Orchd_SetSettingEnabled_FullMethodName, orchdv1.Orchd_Restart_FullMethodName, orchdv1.Orchd_Relabel_FullMethodName} {
		for _, pid := range []int{32, 37, 40} {
			if err := call(pid, terminal, m); grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "only Orch.app") {
				t.Errorf("%s from process %d = %v", m, pid, err)
			}
		}
		if err := call(50, app, m); err != nil {
			t.Errorf("%s from Orch.app = %v", m, err)
		}
	}
	if err := stream(32, terminal, orchdv1.Orchd_WatchTasks_FullMethodName); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("WatchTasks from a terminal = %v", err)
	}
	if err := call(32, terminal, orchdv1.Orchd_Search_FullMethodName); err != nil {
		t.Errorf("Search, which tasks may call, from a terminal = %v", err)
	}
	if err := call(32, terminal, "/orchd.v1.Orchd/Unlisted"); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("an unlisted RPC from a terminal = %v", err)
	}
	ctx := context.WithValue(callerCtx(callerAuth{pid: 900, harness: "claude"}), resolvedKey{}, resolvedCaller{})
	if _, err := r.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: orchdv1.Orchd_Ingest_FullMethodName}, handler); err != nil {
		t.Errorf("Ingest from a session = %v", err)
	}
	r.needsEvidence = policyApplied(nil)
	before := ran
	if err := call(32, terminal, orchdv1.Orchd_SetConfig_FullMethodName); err != nil || ran != before+1 {
		t.Errorf("with no policy SetConfig from a terminal = %v", err)
	}
	if err := stream(32, terminal, orchdv1.Orchd_WatchTasks_FullMethodName); err != nil {
		t.Errorf("with no policy WatchTasks from a terminal = %v", err)
	}
}

// TestRefusedOrchAppIsLogged: an Orch.app that is refused says why in
// orchd's log, once per connection, so a locked-out operator can find out.
func TestRefusedOrchAppIsLogged(t *testing.T) {
	logs := captureReadLog(t)
	for _, tt := range []struct {
		v    *appVerifier
		conn net.Conn
		why  string
	}{
		{fakeVerifier("/Applications/Orch.app", "/home/u/Applications/Orch.app"), appSig("/Applications/Orch.app", codeRuntime, genuineCDHash), "not installed in exactly one"},
		{fakeVerifier(), appSig("/Applications/Orch.app", 0, genuineCDHash), "without the hardened runtime"},
		{fakeVerifier(), appSig("/tmp/x/Orch.app", codeRuntime, genuineCDHash), "runs from /tmp/x/Orch.app"},
		{fakeVerifier(), appSig("/Applications/Orch.app", codeRuntime, []byte{9}), "the bundle changed"},
	} {
		appLogs.last = time.Time{}
		a := &appCheck{v: tt.v, c: tt.conn, pid: 50}
		if a.isApp() || a.isApp() {
			t.Fatalf("%s: recognized", tt.why)
		}
		if n := strings.Count(logs.String(), tt.why); n != 1 {
			t.Errorf("%q logged %d times:\n%s", tt.why, n, logs.String())
		}
	}
	v := fakeVerifier()
	v.static = func(p, _ string) (codeSignature, error) { return codeSignature{Path: p, CDHash: genuineCDHash}, nil }
	if ok, why := v.check(appSig("/Applications/Orch.app", codeRuntime, genuineCDHash)); ok || !strings.Contains(why, "installed bundle was signed without") {
		t.Errorf("bundle without the runtime: %v %q", ok, why)
	}
	if ok, why := fakeVerifier().check(signedConn{err: errors.New("not signed")}); ok || why != "" {
		t.Errorf("a program that is no Orch.app gets a reason: %q", why)
	}
	if (*appCheck)(nil).isApp() {
		t.Fatal("no check is Orch.app")
	}
	// The line is rate-limited and quoted: a flood of refused copies, or a
	// path with a newline, cannot fill or forge orchd's log.
	appLogs.last = time.Time{}
	before := strings.Count(logs.String(), "\n")
	for range 20 {
		(&appCheck{v: fakeVerifier(), c: appSig("/tmp/x\nforged line/Orch.app", codeRuntime, genuineCDHash), pid: 51}).isApp()
	}
	if got := strings.Count(logs.String(), "\n") - before; got != 1 || strings.Contains(logs.String(), "\nforged line") {
		t.Fatalf("%d lines logged for 20 refused copies:\n%s", got, logs.String())
	}
	l := &logLimit{every: time.Minute}
	now := time.Now()
	if !l.allow(now) || l.allow(now.Add(time.Second)) || !l.allow(now.Add(2*time.Minute)) {
		t.Fatal("logLimit")
	}
}

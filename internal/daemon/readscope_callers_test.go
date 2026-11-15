package daemon

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

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

// operatorTree is a fake process table for D-39. launchd is 1.
//
//	Terminal 30 → zsh 31 → orch 32           a command the user typed
//	              zsh 31 → orch 33           a job the user sent to the background
//	claude 400 (in zsh 31's tty, group 400) → orch 41, reparented to launchd
//	wrapper 80 (leads the tty's group, runs claude 81) → orch 82, reparented
//	orch 40 under launchd, no terminal        a double fork that called setsid
//	osascript 61 under launchd → orch 60      started by osascript
//	launchctl's job 70 under launchd          launchctl submit / open
//	Orch.app 50 under launchd                 the app
//	a copy of Orch.app 51 in a temp folder
type operatorTree struct {
	parents map[int]int
	progs   map[int]procstat.Program
	ttys    map[int]procstat.Terminal
}

func newOperatorTree() operatorTree {
	return operatorTree{
		parents: map[int]int{30: 1, 31: 30, 32: 31, 33: 31, 400: 31, 41: 1, 40: 1, 61: 1, 60: 61, 70: 1, 50: 1, 51: 1, 80: 31, 81: 80, 82: 1, 34: 31, 35: 1, 36: 35, 37: 36},
		progs: map[int]procstat.Program{
			30: {Name: "Terminal"}, 31: {Name: "zsh"}, 32: {Name: "orch"}, 33: {Name: "orch"},
			400: {Name: "claude"}, 41: {Name: "orch"}, 40: {Name: "orch"}, 61: {Name: "osascript"},
			60: {Name: "orch"}, 70: {Name: "orch"}, 80: {Name: "caffeinate"}, 81: {Name: "claude"}, 82: {Name: "orch"},
			34: {Name: "orch"}, 35: {Name: "script"}, 36: {Name: "zsh"}, 37: {Name: "orch"},
			50: {Name: "Orch", Path: "/Applications/Orch.app/Contents/MacOS/Orch"},
			51: {Name: "Orch", Path: "/tmp/x/Orch.app/Contents/MacOS/Orch"},
		},
		ttys: map[int]procstat.Terminal{
			32:  {Controlling: true, Group: 32, Foreground: 32},
			33:  {Controlling: true, Group: 33, Foreground: 32},
			41:  {Controlling: true, Group: 400, Foreground: 400},
			40:  {Group: 40},
			60:  {Group: 61},
			70:  {Group: 70},
			50:  {Group: 50},
			51:  {Group: 51},
			400: {Controlling: true, Group: 400, Foreground: 400},
			82:  {Controlling: true, Group: 80, Foreground: 80},
			34:  {Controlling: true, Group: 32, Foreground: 32},
			37:  {Controlling: true, Group: 37, Foreground: 37},
		},
	}
}

func (tr operatorTree) creds(pid int) peerCredentials {
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
		terminal: func(p int) (procstat.Terminal, error) {
			if t, ok := tr.ttys[p]; ok {
				return t, nil
			}
			return procstat.Terminal{}, errors.New("no such process")
		},
		peerPID: func(net.Conn) (int, error) { return pid, nil },
		home:    "/home/u",
	}
}

// TestOperatorReadsNeedEvidence is SP-025.1.a7 (D-39): a caller with no
// harness above it reads every space only when it is a command the user
// runs at a terminal or Orch.app; a process that left its session (a
// double fork, launchctl or open, osascript, a background job) reads
// public memory only.
func TestOperatorReadsNeedEvidence(t *testing.T) {
	e := newReadEnv(t)
	tr := newOperatorTree()
	r := &callerResolver{pins: e.s.pins}
	for _, tt := range []struct {
		name     string
		pid      int
		evidence string
	}{
		{"typed at a terminal", 32, operatorTerminal},
		{"Orch.app", 50, operatorApp},
		{"background job", 33, ""},
		{"left a harness's process group", 41, ""},
		{"double fork with setsid", 40, ""},
		{"started by osascript", 60, ""},
		{"launchctl or open", 70, ""},
		{"a copy of Orch.app elsewhere", 51, ""},
		{"left a wrapper's foreground group", 82, ""},
		{"a pipeline's later command", 34, operatorTerminal},
		// Known gap (RC-705): a terminal an agent opens itself, with
		// script(1) left under launchd, is still evidence.
		{"a pty an agent opened (RC-705)", 37, operatorTerminal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := r.resolve(callerCtx(tr.creds(tt.pid).identify(nil)))
			if c.kind != callerOperator || c.operatorBy != tt.evidence {
				t.Fatalf("resolved %+v, want the operator with evidence %q", c, tt.evidence)
			}
			got := e.readAll(t, as(c), memoText("globex", 3), memoURI("globex", 3))
			if tt.evidence != "" {
				for _, want := range []string{memoURI("globex", 3), memoURI("acme", 3), memoURI("quarantine", 3), memoURI("legacy", 3)} {
					if !slices.Contains(got, want) {
						t.Errorf("the operator does not read every collection: lacks %s", want)
					}
				}
				return
			}
			if l := spaceLeaks([]string{"oss"}, got...); len(l) > 0 {
				t.Fatalf("read beyond public: %q", l[:min(3, len(l))])
			}
		})
	}
	// Reading a terminal fails: no evidence.
	cr := tr.creds(32)
	cr.terminal = func(int) (procstat.Terminal, error) { return procstat.Terminal{}, errors.New("gone") }
	if a := cr.identify(nil); a.operatorBy != "" {
		t.Fatalf("evidence %q from an unreadable terminal", a.operatorBy)
	}
	cr.terminal = nil
	if a := cr.identify(nil); a.operatorBy != "" {
		t.Fatalf("evidence %q without a terminal reader", a.operatorBy)
	}
	// A foreground group whose leader cannot be read, or with no program
	// reader at all, shows nothing either.
	tr.ttys[32] = procstat.Terminal{Controlling: true, Group: 99, Foreground: 99}
	if a := tr.creds(32).identify(nil); a.operatorBy != "" {
		t.Fatalf("evidence %q with an unreadable group leader", a.operatorBy)
	}
	cr = tr.creds(32)
	cr.program = nil
	if ev := cr.operatorEvidence(32); ev != "" {
		t.Fatalf("evidence %q without a program reader", ev)
	}
	// Orch.app in the user's own Applications folder counts too.
	if !(peerCredentials{home: "/home/u"}).isApp("/home/u/Applications/Orch.app/Contents/MacOS/Orch") ||
		(peerCredentials{}).isApp("/home/u/Applications/Orch.app/Contents/MacOS/Orch") ||
		(peerCredentials{}).isApp("Orch.app/Contents/MacOS/Orch") {
		t.Fatal("isApp")
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
	terminal := as(resolvedCaller{kind: callerOperator, operatorBy: operatorTerminal})
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
		{kind: callerOperator}:                          "the operator (no terminal or Orch.app)",
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
		op := e.readAll(t, as(resolvedCaller{kind: callerOperator, operatorBy: operatorTerminal}), "acme stale note "+name, "note://acme/stale-"+name)
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

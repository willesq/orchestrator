package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestSpacesPaneRPCsOperatorOnly is SP-050.1.a1's caller check: the pane's
// policy RPCs, ApplySpaces included, are refused to every caller but the
// operator, and are classed operator-only for queued tasks.
func TestSpacesPaneRPCsOperatorOnly(t *testing.T) {
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	callers := map[string]context.Context{
		"claude session":   callerCtx(callerAuth{pid: 7, harness: "claude"}),
		"codex session":    callerCtx(callerAuth{pid: 7, harness: "codex"}),
		"orchd's child":    callerCtx(callerAuth{pid: 7, spawned: true}),
		"queued task":      queued,
		"unreadable tree":  callerCtx(callerAuth{pid: 7, harnessErr: errors.New("gone")}),
		"unidentified":     callerCtx(callerAuth{err: errPeerUnsupported}),
		"no peer whatever": context.Background(),
	}
	s, code, _ := placementServer(t)
	rpcs := map[string]func(context.Context) error{
		orchdv1.Orchd_ListSpaces_FullMethodName: func(ctx context.Context) error {
			_, err := s.ListSpaces(ctx, &orchdv1.ListSpacesRequest{})
			return err
		},
		orchdv1.Orchd_ScanSpaces_FullMethodName: func(ctx context.Context) error {
			_, err := s.ScanSpaces(ctx, &orchdv1.ScanSpacesRequest{Folders: []string{code}, Offline: true})
			return err
		},
		orchdv1.Orchd_DiffSpaces_FullMethodName: func(ctx context.Context) error {
			_, err := s.DiffSpaces(ctx, &orchdv1.DiffSpacesRequest{})
			return err
		},
		orchdv1.Orchd_ApplySpaces_FullMethodName: func(ctx context.Context) error {
			_, err := s.ApplySpaces(ctx, &orchdv1.ApplySpacesRequest{ReviewedRevision: 1})
			return err
		},
	}
	for method, call := range rpcs {
		if taskCallable[method] || operatorActions[method] == "" {
			t.Errorf("%s is not classed operator-only", method)
		}
		for name, ctx := range callers {
			if err := call(ctx); grpcstatus.Code(err) != codes.PermissionDenied {
				t.Errorf("%s from %s = %v, want PermissionDenied", method, name, err)
			}
		}
		if err := call(operatorCtx); grpcstatus.Code(err) == codes.PermissionDenied {
			t.Errorf("%s refused the operator: %v", method, err)
		}
	}
}

// TestListSpaces is SP-050.1.a1 for ListSpaces: every applied space with its
// bindings and what it may read, plus the policy's status.
func TestListSpaces(t *testing.T) {
	s, code, _ := placementServer(t)
	resp, err := s.ListSpaces(operatorCtx, &orchdv1.ListSpacesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st := resp.GetStatus(); !st.GetConfigured() || st.GetAppliedRevision() != 1 {
		t.Errorf("status = %v", st)
	}
	if resp.GetPublicRootSessions() != "personal" {
		t.Errorf("public_root_sessions = %q", resp.GetPublicRootSessions())
	}
	var names []string
	byName := map[string]*orchdv1.SpaceInfo{}
	for _, sp := range resp.GetSpaces() {
		names = append(names, sp.GetName())
		byName[sp.GetName()] = sp
	}
	if want := []string{"acme", "globex", "oss", "personal"}; !slices.Equal(names, want) {
		t.Fatalf("spaces = %v, want %v", names, want)
	}
	acme := byName["acme"]
	if acme.GetKind() != "isolated" || !slices.Equal(acme.GetGithubOrgs(), []string{"acme"}) ||
		!slices.Equal(acme.GetRoots(), []string{code + "/company1"}) || !slices.Equal(acme.GetChat(), []string{"slack:TACME"}) {
		t.Errorf("acme = %v", acme)
	}
	if got := byName["oss"].GetWebPrefixes(); !slices.Equal(got, []string{"https://docs.oss.dev/"}) {
		t.Errorf("oss web prefixes = %v", got)
	}
	for name, want := range map[string][]string{
		"acme":     {"acme", "oss"},
		"globex":   {"globex", "oss"},
		"oss":      {"oss"},
		"personal": {"oss", "personal"},
	} {
		if got := byName[name].GetReadable(); !slices.Equal(got, want) {
			t.Errorf("%s readable = %v, want %v", name, got, want)
		}
	}
}

// TestListSpacesWithoutPolicy: nothing applied lists no spaces, and a
// policy that does not verify reports why instead of failing.
func TestListSpacesWithoutPolicy(t *testing.T) {
	s := spacesServer(t)
	resp, err := s.ListSpaces(operatorCtx, &orchdv1.ListSpacesRequest{})
	if err != nil || len(resp.GetSpaces()) != 0 || resp.GetStatus().GetConfigured() {
		t.Fatalf("ListSpaces = %v, %v", resp, err)
	}
	s.spaces = nil
	if _, err := s.ListSpaces(operatorCtx, &orchdv1.ListSpacesRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no engine: %v", err)
	}
	if _, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no engine: %v", err)
	}
}

// TestListSpacesBrokenPolicy: an applied revision edited in orch.db keeps
// the enforced spaces listed with drift; once orchd restarts on it, nothing
// verifies, so no space is listed and the status says why.
func TestListSpacesBrokenPolicy(t *testing.T) {
	s, _, _ := placementServer(t)
	conn := spacesDB(t)
	if _, err := conn.Exec(`UPDATE spaces_revisions SET doc = '{"spaces":[]}'`); err != nil {
		t.Fatal(err)
	}
	resp, err := s.ListSpaces(operatorCtx, &orchdv1.ListSpacesRequest{})
	if err != nil || len(resp.GetSpaces()) != 4 || !slices.Contains(resp.GetStatus().GetDrift(), "changed_outside_orchd") {
		t.Fatalf("edited revision = %v, %v", resp, err)
	}
	e, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	s.spaces = e
	resp, err = s.ListSpaces(operatorCtx, &orchdv1.ListSpacesRequest{})
	if err != nil || len(resp.GetSpaces()) != 0 || resp.GetStatus().GetError() == "" {
		t.Errorf("broken policy = %v, %v", resp, err)
	}
	if _, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("scan under a broken policy: %v", err)
	}
}

// TestScanSpaces is SP-050.1.a1 for ScanSpaces over a real tree: it finds
// checkouts to the depth asked, without entering hidden folders, symbolic
// links or a checkout's own folders, and places each as orchd would.
func TestScanSpaces(t *testing.T) {
	s, code, _ := placementServer(t)
	outside := t.TempDir()
	checkout(t, outside+"/linked", "acme", "app")
	if err := os.Symlink(outside, code+"/link"); err != nil {
		t.Fatal(err)
	}
	checkout(t, code+"/.hidden/repo", "acme", "app")
	checkout(t, code+"/a/b/c/d/deep", "acme", "app")
	checkout(t, code+"/lib/vendor/inner", "acme", "app") // inside a checkout
	if err := os.MkdirAll(code+"/anon", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(code+"/anon/.git", []byte("gitdir: /nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code, code + "/", code + "/missing"}, Offline: false})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*orchdv1.ScannedCheckout{}
	var paths []string
	for _, c := range resp.GetCheckouts() {
		rel := strings.TrimPrefix(c.GetPath(), code+"/")
		got[rel] = c
		paths = append(paths, rel)
	}
	want := []string{"anon", "company1/app", "company1/fork", "company1/lib", "company1/x", "lib", "moved", "probe-bound", "probe-unbound", "secret", "willesq/app"}
	if !slices.Equal(paths, want) {
		t.Fatalf("checkouts = %v, want %v", paths, want)
	}
	for rel, w := range map[string][4]string{ // owner, visibility, space, outcome
		"company1/app":  {"acme", "private", "acme", "placed"},
		"lib":           {"oss-org", "public", "oss", "placed"},
		"moved":         {"acme", "private", "acme", "placed"},
		"probe-unbound": {"stranger", "public", "oss", "placed"},
		"anon":          {"", "", "oss", "placed"},
	} {
		c := got[rel]
		if c.GetOwner() != w[0] || c.GetVisibility() != w[1] || c.GetSpace() != w[2] || c.GetOutcome() != w[3] || c.GetRule() == "" {
			t.Errorf("%s = %v, want %v", rel, c, w)
		}
	}
	if !got["anon"].GetAnonymous() || got["lib"].GetAnonymous() {
		t.Error("anonymous checkout not marked, or a real one marked")
	}
	if len(resp.GetProblems()) != 1 || !strings.Contains(resp.GetProblems()[0], "missing: does not exist") {
		t.Errorf("problems = %v", resp.GetProblems())
	}
	if resp.GetTruncated() {
		t.Error("a small tree was truncated")
	}

	deep, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code + "/a"}, MaxDepth: 4, Offline: true})
	if err != nil || len(deep.GetCheckouts()) != 1 || deep.GetCheckouts()[0].GetPath() != code+"/a/b/c/d/deep" {
		t.Errorf("depth 4 = %v, %v", deep, err)
	}
	shallow, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code + "/a"}, Offline: true})
	if err != nil || len(shallow.GetCheckouts()) != 0 {
		t.Errorf("default depth = %v, %v", shallow, err)
	}
	direct, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{outside + "/linked"}, Offline: true})
	if err != nil || len(direct.GetCheckouts()) != 1 {
		t.Errorf("a folder that is a checkout = %v, %v", direct, err)
	}
}

// TestScanSpacesWritesNothing is SP-050.1.a2: a scan over a read-only fake
// filesystem and a fake store reads both and writes neither.
func TestScanSpacesWritesNothing(t *testing.T) {
	tree := fstest.MapFS{
		"code/app/.git/config":    {Data: []byte("[remote \"origin\"]\n\turl = https://github.com/acme/app\n")},
		"code/app/main.go":        {Data: []byte("package main\n")},
		"code/notes/readme.md":    {Data: []byte("hi\n")},
		"code/lib/.git/HEAD":      {Data: []byte("ref: refs/heads/main\n")},
		"code/.cache/x/.git/HEAD": {Data: []byte("ref: refs/heads/main\n")},
	}
	before := cloneMapFS(tree)
	fsys := &recordingFS{fs: tree}
	store := &fakeSpacesStore{res: resolvedFor(t, `{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"],"roots":["/r/code/app"]}]}`)}
	var lookups []string
	sc := &spaceScanner{
		fsys: func(dir string) (fs.FS, func(), error) {
			sub, err := fs.Sub(fsys, strings.TrimPrefix(dir, "/r/"))
			return sub, func() {}, err
		},
		checkoutAt: func(dir string) (handoff.Checkout, bool) {
			if strings.HasSuffix(dir, "/app") {
				return handoff.Checkout{Root: dir, Owner: "acme", Repo: "app"}, true
			}
			return handoff.Checkout{Root: dir}, true
		},
		placer: store,
		repo: func(_ context.Context, owner, name string) *spaces.Repo {
			lookups = append(lookups, owner+"/"+name)
			return &spaces.Repo{Owner: owner, Name: name, Visibility: spaces.VisibilityPrivate}
		},
		account: func(_ context.Context, dir string) (string, []string, error) {
			return "me", []string{"acme"}, nil
		},
	}
	resp, err := sc.scan(context.Background(), &orchdv1.ScanSpacesRequest{Folders: []string{"/r/code"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.GetCheckouts()); n != 2 || resp.GetCheckouts()[0].GetSpace() != "acme" || resp.GetCheckouts()[1].GetSpace() != spaces.Quarantine {
		t.Fatalf("checkouts = %v", resp.GetCheckouts())
	}
	if !slices.Equal(lookups, []string{"acme/app"}) {
		t.Errorf("repo lookups = %v", lookups)
	}
	if fsys.opens == 0 || store.reads == 0 {
		t.Fatalf("the scan read nothing: %d opens, %d store reads", fsys.opens, store.reads)
	}
	if store.writes != 0 {
		t.Errorf("the scan wrote to the store %d times", store.writes)
	}
	if !maps.EqualFunc(tree, before, func(a, b *fstest.MapFile) bool { return string(a.Data) == string(b.Data) && a.Mode == b.Mode }) {
		t.Error("the scan changed the filesystem")
	}
	if len(resp.GetAccounts()) != 1 || resp.GetAccounts()[0].GetLogin() != "me" {
		t.Errorf("accounts = %v", resp.GetAccounts())
	}
}

// TestScanSpacesLeavesDiskAndDBAlone runs ScanSpaces through the server over
// a real tree and orch.db, and compares both before and after.
func TestScanSpacesLeavesDiskAndDBAlone(t *testing.T) {
	s, code, _ := placementServer(t)
	conn := spacesDB(t)
	disk, db := treeDigest(t, code), dbDigest(t, conn)
	if _, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code}}); err != nil {
		t.Fatal(err)
	}
	if treeDigest(t, code) != disk {
		t.Error("ScanSpaces changed the scanned tree")
	}
	if dbDigest(t, conn) != db {
		t.Error("ScanSpaces changed orch.db")
	}
}

func TestScanSpacesRequests(t *testing.T) {
	s, code, _ := placementServer(t)
	many := make([]string, maxScanFolders+1)
	for i := range many {
		many[i] = fmt.Sprintf("/f%d", i)
	}
	for name, req := range map[string]*orchdv1.ScanSpacesRequest{
		"no folders":     {},
		"relative":       {Folders: []string{"code"}},
		"nul":            {Folders: []string{"/a\x00b"}},
		"too many":       {Folders: many},
		"negative depth": {Folders: []string{code}, MaxDepth: -1},
	} {
		if _, err := s.ScanSpaces(operatorCtx, req); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	deep := code + "/1/2/3/4/5/6/7/eight" // seven folders below the scanned one
	checkout(t, deep, "acme", "app")
	checkout(t, code+"/1/2/3/4/5/6/six", "acme", "app")
	resp, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code + "/1"}, MaxDepth: 99, Offline: true})
	if err != nil || len(resp.GetCheckouts()) != 1 || !strings.HasSuffix(resp.GetCheckouts()[0].GetPath(), "/six") {
		t.Errorf("depth cap = %v, %v", resp, err)
	}
	ctx, cancel := context.WithCancel(operatorCtx)
	cancel()
	if _, err := s.ScanSpaces(ctx, &orchdv1.ScanSpacesRequest{Folders: []string{code}}); grpcstatus.Code(err) != codes.Canceled {
		t.Errorf("cancelled: %v", err)
	}
}

func TestScanSpacesLimits(t *testing.T) {
	s, code, _ := placementServer(t)
	for name, set := range map[string]func(){
		"folders":   func() { maxScanDirs = 3 },
		"checkouts": func() { maxScanCheckouts = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			dirs, outs := maxScanDirs, maxScanCheckouts
			t.Cleanup(func() { maxScanDirs, maxScanCheckouts = dirs, outs })
			set()
			resp, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code}, Offline: true})
			if err != nil || !resp.GetTruncated() || len(resp.GetCheckouts()) > 2 {
				t.Errorf("ScanSpaces = %v, %v", resp, err)
			}
		})
	}
}

// TestScanSpacesAccounts: one entry for the default login and one per space
// profile directory, shared directories named once; offline lists none.
func TestScanSpacesAccounts(t *testing.T) {
	res := resolvedFor(t, `{"spaces":[
		{"name":"acme","kind":"isolated","github_orgs":["acme"],"gh_config_dir":"/p/acme"},
		{"name":"globex","kind":"isolated","github_orgs":["globex"],"gh_config_dir":"/p/globex"},
		{"name":"personal","kind":"private","github_orgs":["willesq"]}]}`)
	sc := &spaceScanner{
		fsys:       fixedFS(fstest.MapFS{}),
		checkoutAt: func(string) (handoff.Checkout, bool) { return handoff.Checkout{}, false },
		placer:     &fakeSpacesStore{res: res},
		account: func(_ context.Context, dir string) (string, []string, error) {
			if dir == "/p/globex" {
				return "", nil, errors.New("not logged in")
			}
			return "login" + dir, []string{"org" + dir}, nil
		},
	}
	resp, err := sc.scan(context.Background(), &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range resp.GetAccounts() {
		got = append(got, fmt.Sprintf("%s|%s|%v|%s", a.GetProfile(), a.GetLogin(), a.GetOrgs(), a.GetError()))
	}
	want := []string{"|login|[org]|", "acme|login/p/acme|[org/p/acme]|", "globex||[]|not logged in"}
	if !slices.Equal(got, want) {
		t.Errorf("accounts = %v, want %v", got, want)
	}
	offline, err := sc.scan(context.Background(), &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}, Offline: true})
	if err != nil || len(offline.GetAccounts()) != 0 {
		t.Errorf("offline = %v, %v", offline, err)
	}
	sc.account = nil
	if r, _ := sc.scan(context.Background(), &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}}); len(r.GetAccounts()) != 0 {
		t.Errorf("no account checker: %v", r.GetAccounts())
	}
}

func TestScanSpacesStoreErrors(t *testing.T) {
	sc := &spaceScanner{
		fsys:       fixedFS(fstest.MapFS{"r/.git/HEAD": {}}),
		checkoutAt: func(string) (handoff.Checkout, bool) { return handoff.Checkout{}, false },
		placer:     &fakeSpacesStore{err: errors.New("broken")},
	}
	req := &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}}
	if _, err := sc.scan(context.Background(), req); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("unreadable policy: %v", err)
	}
	sc.placer = &fakeSpacesStore{placeErr: errors.New("db gone")}
	sc.fsys = fixedFS(fstest.MapFS{".git/HEAD": {}})
	if _, err := sc.scan(context.Background(), req); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("placement error: %v", err)
	}
}

func TestScanErr(t *testing.T) {
	for err, want := range map[error]string{
		fs.ErrNotExist:   "does not exist",
		fs.ErrPermission: "permission denied",
		&fs.PathError{Op: "open", Path: "/secret/x", Err: errors.New("i/o")}: "i/o",
		errors.New("odd"): "odd",
	} {
		if got := scanErr(err); got != want {
			t.Errorf("scanErr(%v) = %q, want %q", err, got, want)
		}
	}
}

// fixedFS serves every folder from fsys.
func fixedFS(fsys fs.FS) func(string) (fs.FS, func(), error) {
	return func(string) (fs.FS, func(), error) { return fsys, func() {}, nil }
}

// failingFS fails every open.
type failingFS struct{ err error }

func (f failingFS) Open(string) (fs.File, error) { return nil, f.err }

// TestScanSpacesUnreadableFolder: a chosen folder that cannot be opened or
// listed is a problem, not an error, and the rest of the scan goes on.
func TestScanSpacesUnreadableFolder(t *testing.T) {
	sc := &spaceScanner{
		fsys: func(dir string) (fs.FS, func(), error) {
			if dir == "/locked" {
				return failingFS{fs.ErrPermission}, func() {}, nil
			}
			return osDirFS(dir)
		},
		checkoutAt: func(string) (handoff.Checkout, bool) { return handoff.Checkout{}, false },
		placer:     &fakeSpacesStore{},
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := sc.scan(context.Background(), &orchdv1.ScanSpacesRequest{Folders: []string{"/locked", file}, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if p := resp.GetProblems(); len(p) != 2 || p[0] != "/locked: permission denied" || !strings.HasPrefix(p[1], file+": ") {
		t.Errorf("problems = %v", p)
	}
}

// TestScanSpacesOneAtATime: a second scan while one runs is refused.
func TestScanSpacesOneAtATime(t *testing.T) {
	s, code, _ := placementServer(t)
	scanSlot <- struct{}{}
	_, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code}, Offline: true})
	<-scanSlot
	if grpcstatus.Code(err) != codes.Aborted {
		t.Fatalf("concurrent scan = %v, want Aborted", err)
	}
	if _, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code}, Offline: true}); err != nil {
		t.Errorf("scan after the first ended: %v", err)
	}
}

// TestScanSpacesLookupBudget: one scan asks GitHub about at most
// maxScanLookups repos; the rest answer from the cache.
func TestScanSpacesLookupBudget(t *testing.T) {
	tree := fstest.MapFS{}
	for i := 0; i < maxScanLookups+5; i++ {
		tree[fmt.Sprintf("r%02d/.git/HEAD", i)] = &fstest.MapFile{}
	}
	live := 0
	sc := &spaceScanner{
		fsys: fixedFS(tree),
		checkoutAt: func(dir string) (handoff.Checkout, bool) {
			return handoff.Checkout{Owner: "acme", Repo: filepath.Base(dir)}, true
		},
		placer: &fakeSpacesStore{},
		repo: func(ctx context.Context, owner, name string) *spaces.Repo {
			if ctx.Value(cachedRepoLookupsKey{}) == nil {
				live++
			}
			return &spaces.Repo{Owner: owner, Name: name}
		},
	}
	resp, err := sc.scan(context.Background(), &orchdv1.ScanSpacesRequest{Folders: []string{"/x"}, MaxDepth: 1})
	if err != nil || len(resp.GetCheckouts()) != maxScanLookups+5 {
		t.Fatalf("scan = %v, %v", resp, err)
	}
	if live != maxScanLookups {
		t.Errorf("%d live lookups, want %d", live, maxScanLookups)
	}
}

// recordingFS counts reads of a read-only filesystem. fs.FS has no write
// method, so the scan cannot write through it.
type recordingFS struct {
	fs    fs.FS
	opens int
}

func (r *recordingFS) Open(name string) (fs.File, error) {
	r.opens++
	return r.fs.Open(name)
}

// fakeSpacesStore stands in for the spaces engine. It has the engine's write
// methods too, counting any call, so a scan that reached them would show.
type fakeSpacesStore struct {
	res      *spaces.Resolved
	err      error
	placeErr error
	reads    int
	writes   int
}

func (f *fakeSpacesStore) Current() (*spaces.Resolved, error) {
	f.reads++
	return f.res, f.err
}

func (f *fakeSpacesStore) PlaceUnder(_ context.Context, res *spaces.Resolved, o spaces.Origin) (spaces.Placement, error) {
	f.reads++
	if f.placeErr != nil {
		return spaces.Placement{}, f.placeErr
	}
	return spaces.Classify(o, res, spaces.Options{Confirmed: func(string) bool { return true }}), nil
}

func (f *fakeSpacesStore) SaveDraft(context.Context, spaces.Policy, string, bool) (spaces.Revision, spaces.Problems, error) {
	f.writes++
	return spaces.Revision{}, nil, nil
}

func (f *fakeSpacesStore) Apply(context.Context, int64, string, bool, []string) (spaces.Revision, error) {
	f.writes++
	return spaces.Revision{}, nil
}

// resolvedFor validates doc as a policy, failing the test on any problem.
func resolvedFor(t *testing.T, doc string) *spaces.Resolved {
	t.Helper()
	p, err := spaces.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	res, err := spaces.Validate(p, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func cloneMapFS(m fstest.MapFS) fstest.MapFS {
	out := fstest.MapFS{}
	for k, v := range m {
		c := *v
		c.Data = slices.Clone(v.Data)
		out[k] = &c
	}
	return out
}

// treeDigest hashes every entry under root: path, mode, size, time and
// content, without following links.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %v %d %d\n", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// spacesDB opens a second connection to the orch.db spacesServer made.
func spacesDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// dbDigest hashes every row of every table.
func dbDigest(t *testing.T, conn *sql.DB) string {
	t.Helper()
	rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	h := sha256.New()
	for _, tbl := range tables {
		r, err := conn.Query(`SELECT * FROM "` + tbl + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(vals)
			fmt.Fprintf(h, "%s %s\n", tbl, b)
		}
		_ = r.Close()
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestScanSpacesReadsCachedAccounts: the server's scanner lists the default
// login from the gh profiles' account check.
func TestScanSpacesReadsCachedAccounts(t *testing.T) {
	s, code, _ := placementServer(t)
	s.ghProfiles = &ghProfiles{
		run: func(context.Context, []string, ...string) ([]byte, error) { return nil, errors.New("not logged in") },
		now: time.Now,
		checked: map[string]profileCheck{
			defaultProfile: {login: "me", orgs: []string{"acme", "oss-org"}, at: time.Now()},
		},
	}
	resp, err := s.ScanSpaces(operatorCtx, &orchdv1.ScanSpacesRequest{Folders: []string{code}, MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetAccounts(); len(a) != 1 || a[0].GetLogin() != "me" || !slices.Equal(a[0].GetOrgs(), []string{"acme", "oss-org"}) {
		t.Errorf("accounts = %v", a)
	}
}

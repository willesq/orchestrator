package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/mdwalk"
	"github.com/willesq/orchestrator/internal/rag"
)

func TestRegistryMarkDue(t *testing.T) {
	tests := []struct {
		name string
		all  bool
	}{
		{name: "mark specific due", all: false},
		{name: "mark all due", all: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newRegistry(t)
			ctx := context.Background()
			_ = reg.Add(ctx, Source{Kind: KindWeb, URI: "https://a", RefreshSecs: 100})
			_ = reg.MarkFetched(ctx, ID(KindWeb, "https://a"), 1000, "ok") // fresh
			if due, _ := reg.Due(ctx, 1000); len(due) != 0 {
				t.Fatalf("should not be due yet")
			}
			id := ""
			if !tt.all {
				id = ID(KindWeb, "https://a")
			}
			if err := reg.MarkDue(ctx, id); err != nil {
				t.Fatalf("MarkDue: %v", err)
			}
			if due, _ := reg.Due(ctx, 1000); len(due) != 1 {
				t.Errorf("expected 1 due after MarkDue")
			}
		})
	}
}

// fakeIngester records IngestText calls.
type fakeIngester struct {
	calls int
	last  rag.Meta
	uris  []string
	err   error
}

func (f *fakeIngester) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	f.calls++
	f.last = m
	f.uris = append(f.uris, m.SourceURI)
	return 1, f.err
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewRegistry(conn)
}

func TestRegistryAddListRemove(t *testing.T) {
	tests := []struct {
		name string
		src  Source
	}{
		{name: "web source", src: Source{Kind: KindWeb, URI: "https://x", RefreshSecs: 3600, Tags: []string{"a", "b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newRegistry(t)
			ctx := context.Background()
			if err := reg.Add(ctx, tt.src); err != nil {
				t.Fatalf("Add: %v", err)
			}
			// idempotent re-add
			if err := reg.Add(ctx, tt.src); err != nil {
				t.Fatalf("re-Add: %v", err)
			}
			list, err := reg.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("list = %d, want 1", len(list))
			}
			if len(list[0].Tags) != 2 {
				t.Errorf("tags not round-tripped: %v", list[0].Tags)
			}
			if err := reg.Remove(ctx, list[0].ID); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			list, _ = reg.List(ctx)
			if len(list) != 0 {
				t.Errorf("after remove list = %d, want 0", len(list))
			}
		})
	}
}

func TestRegistryDueAndMark(t *testing.T) {
	tests := []struct {
		name        string
		lastFetched int64
		refreshSecs int64
		now         int64
		wantDue     bool
	}{
		{name: "overdue", lastFetched: 0, refreshSecs: 100, now: 200, wantDue: true},
		{name: "fresh", lastFetched: 150, refreshSecs: 100, now: 200, wantDue: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newRegistry(t)
			ctx := context.Background()
			s := Source{Kind: KindWeb, URI: "https://x", RefreshSecs: tt.refreshSecs, LastFetched: tt.lastFetched}
			_ = reg.Add(ctx, s)
			// Add stores last_fetched from the struct only on insert.
			_ = reg.MarkFetched(ctx, ID(KindWeb, "https://x"), tt.lastFetched, "ok")
			due, err := reg.Due(ctx, tt.now)
			if err != nil {
				t.Fatalf("Due: %v", err)
			}
			if (len(due) > 0) != tt.wantDue {
				t.Errorf("due = %v, want %v", len(due) > 0, tt.wantDue)
			}
		})
	}
}

func TestRefreshWeb(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		html       bool
		wantStatus string
		wantIngest bool
	}{
		{name: "ok text", status: 200, wantStatus: "ok", wantIngest: true},
		{name: "html stripped", status: 200, html: true, wantStatus: "ok", wantIngest: true},
		{name: "404 flags missing, no ingest", status: 404, wantStatus: "missing", wantIngest: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.html {
					w.Header().Set("Content-Type", "text/html")
				}
				w.WriteHeader(tt.status)
				if tt.html {
					_, _ = w.Write([]byte("<html><body>hello world</body></html>"))
				} else {
					_, _ = w.Write([]byte("hello world"))
				}
			}))
			defer srv.Close()

			ing := &fakeIngester{}
			s := Source{Kind: KindWeb, URI: srv.URL}
			status, err := Refresh(context.Background(), s, ing, RefreshOpts{})
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if (ing.calls > 0) != tt.wantIngest {
				t.Errorf("ingest calls = %d, wantIngest %v", ing.calls, tt.wantIngest)
			}
		})
	}
}

func TestRefreshOwnerAndRepo(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		wantStatus string
		wantTag    string
	}{
		{name: "owner walks repos", kind: KindOwner, wantStatus: "ok", wantTag: "owner:willesq"},
		{name: "repo walks md", kind: KindRepo, wantStatus: "ok", wantTag: "repo:proj"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "willesq", "proj")
			if err := os.MkdirAll(repo, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# doc"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			ing := &fakeIngester{}
			var s Source
			var opts RefreshOpts
			if tt.kind == KindOwner {
				s = Source{Kind: KindOwner, URI: "willesq"}
				opts = RefreshOpts{KnownRoots: []string{root}}
			} else {
				s = Source{Kind: KindRepo, URI: repo, Owner: "willesq"}
			}
			status, err := Refresh(context.Background(), s, ing, opts)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if ing.calls == 0 {
				t.Fatalf("nothing ingested")
			}
			found := false
			for _, tag := range ing.last.Tags {
				if tag == tt.wantTag {
					found = true
				}
			}
			if !found {
				t.Errorf("expected tag %q in %v", tt.wantTag, ing.last.Tags)
			}
		})
	}
}

func TestRefreshMissing(t *testing.T) {
	tests := []struct {
		name string
		src  Source
		opts RefreshOpts
	}{
		{name: "owner not found", src: Source{Kind: KindOwner, URI: "nobody"}, opts: RefreshOpts{KnownRoots: []string{t.TempDir()}}},
		{name: "repo not found", src: Source{Kind: KindRepo, URI: "/nonexistent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := Refresh(context.Background(), tt.src, &fakeIngester{}, tt.opts)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if status != "missing" {
				t.Errorf("status = %q, want missing", status)
			}
		})
	}
}

func TestRefreshUnknownKind(t *testing.T) {
	tests := []struct{ name string }{{name: "unknown kind errors"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Refresh(context.Background(), Source{Kind: "bogus"}, &fakeIngester{}, RefreshOpts{}); err == nil {
				t.Errorf("expected error for unknown kind")
			}
		})
	}
}

func TestSchedulerScan(t *testing.T) {
	tests := []struct{ name string }{{name: "scan refreshes due sources"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newRegistry(t)
			ctx := context.Background()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("content"))
			}))
			defer srv.Close()
			_ = reg.Add(ctx, Source{Kind: KindWeb, URI: srv.URL, RefreshSecs: 10})

			ing := &fakeIngester{}
			sc := NewScheduler(reg, ing, RefreshOpts{})
			const now int64 = 1_000_000
			sc.now = func() int64 { return now } // far in the future → due
			sc.Scan(ctx)

			if ing.calls == 0 {
				t.Errorf("scheduler did not refresh the due source")
			}
			list, _ := reg.List(ctx)
			if list[0].Status != "ok" || list[0].LastFetched != now {
				t.Errorf("source not marked fetched: %+v", list[0])
			}
		})
	}
}

func TestNewSchedulerDefaultClock(t *testing.T) {
	tests := []struct{ name string }{{name: "default clock returns wall-clock unix seconds"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := NewScheduler(nil, nil, RefreshOpts{})
			if got := sc.now(); got < 1_700_000_000 {
				t.Errorf("default clock returned %d, want a current unix timestamp", got)
			}
		})
	}
}

func TestSchedulerScanRegistryError(t *testing.T) {
	tests := []struct{ name string }{{name: "an unreadable registry is a silent no-op"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			conn, err := db.Open()
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			_ = conn.Close() // queries now fail

			ing := &fakeIngester{}
			sc := NewScheduler(NewRegistry(conn), ing, RefreshOpts{})
			sc.Scan(context.Background()) // must not panic
			if ing.calls != 0 {
				t.Errorf("ingested %d times against a dead registry, want 0", ing.calls)
			}
		})
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "strips tags", in: "<p>hi <b>there</b></p>", want: "hi there"},
		{name: "plain text", in: "just text", want: "just text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripHTML(tt.in); got != tt.want {
				t.Errorf("stripHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRefreshSkipsExcludedDirs(t *testing.T) {
	root := resolve(t.TempDir())
	repo := filepath.Join(root, "willesq", "tui")
	for _, rel := range []string{"README.md", "docs/arch.md", "vendor/github.com/charmbracelet/lipgloss/README.md", "node_modules/x/README.md"} {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# doc"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		src  Source
		opts RefreshOpts
	}{
		{name: "repo source", src: Source{Kind: KindRepo, URI: repo, Owner: "willesq"}, opts: RefreshOpts{ExcludeDirs: mdwalk.DefaultExcludeDirs}},
		{name: "owner source", src: Source{Kind: KindOwner, URI: "willesq"}, opts: RefreshOpts{KnownRoots: []string{root}, ExcludeDirs: mdwalk.DefaultExcludeDirs}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ing := &fakeIngester{}
			if _, err := Refresh(context.Background(), tt.src, ing, tt.opts); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			slices.Sort(ing.uris)
			want := []string{CheckoutURI(filepath.Join(repo, "README.md")), CheckoutURI(filepath.Join(repo, "docs", "arch.md"))}
			if !slices.Equal(ing.uris, want) {
				t.Errorf("ingested %v, want %v", ing.uris, want)
			}
		})
	}
}

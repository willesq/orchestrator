package sources

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

// spacedStore is a store that knows each stored doc's space and logs every
// write and delete in order.
type spacedStore struct {
	pruningIngester
	space     map[string]string
	log       []string
	deleteErr error
}

func (s *spacedStore) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	s.log = append(s.log, "write "+m.SourceURI)
	return 1, nil
}

func (s *spacedStore) Copy(_ context.Context, uri string) (rag.StoredCopy, bool, error) {
	sp, ok := s.space[uri]
	return rag.StoredCopy{Writer: rag.WriterSources, Space: sp}, ok, nil
}

func (s *spacedStore) DeleteSource(_ context.Context, uri string) (int, error) {
	s.log = append(s.log, "delete "+uri)
	return 1, s.deleteErr
}

// TestCheckoutNoLongerPublicLeavesFirst is SP-030.1.a2 for the sources
// refresh: before an owner source walks any repo, the docs it stored in a
// public space that would not be public now (lib's repo turned private) are
// deleted, even when lib itself is left as last indexed (off its default
// branch), and nothing else is touched.
func TestCheckoutNoLongerPublicLeavesFirst(t *testing.T) {
	root := resolve(t.TempDir())
	aaa, lib := filepath.Join(root, "oss", "aaa"), filepath.Join(root, "oss", "lib")
	write(t, filepath.Join(aaa, "README.md"), "# aaa")
	write(t, filepath.Join(lib, "README.md"), "# lib")
	gitRepo(t, aaa, "main", "main")
	gitRepo(t, lib, "feature", "main")
	aaaDoc, libDoc := CheckoutURI(filepath.Join(aaa, "README.md")), CheckoutURI(filepath.Join(lib, "README.md"))
	st := &spacedStore{space: map[string]string{aaaDoc: "oss", libDoc: "oss"}}
	st.stored = map[string]string{aaaDoc: rag.WriterSources, libDoc: rag.WriterSources}
	var asked []rag.Meta
	opts := RefreshOpts{KnownRoots: []string{root}, LeavesPublic: func(_ context.Context, stored string, m rag.Meta) bool {
		asked = append(asked, m)
		return stored == "oss" && strings.Contains(m.Origin.Path, "/lib/")
	}}
	status, err := Refresh(context.Background(), Source{Kind: KindOwner, URI: "oss"}, st, opts)
	if err != nil || !strings.HasPrefix(status, "ok; skipped 1: lib") {
		t.Fatalf("refresh = %q, %v", status, err)
	}
	if want := []string{"delete " + libDoc, "write " + aaaDoc}; !slices.Equal(st.log, want) {
		t.Fatalf("order = %q, want %q", st.log, want)
	}
	for _, m := range asked {
		if m.Writer != rag.WriterSources || m.Origin != (rag.Origin{System: rag.OriginFS, Path: strings.TrimPrefix(m.SourceURI, "checkout://")}) {
			t.Fatalf("asked about %+v", m)
		}
	}

	// A repo source is checked the same way.
	st.log = nil
	if _, err := Refresh(context.Background(), Source{Kind: KindRepo, URI: lib}, st, opts); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.log, []string{"delete " + libDoc}) {
		t.Fatalf("repo source = %q", st.log)
	}
}

// TestLeavePublicSourcesEdges: no check, a store without copies, a listing
// that fails, a doc not found, a failed delete and a canceled refresh delete
// nothing more than they should.
func TestLeavePublicSourcesEdges(t *testing.T) {
	ctx := context.Background()
	prefix := "checkout:///r/"
	always := RefreshOpts{LeavesPublic: func(context.Context, string, rag.Meta) bool { return true }}
	st := &spacedStore{space: map[string]string{"checkout:///r/a.md": "oss"}, deleteErr: errors.New("store down")}
	st.stored = map[string]string{"checkout:///r/a.md": rag.WriterSources, "checkout:///r/gone.md": rag.WriterSources}

	leavePublic(ctx, prefix, st, RefreshOpts{})
	leavePublic(ctx, prefix, &originIng{}, always)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	leavePublic(canceled, prefix, st, always)
	if len(st.log) != 0 {
		t.Fatalf("deleted %q", st.log)
	}
	leavePublic(ctx, prefix, st, always)
	if !slices.Equal(st.log, []string{"delete checkout:///r/a.md"}) {
		t.Fatalf("log = %q", st.log)
	}
	st.log, st.listErr = nil, errors.New("list failed")
	leavePublic(ctx, prefix, st, always)
	if len(st.log) != 0 {
		t.Fatalf("a failed listing deleted %q", st.log)
	}
}

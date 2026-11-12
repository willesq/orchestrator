package sources

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

type originIng struct{ metas []rag.Meta }

func (o *originIng) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	o.metas = append(o.metas, m)
	return 1, nil
}

// TestRepoRefreshReportsOriginAndRechecksOwner: a checkout's docs carry their
// path as origin, and the org its origin names is re-checked before each
// repo of an owner source is written (SP-023.1.a1, a4).
func TestRepoRefreshReportsOriginAndRechecksOwner(t *testing.T) {
	root := resolve(t.TempDir())
	good := filepath.Join(root, "acme", "good")
	plain := filepath.Join(root, "acme", "plain") // no .git: nothing to check
	write(t, filepath.Join(good, "README.md"), "# good")
	write(t, filepath.Join(plain, "README.md"), "# plain")
	gitRepo(t, good, "main", "main") // origin acme/widget
	var asked []string
	refuse := false
	opts := RefreshOpts{KnownRoots: []string{root}, CheckOwner: func(_ context.Context, owner string) error {
		asked = append(asked, owner)
		if refuse {
			return errors.New("binding not confirmed")
		}
		return nil
	}}
	ing := &originIng{}
	if st, err := Refresh(context.Background(), Source{Kind: KindOwner, URI: "acme"}, ing, opts); err != nil || st != "ok" {
		t.Fatalf("refresh = %q, %v", st, err)
	}
	if len(asked) != 1 || asked[0] != "acme" {
		t.Fatalf("asked %v", asked)
	}
	for _, m := range ing.metas {
		p := strings.TrimPrefix(m.SourceURI, "checkout://")
		if m.Origin != (rag.Origin{System: rag.OriginFS, Path: p}) {
			t.Fatalf("%s origin = %+v", m.SourceURI, m.Origin)
		}
	}
	refuse, ing.metas = true, nil
	st, err := Refresh(context.Background(), Source{Kind: KindOwner, URI: "acme"}, ing, opts)
	if err != nil || !strings.Contains(st, "good not indexed: binding not confirmed") {
		t.Fatalf("refused refresh = %q, %v", st, err)
	}
	for _, m := range ing.metas {
		if strings.Contains(m.SourceURI, "/good/") {
			t.Fatalf("a refused org's checkout was written: %s", m.SourceURI)
		}
	}
	if len(ing.metas) == 0 {
		t.Fatal("the unchecked plain directory was not written")
	}
}

package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDocument(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	f.seed("orchestrator",
		map[string]any{"source_uri": "repo://o/r/guide.md", "source_type": "repo_doc", "scope": "work", "tags": []any{"repo:r"}, "chunk_index": 1,
			"text": "[section: Guide]\n\n## Install\n\nRun make."},
		map[string]any{"source_uri": "repo://o/r/guide.md", "source_type": "repo_doc", "scope": "work", "tags": []any{"repo:r"}, "chunk_index": 0,
			"text": "# Guide\n\nIntro text."},
		map[string]any{"source_uri": "repo://o/r/broken.md", "source_type": "repo_doc", "chunk_index": 0, "text": "one"},
		map[string]any{"source_uri": "repo://o/r/broken.md", "source_type": "repo_doc", "text": "two"},
	)
	ctx := context.Background()

	doc, err := svc.Document(ctx, "repo://o/r/guide.md")
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if want := "# Guide\n\nIntro text.\n\n## Install\n\nRun make."; doc.Text != want {
		t.Errorf("text = %q, want %q (ordered, context line dropped)", doc.Text, want)
	}
	if doc.Chunks != 2 || doc.Malformed || doc.Oversize {
		t.Errorf("doc = %+v", doc)
	}
	if doc.Meta.SourceType != "repo_doc" || doc.Meta.Scope != "work" || len(doc.Meta.Tags) != 1 {
		t.Errorf("meta = %+v", doc.Meta)
	}

	broken, err := svc.Document(ctx, "repo://o/r/broken.md")
	if err != nil || !broken.Malformed || !strings.Contains(broken.Text, "one") || !strings.Contains(broken.Text, "two") {
		t.Errorf("chunk without an index = %+v, %v; want it kept and flagged malformed", broken, err)
	}

	if _, err := svc.Document(ctx, "repo://o/r/missing.md"); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("missing source err = %v, want ErrSourceNotFound", err)
	}
}

func TestDocumentOversize(t *testing.T) {
	old := maxSourceBytes
	maxSourceBytes = 10
	t.Cleanup(func() { maxSourceBytes = old })
	svc, _ := newMigrationService(t, &hookEmbedder{})
	doc, err := svc.Document(context.Background(), "repo://a/README.md")
	if err != nil || !doc.Oversize || doc.Text != "" {
		t.Errorf("oversize doc = %+v, %v", doc, err)
	}
}

func TestDocumentStoreError(t *testing.T) {
	svc := NewService(&hookEmbedder{}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 40, 4)
	if _, err := svc.Document(context.Background(), "x"); err == nil || errors.Is(err, ErrSourceNotFound) {
		t.Errorf("unreachable store err = %v, want a store error", err)
	}
}

package rag

import (
	"context"
	"testing"
	"time"
)

func TestWriterAssignsTrustAndContentHash(t *testing.T) {
	svc, f := datedService(t)
	ctx := context.Background()
	for _, m := range []Meta{
		{SourceURI: "repo://o/r/a.md", SourceType: "repo_doc", Writer: WriterDocs},
		{SourceURI: "https://x", SourceType: "web", Writer: WriterSources},
		{SourceURI: "checkout:///c/a.md", SourceType: "repo_doc", Writer: WriterSources},
		{SourceURI: "repo://o/r/fake.md", SourceType: "repo_doc", Writer: WriterClient},
		{SourceURI: "session://s/record", SourceType: "session_record", Writer: WriterSession},
		{SourceURI: "file:///a.md", SourceType: "repo_doc", Writer: WriterFile},
		{SourceURI: "manual://legacy"},
	} {
		if _, err := svc.IngestText(ctx, "body of "+m.SourceURI, m); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"repo://o/r/a.md": TrustRepo, "https://x": TrustWeb, "checkout:///c/a.md": TrustRepo,
		"repo://o/r/fake.md": TrustClient, "session://s/record": TrustSession, "file:///a.md": TrustFile, "manual://legacy": ""}
	for _, p := range f.collection("c").points {
		uri := str(p.Payload["source_uri"])
		if got := str(p.Payload[fieldTrust]); got != want[uri] {
			t.Errorf("%s trust = %q, want %q", uri, got, want[uri])
		}
		if str(p.Payload[fieldContentHash]) != ContentHash("body of "+uri) {
			t.Errorf("%s content hash wrong", uri)
		}
	}
	cp, ok, err := svc.Copy(ctx, "repo://o/r/a.md")
	if err != nil || !ok || cp.Writer != WriterDocs || cp.Trust != TrustRepo || cp.ContentHash != ContentHash("body of repo://o/r/a.md") {
		t.Fatalf("Copy = %+v, %v, %v", cp, ok, err)
	}
	if _, ok, _ := svc.Copy(ctx, "repo://none"); ok {
		t.Fatal("Copy of a missing source")
	}
	if _, ok, _ := svc.Copy(ctx, ""); ok {
		t.Fatal("Copy of an empty URI")
	}
	got, err := svc.SourcesWrittenBy(ctx, "checkout:///c/", WriterSources)
	if err != nil || len(got) != 1 || got[0] != "checkout:///c/a.md" {
		t.Fatalf("SourcesWrittenBy = %v, %v", got, err)
	}
	if _, err := svc.SourcesWrittenBy(ctx, "", ""); err == nil {
		t.Fatal("no writer accepted")
	}
	hits, _, err := svc.Search(ctx, "body", SearchOpts{TopK: 10, Mode: ModeDense, PerSource: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Trust != want[h.SourceURI] {
			t.Errorf("hit %s trust = %q", h.SourceURI, h.Trust)
		}
	}
	doc, err := svc.Document(ctx, "repo://o/r/a.md")
	if err != nil || doc.Meta.Writer != WriterDocs || doc.Meta.ContentHash == "" || TrustFor(doc.Meta.Writer, doc.Meta.SourceType) != TrustRepo {
		t.Fatalf("Document = %+v, %v", doc.Meta, err)
	}
	if TrustFor("bogus", "") != "" {
		t.Fatal("unknown writer given a tier")
	}
}

func TestUnchangedWritesAreSkipped(t *testing.T) {
	svc, f := datedService(t)
	ctx := context.Background()
	m := Meta{SourceURI: "https://x", SourceType: "web", Writer: WriterSources}
	if _, err := svc.IngestText(ctx, "the page", m); err != nil {
		t.Fatal(err)
	}
	first := map[string]any{}
	for id, p := range f.collection("c").points {
		first[id] = p.Payload[fieldIngestedAt]
		p.Payload[fieldIngestedAt] = float64(1) // mark: a rewrite would replace it
	}
	time.Sleep(time.Millisecond)
	n, err := svc.IngestText(ctx, "the page", m)
	if err != nil || n != len(first) {
		t.Fatalf("skip = %d, %v", n, err)
	}
	for _, p := range f.collection("c").points {
		if p.Payload[fieldIngestedAt] != float64(1) {
			t.Fatal("an unchanged write was rewritten")
		}
	}
	// Changed text, or changed metadata, is written.
	m.Tags = []string{"new"}
	if _, err := svc.IngestText(ctx, "the page", m); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c").points {
		if p.Payload[fieldIngestedAt] == float64(1) {
			t.Fatal("a write with new tags was skipped")
		}
	}
}

func TestWeightIgnoresANaNFloorAndNegativeScores(t *testing.T) {
	p := RankingParams{HalfLife: time.Hour, Floor: nanFloat()}
	if w := p.weight("a", time.Time{}, time.Now()); w != w || w <= 0 {
		t.Fatalf("NaN floor weight = %v", w)
	}
	c := []candidate{cand("neg", "session_transcript", -0.5, rankNow), cand("zero", "repo_doc", 0, rankNow)}
	got := rank(c, DefaultRanking(), "", 0, 2, rankNow)
	for _, x := range got {
		if x.score < 0 {
			t.Fatalf("negative score after weighting: %+v", x)
		}
	}
}

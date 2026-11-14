package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// placeAll is a placer that puts every write in space at revision rev and
// records what it was asked.
func placeAll(space string, rev int64, seen *[]Meta) PlaceFunc {
	return func(_ context.Context, m *Meta) error {
		if seen != nil {
			*seen = append(*seen, *m)
		}
		m.Space, m.LabelsV = space, rev
		return nil
	}
}

func TestPlacedWritesCarryOriginSpaceAndLabels(t *testing.T) {
	svc, f := datedService(t)
	var seen []Meta
	svc.Place = placeAll("acme", 7, &seen)
	ctx := context.Background()
	o := Origin{System: OriginGitHub, RepoOwner: "acme", RepoName: "api", Visibility: "private", OwnerNodeID: "O_1"}
	// A caller's own Space and LabelsV are not what is stored.
	if _, err := svc.IngestText(ctx, "the doc", Meta{SourceURI: "repo://acme/api/a.md", Writer: WriterDocs, Origin: o,
		Space: "public", LabelsV: 99}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Space != "" || seen[0].LabelsV != 0 {
		t.Fatalf("the placer saw a caller's placement: %+v", seen)
	}
	// Placed in acme, the write is stored in acme's collection (SP-024).
	pts := f.collection("c__acme").points
	if len(pts) == 0 {
		t.Fatal("nothing stored")
	}
	for _, p := range pts {
		if str(p.Payload[fieldSpace]) != "acme" || labelsV(p.Payload[fieldLabelsV]) != 7 {
			t.Fatalf("placement = %v, %v", p.Payload[fieldSpace], p.Payload[fieldLabelsV])
		}
		if got := originFrom(p.Payload[fieldOrigin]); got != o {
			t.Fatalf("origin = %+v, want %+v", got, o)
		}
	}
}

func TestPlacerRefusals(t *testing.T) {
	svc, f := datedService(t)
	ctx := context.Background()
	svc.Place = placeAll("acme", 1, nil)
	if _, err := svc.IngestText(ctx, "no origin", Meta{SourceURI: "manual://x"}); err == nil || !strings.Contains(err.Error(), "no origin") {
		t.Fatalf("a write with no origin: %v", err)
	}
	boom := errors.New("conflict")
	svc.Place = func(context.Context, *Meta) error { return boom }
	if _, err := svc.IngestText(ctx, "refused", Meta{SourceURI: "manual://y", Origin: Origin{System: OriginClient}}); !errors.Is(err, boom) {
		t.Fatalf("a refused write: %v", err)
	}
	svc.Place = func(context.Context, *Meta) error { return nil }
	if _, err := svc.IngestText(ctx, "unplaced", Meta{SourceURI: "manual://z", Origin: Origin{System: OriginClient}}); err == nil {
		t.Fatal("a write the placer left without a space was stored")
	}
	if c := f.collection("c"); c != nil && len(c.points) != 0 {
		t.Fatalf("%d chunks stored by refused writes", len(c.points))
	}
}

func TestUnplacedWritesStoreNoPlacement(t *testing.T) {
	svc, f := datedService(t)
	if _, err := svc.IngestText(context.Background(), "plain", Meta{SourceURI: "manual://plain"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c").points {
		for _, k := range []string{fieldOrigin, fieldSpace, fieldLabelsV} {
			if _, ok := p.Payload[k]; ok {
				t.Fatalf("an unplaced write stored %s", k)
			}
		}
	}
}

func TestPlacementChangeIsNotSkipped(t *testing.T) {
	svc, f := datedService(t)
	ctx := context.Background()
	m := Meta{SourceURI: "https://x", SourceType: "web", Writer: WriterSources, Origin: Origin{System: OriginWeb, URL: "https://x"}}
	svc.Place = placeAll("acme", 1, nil)
	if _, err := svc.IngestText(ctx, "the page", m); err != nil {
		t.Fatal(err)
	}
	svc.Place = placeAll("acme", 2, nil)
	if _, err := svc.IngestText(ctx, "the page", m); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c__acme").points {
		if labelsV(p.Payload[fieldLabelsV]) != 2 {
			t.Fatal("a write placed under a new revision was skipped as unchanged")
		}
	}
}

func TestLabelsVDecoding(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int64
	}{{float64(3), 3}, {int64(4), 4}, {5, 5}, {"6", 6}, {nil, 0}, {"x", 0}} {
		if got := labelsV(c.in); got != c.want {
			t.Errorf("labelsV(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	if (originFrom(nil) != Origin{}) {
		t.Fatal("origin from nothing")
	}
}

// TestMigrationKeepsPlacement: the rag writer of its own, a migration's
// copy, carries each source's origin, space and labels_v over unchanged and
// never asks the placer again.
func TestMigrationKeepsPlacement(t *testing.T) {
	f, url := newFakeQdrant(t)
	origin := map[string]any{"system": "github", "repo_owner": "acme", "repo_name": "api", "visibility": "private", "owner_node_id": "O_1"}
	f.seed("orchestrator",
		map[string]any{"source_uri": "repo://acme/api/a.md", "source_type": "repo_doc", "chunk_index": 0, "text": "acme doc",
			"writer": WriterDocs, "origin": origin, "space": "acme", "labels_v": float64(3)},
		map[string]any{"source_uri": "manual://old", "source_type": "manual", "chunk_index": 0, "text": "written before spaces"},
	)
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "orchestrator", ""), 40, 4)
	svc.StateDir = t.TempDir()
	svc.Place = func(context.Context, *Meta) error { t.Fatal("a migration copy was placed again"); return nil }
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, p := range f.collection("orchestrator-hybrid").points {
		switch str(p.Payload["source_uri"]) {
		case "repo://acme/api/a.md":
			saw = true
			o := originFrom(p.Payload[fieldOrigin])
			if o.System != OriginGitHub || o.RepoOwner != "acme" || o.OwnerNodeID != "O_1" || o.Visibility != "private" {
				t.Fatalf("origin = %+v", o)
			}
			if str(p.Payload[fieldSpace]) != "acme" || labelsV(p.Payload[fieldLabelsV]) != 3 {
				t.Fatalf("placement = %v, %v", p.Payload[fieldSpace], p.Payload[fieldLabelsV])
			}
		case "manual://old":
			if _, ok := p.Payload[fieldSpace]; ok {
				t.Fatal("an unplaced source was given a space by the copy")
			}
		}
	}
	if !saw {
		t.Fatal("the placed source was not copied")
	}
}

package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// movedChunk is a seeded chunk of uri labelled space (none when "") under
// revision 2, with an origin.
func movedChunk(uri string, i int, space string, extra map[string]any) map[string]any {
	p := map[string]any{"source_uri": uri, "source_type": "note", "chunk_index": i, "text": "the deploy runbook " + uri,
		fieldIngestHash: "h-" + uri, fieldIngestedAt: float64(1000 + i),
		fieldOrigin: map[string]any{"system": "manual"}}
	if space != "" {
		p[fieldSpace], p[fieldLabelsV] = space, float64(2)
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// seedMoveFixture stores, under base "c": a Shared note and a legacy unplaced
// one in the base, two chunks in acme's collection, one in globex's and one
// in quarantine's. Every point gets its own vector, so a move that changed
// one would show.
func seedMoveFixture(f *fakeQdrant) {
	f.seedLayout("c", LayoutDense,
		movedChunk("manual://shared", 0, spaces.Shared, nil),
		movedChunk("manual://legacy", 0, "", nil))
	f.seedLayout("c__acme", LayoutDense, movedChunk("manual://acme", 0, "acme", nil), movedChunk("manual://acme", 1, "acme", nil))
	f.seedLayout("c__globex", LayoutDense, movedChunk("manual://globex", 0, "globex", nil))
	f.seedLayout("c__quarantine", LayoutDense, movedChunk("manual://q", 0, spaces.Quarantine, nil))
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.collections))
	for name := range f.collections {
		names = append(names, name)
	}
	sort.Strings(names)
	n := 0
	for _, name := range names {
		ids := make([]string, 0)
		for id := range f.collections[name].points {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			n++
			p := f.collections[name].points[id]
			p.Vector = json.RawMessage(fmt.Sprintf(`[%d,1]`, n))
			f.collections[name].points[id] = p
		}
	}
}

// storedPoint is a point as the fake holds it: where, its payload and its
// vector.
type storedPoint struct {
	collection string
	payload    string
	vector     string
}

// pointsOf returns every stored point by ID.
func pointsOf(t *testing.T, f *fakeQdrant) map[string]storedPoint {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]storedPoint{}
	for name, c := range f.collections {
		for id, p := range c.points {
			if prev, ok := out[id]; ok {
				t.Fatalf("point %s is in both %s and %s", id, prev.collection, name)
			}
			b, err := json.Marshal(p.Payload)
			if err != nil {
				t.Fatal(err)
			}
			out[id] = storedPoint{collection: name, payload: string(b), vector: compactJSON(t, p.Vector)}
		}
	}
	return out
}

// compactJSON is a point's dense vector as JSON. Reason: a collection a
// move creates is hybrid (as a relabel's are), so a point moved there
// stores its dense vector by name beside a keyword vector.
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if named, ok := v.(map[string]any); ok {
		v = named[denseName]
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// collectionsOf lists the fake's collections, sorted.
func collectionsOf(f *fakeQdrant) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name := range f.collections {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// moveService is a service over a fake store whose embeds are counted.
func moveService(t *testing.T) (*Service, *fakeQdrant, *int) {
	t.Helper()
	svc, f := datedService(t)
	n := 0
	svc.emb = embedCounter{Embedder: svc.emb, n: &n}
	return svc, f, &n
}

// TestSpaceMoveRoundTrip: turning spaces off merges every space's chunks into
// the base; turning them on splits them back. Every chunk keeps its payload
// (space, labels_v, origin) and its vectors, ends where it started, and
// nothing is embedded. Running either again finds nothing to do.
func TestSpaceMoveRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, f, embeds := moveService(t)
	seedMoveFixture(f)
	before := pointsOf(t, f)

	rep, err := svc.MergeSpaces(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Collections != 3 || rep.Done != 3 || rep.Sources != 3 || rep.Chunks != 4 || rep.Dropped != 3 || rep.Failed != 0 {
		t.Fatalf("merge report = %+v", rep)
	}
	if got := collectionsOf(f); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("collections after merge = %v, want only the base", got)
	}
	merged := pointsOf(t, f)
	for id, p := range before {
		m, ok := merged[id]
		if !ok || m.collection != "c" || m.payload != p.payload || m.vector != p.vector {
			t.Fatalf("point %s after merge = %+v, before %+v", id, m, p)
		}
	}
	if again, err := svc.MergeSpaces(ctx, nil); err != nil || again.Sources != 0 || again.Collections != 0 {
		t.Fatalf("second merge = %+v, %v", again, err)
	}

	rep, err = svc.SplitSpaces(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sources != 3 || rep.Chunks != 4 || rep.Failed != 0 {
		t.Fatalf("split report = %+v", rep)
	}
	if after := pointsOf(t, f); !reflect.DeepEqual(after, before) {
		t.Fatalf("a round trip changed the store:\nbefore %v\nafter  %v", before, after)
	}
	if again, err := svc.SplitSpaces(ctx, nil); err != nil || again.Sources != 0 {
		t.Fatalf("second split = %+v, %v", again, err)
	}
	if *embeds != 0 {
		t.Fatalf("the moves embedded %d time(s)", *embeds)
	}
}

// TestSpaceMoveSearchMatchesOneStore: with spaces off, a search of the
// merged store finds what one collection holding the same chunks finds, as
// before spaces, quarantined content included.
func TestSpaceMoveSearchMatchesOneStore(t *testing.T) {
	ctx := context.Background()
	svc, f, _ := moveService(t)
	seedMoveFixture(f)
	if _, err := svc.MergeSpaces(ctx, nil); err != nil {
		t.Fatal(err)
	}
	baseline, g := datedService(t)
	f.mu.Lock()
	one := &fakeCollection{layout: LayoutDense, points: map[string]fakePoint{}}
	for id, p := range f.collections["c"].points {
		one.points[id] = p
	}
	f.mu.Unlock()
	g.mu.Lock()
	g.collections["c"] = one
	g.mu.Unlock()
	search := func(s *Service) []string {
		hits, _, err := s.Search(ctx, "deploy runbook", SearchOpts{TopK: 50, Mode: ModeDense})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, fmt.Sprintf("%s %q %.4f", h.SourceURI, h.Text, h.Score))
		}
		sort.Strings(out)
		return out
	}
	got, want := search(svc), search(baseline)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged search = %v\none-store search = %v", got, want)
	}
	if !strings.Contains(strings.Join(got, " "), "manual://q") {
		t.Fatalf("quarantined content is not readable with spaces off: %v", got)
	}
}

// TestSpaceMoveResumes: a move stopped between a copy and its delete, or
// cancelled part-way, finishes on the next run in either direction without
// losing or duplicating a chunk.
func TestSpaceMoveResumes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fail string // the collection whose next delete fails
		run  func(*Service, context.Context) error
	}{
		{"merge, failed delete", "c__acme", func(s *Service, ctx context.Context) error { _, err := s.MergeSpaces(ctx, nil); return err }},
		{"split, failed delete", "c", func(s *Service, ctx context.Context) error { _, err := s.SplitSpaces(ctx, nil); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, f, _ := moveService(t)
			seedMoveFixture(f)
			before := pointsOf(t, f)
			if strings.HasPrefix(tc.name, "split") {
				if _, err := svc.MergeSpaces(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			f.mu.Lock()
			f.failDeletes[tc.fail] = 1
			f.mu.Unlock()
			err := tc.run(svc, ctx)
			if err == nil {
				t.Fatal("a failed delete did not stop the run")
			}
			if strings.Contains(err.Error(), "acme") || strings.Contains(err.Error(), "manual://") {
				t.Fatalf("the error names a space or a source: %v", err)
			}
			if err := tc.run(svc, ctx); err != nil {
				t.Fatal(err)
			}
			after := pointsOf(t, f)
			if len(after) != len(before) {
				t.Fatalf("%d point(s) before, %d after the resumed run", len(before), len(after))
			}
			for id, p := range before {
				if a := after[id]; a.payload != p.payload || a.vector != p.vector {
					t.Fatalf("point %s changed: %+v, was %+v", id, a, p)
				}
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		svc, f, _ := moveService(t)
		seedMoveFixture(f)
		before := pointsOf(t, f)
		cctx, cancel := context.WithCancel(ctx)
		if _, err := svc.MergeSpaces(cctx, func(r MoveReport) {
			if r.Sources == 1 {
				cancel()
			}
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled merge returned %v", err)
		}
		if _, err := svc.MergeSpaces(ctx, nil); err != nil {
			t.Fatal(err)
		}
		cctx, cancel = context.WithCancel(ctx)
		if _, err := svc.SplitSpaces(cctx, func(r MoveReport) {
			if r.Sources == 1 {
				cancel()
			}
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled split returned %v", err)
		}
		if _, err := svc.SplitSpaces(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if after := pointsOf(t, f); !reflect.DeepEqual(after, before) {
			t.Fatalf("interrupted round trip changed the store:\nbefore %v\nafter  %v", before, after)
		}
	})
}

// TestSpaceMoveKeepsALaterWrite: a source written again where the switch
// now puts it is kept; the older copy being moved is only deleted.
func TestSpaceMoveKeepsALaterWrite(t *testing.T) {
	svc, f, _ := moveService(t)
	f.seedLayout("c", LayoutDense, movedChunk("manual://acme", 0, spaces.Shared,
		map[string]any{fieldIngestHash: "newer", fieldIngestedAt: float64(5000), "text": "rewritten"}))
	f.seedLayout("c__acme", LayoutDense, movedChunk("manual://acme", 0, "acme", nil), movedChunk("manual://acme", 1, "acme", nil))
	if _, err := svc.MergeSpaces(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got := sourcesIn(f)
	if len(got) != 1 || got["c"]["manual://acme"] != 1 {
		t.Fatalf("store after merge = %v, want only the later write", got)
	}
	f.mu.Lock()
	text := f.collections["c"].points[pointID("manual://acme", 0)].Payload["text"]
	f.mu.Unlock()
	if text != "rewritten" {
		t.Fatalf("the later write was replaced: %v", text)
	}
}

// TestSpaceMoveSplitQuarantinesAnUnknownSpace: a chunk whose stored space
// is not a space name goes to quarantine, never stays readable as Shared.
func TestSpaceMoveSplitQuarantinesAnUnknownSpace(t *testing.T) {
	svc, f, _ := moveService(t)
	f.seedLayout("c", LayoutDense, movedChunk("manual://odd", 0, "Not A Space", nil), movedChunk("manual://ok", 0, "acme", nil))
	rep, err := svc.SplitSpaces(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 || rep.Sources != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if got := sourcesIn(f); got["c__quarantine"]["manual://odd"] != 1 || got["c__acme"]["manual://ok"] != 1 || len(got["c"]) != 0 {
		t.Fatalf("store = %v", got)
	}
}

// TestSpaceMoveRefusesDuringAMigration: a move waits for a hybrid
// migration's shadow to go.
func TestSpaceMoveRefusesDuringAMigration(t *testing.T) {
	svc, _, _ := moveService(t)
	svc.shadow = svc.store.WithCollection("c_next")
	if _, err := svc.MergeSpaces(context.Background(), nil); !errors.Is(err, errMoveMigrating) {
		t.Fatalf("merge during a migration = %v", err)
	}
	if _, err := svc.SplitSpaces(context.Background(), nil); !errors.Is(err, errMoveMigrating) {
		t.Fatalf("split during a migration = %v", err)
	}
}

// TestSearchFindsAMovingSourceOnce: while a move runs, a source caught in
// two collections is found once; outside a move both copies are, as before.
func TestSearchFindsAMovingSourceOnce(t *testing.T) {
	svc, f, _ := moveService(t)
	f.seedLayout("c", LayoutDense, movedChunk("manual://acme", 0, "acme", nil))
	f.seedLayout("c__acme", LayoutDense, movedChunk("manual://acme", 0, "acme", nil))
	count := func() int {
		hits, _, err := svc.Search(context.Background(), "deploy runbook", SearchOpts{TopK: 10, Mode: ModeDense})
		if err != nil {
			t.Fatal(err)
		}
		return len(hits)
	}
	if n := count(); n != 2 {
		t.Fatalf("outside a move: %d hit(s), want both copies", n)
	}
	done := svc.moves.start()
	defer done()
	if !svc.Moving() {
		t.Fatal("Moving() during a move")
	}
	if n := count(); n != 1 {
		t.Fatalf("during a move: %d hit(s), want one", n)
	}
}

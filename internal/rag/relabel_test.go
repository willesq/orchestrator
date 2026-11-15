package rag

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// placeByTag places a stale source in the space its first tag names, as a test
// stand-in for orchd's relabel placer.
func placeByTag(_ context.Context, m *Meta) error {
	if len(m.Tags) == 0 {
		return errors.New("no tag to place by")
	}
	m.Space = m.Tags[0]
	return nil
}

// staleChunk is one seeded chunk of uri, tagged with the space it belongs in.
func staleChunk(uri string, i int, tag string, extra map[string]any) map[string]any {
	p := map[string]any{"source_uri": uri, "source_type": "note", "chunk_index": i, "text": "the deploy runbook " + uri,
		"tags": []any{tag}, fieldIngestHash: "h-" + uri}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// seedStale stores, under base "c": three legacy chunks of manual://plan
// (unplaced, belongs in acme), one legacy chunk of manual://shared-doc
// (belongs in Shared), one chunk SP-023 placed in acme but left in the base
// (manual://placed, still belongs in acme), and in acme's collection one chunk
// labelled by revision 1 that now belongs in globex (manual://moved).
func seedStale(f *fakeQdrant) {
	f.seedLayout("c", LayoutHybrid,
		staleChunk("manual://plan", 0, "acme", nil), staleChunk("manual://plan", 1, "acme", nil), staleChunk("manual://plan", 2, "acme", nil),
		staleChunk("manual://shared-doc", 0, spaces.Shared, nil),
		staleChunk("manual://placed", 0, "acme", map[string]any{fieldSpace: "acme", fieldLabelsV: float64(1)}))
	f.seedLayout("c__acme", LayoutHybrid,
		staleChunk("manual://moved", 0, "globex", map[string]any{fieldSpace: "acme", fieldLabelsV: float64(1)}))
}

// found returns the sources a search of spaces finds.
func found(t *testing.T, svc *Service, sp ...string) []string {
	t.Helper()
	hits, _, err := svc.Search(context.Background(), "deploy runbook", SearchOpts{TopK: 50, Spaces: sp, Mode: ModeDense})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hits {
		if !slices.Contains(out, h.SourceURI) {
			out = append(out, h.SourceURI)
		}
	}
	sort.Strings(out)
	return out
}

// sourcesIn lists each collection's sources with their chunk counts.
func sourcesIn(f *fakeQdrant) map[string]map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]int{}
	for name, c := range f.collections {
		for _, p := range c.points {
			if out[name] == nil {
				out[name] = map[string]int{}
			}
			out[name][str(p.Payload["source_uri"])]++
		}
	}
	return out
}

func relabel(t *testing.T, svc *Service, apply bool) RelabelReport {
	t.Helper()
	rep, err := svc.Relabel(context.Background(), RelabelOpts{Rev: 2, Place: placeByTag, Apply: apply})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestRelabelMakesStaleChunksReadableInTheirSpace: SP-027.1.a1. Before a
// relabel, chunks stored before collections were split (unplaced, or placed
// but left in the base) and chunks a policy change moved to another space are
// not found by a search of the space they now belong to; after it they are,
// each in its own space's collection, and the base keeps only Shared content.
func TestRelabelMakesStaleChunksReadableInTheirSpace(t *testing.T) {
	svc, f := datedService(t)
	seedStale(f)
	f.seedLayout("c__globex", LayoutHybrid) // exists, empty: a search of a missing collection fails
	if got := found(t, svc, "acme"); !slices.Equal(got, []string{"manual://moved"}) {
		t.Fatalf("before relabel, acme reads %v; want only the chunk still in its collection", got)
	}
	if got := found(t, svc, "globex"); len(got) != 0 {
		t.Fatalf("before relabel, globex reads %v", got)
	}

	rep := relabel(t, svc, true)

	if got := found(t, svc, "acme"); !slices.Equal(got, []string{"manual://placed", "manual://plan"}) {
		t.Fatalf("after relabel, acme reads %v", got)
	}
	if got := found(t, svc, "globex"); !slices.Equal(got, []string{"manual://moved"}) {
		t.Fatalf("after relabel, globex reads %v", got)
	}
	if got := found(t, svc, spaces.Shared); !slices.Equal(got, []string{"manual://shared-doc"}) {
		t.Fatalf("after relabel, shared reads %v", got)
	}
	want := map[string]map[string]int{
		"c":         {"manual://shared-doc": 1},
		"c__acme":   {"manual://plan": 3, "manual://placed": 1},
		"c__globex": {"manual://moved": 1},
	}
	got := sourcesIn(f)
	for name, srcs := range want {
		for uri, n := range srcs {
			if got[name][uri] != n {
				t.Fatalf("after relabel the collections hold %v, want %v", got, want)
			}
		}
		if len(got[name]) != len(srcs) {
			t.Fatalf("after relabel the collections hold %v, want %v", got, want)
		}
	}
	for name, c := range map[string]*fakeCollection{"c": f.collection("c"), "c__acme": f.collection("c__acme"), "c__globex": f.collection("c__globex")} {
		for _, p := range c.points {
			if labelsV(p.Payload[fieldLabelsV]) != 2 || str(p.Payload[fieldSpace]) == "" {
				t.Fatalf("%s: a chunk was left labelled %v/%v", name, p.Payload[fieldSpace], p.Payload[fieldLabelsV])
			}
			if string(p.dense()) != "[1,0]" {
				t.Fatalf("%s: a moved chunk's vector changed to %s", name, p.dense())
			}
		}
	}
	wantTr := []Transition{{"acme", "acme", 1}, {"acme", "globex", 1}, {Unplaced, "acme", 3}, {Unplaced, spaces.Shared, 1}}
	if !slices.Equal(rep.Transitions, wantTr) {
		t.Fatalf("transitions = %v, want %v", rep.Transitions, wantTr)
	}
	if rep.Total != 6 || rep.Stale != 6 || rep.Changed != 6 || rep.Moved != 5 || rep.Failed != 0 || !rep.Applied {
		t.Fatalf("report = %+v", rep)
	}
}

// TestRelabelSecondRunChangesNothing: SP-027.1.a2. A second run finds no
// stale chunk, changes 0 chunks and sends no write.
func TestRelabelSecondRunChangesNothing(t *testing.T) {
	svc, f := datedService(t)
	seedStale(f)
	relabel(t, svc, true)
	f.requests()

	rep := relabel(t, svc, true)

	if rep.Changed != 0 || rep.Stale != 0 || rep.Moved != 0 || len(rep.Transitions) != 0 {
		t.Fatalf("a second run changed %d chunk(s): %+v", rep.Changed, rep)
	}
	for _, r := range f.requests() {
		if isWrite(r) {
			t.Fatalf("a second run wrote: %s %s on %s", r.method, r.action, r.collection)
		}
	}
}

// total counts every chunk of every collection, and fails on a source held in
// two collections.
func total(t *testing.T, f *fakeQdrant) int {
	t.Helper()
	n := 0
	where := map[string]string{}
	for name, srcs := range sourcesIn(f) {
		for uri, k := range srcs {
			if other, ok := where[uri]; ok {
				t.Fatalf("%s is held by both %s and %s", uri, other, name)
			}
			where[uri] = name
			n += k
		}
	}
	return n
}

// TestRelabelResumesAfterInterruption: SP-027.1.a3. A run stopped between
// copying a source and deleting its old copy, and a run cancelled part-way,
// each resume on the next run without duplicating or losing a chunk: the
// count after equals the count before, and every source ends in one
// collection.
func TestRelabelResumesAfterInterruption(t *testing.T) {
	t.Run("between copy and delete", func(t *testing.T) {
		svc, f := datedService(t)
		seedStale(f)
		before := total(t, f)
		f.mu.Lock()
		f.failDeletes["c"] = 1
		f.mu.Unlock()
		if _, err := svc.Relabel(context.Background(), RelabelOpts{Rev: 2, Place: placeByTag, Apply: true}); err == nil {
			t.Fatal("a failed delete did not stop the run")
		} else if strings.Contains(err.Error(), "acme") || strings.Contains(err.Error(), "manual://") {
			t.Fatalf("the error names a space or a source: %v", err)
		}
		mid, both := sourcesIn(f), false
		for uri := range mid["c"] {
			both = both || mid["c__acme"][uri] > 0
		}
		if !both {
			t.Fatalf("the stopped run left no source copied but not deleted: %v", mid)
		}
		rep := relabel(t, svc, true)
		if after := total(t, f); after != before {
			t.Fatalf("chunks before %d, after the resumed run %d", before, after)
		}
		if again := relabel(t, svc, true); again.Changed != 0 {
			t.Fatalf("the resumed run left %d chunk(s) stale (first resume: %+v)", again.Changed, rep)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		svc, f := datedService(t)
		seedStale(f)
		before := total(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		stop := func(ctx context.Context, m *Meta) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return placeByTag(ctx, m)
		}
		if _, err := svc.Relabel(ctx, RelabelOpts{Rev: 2, Place: stop, Apply: true}); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled run returned %v", err)
		}
		relabel(t, svc, true)
		if after := total(t, f); after != before {
			t.Fatalf("chunks before %d, after the resumed run %d", before, after)
		}
		if again := relabel(t, svc, true); again.Changed != 0 {
			t.Fatalf("the resumed run left %d chunk(s) stale", again.Changed)
		}
	})
}

// TestRelabelDryRunOutputHasOnlyCountsAndSpaceNames: SP-027.1.a4. A dry run
// changes nothing, and what it prints is counts and space names only: no
// source URI, no chunk text, no collection name.
func TestRelabelDryRunOutputHasOnlyCountsAndSpaceNames(t *testing.T) {
	svc, f := datedService(t)
	seedStale(f)
	f.requests()
	rep := relabel(t, svc, false)
	for _, r := range f.requests() {
		if isWrite(r) {
			t.Fatalf("a dry run wrote: %s %s on %s", r.method, r.action, r.collection)
		}
	}
	if rep.Applied || rep.Changed != 6 {
		t.Fatalf("dry-run report = %+v", rep)
	}
	var out bytes.Buffer
	WriteRelabelReport(&out, rep)
	words := regexp.MustCompile(`[A-Za-z_]+`)
	allowed := map[string]bool{"acme": true, "globex": true, "shared": true, Unplaced: true}
	for _, w := range strings.Fields("policy revision chunk s stale would change of them moved to another collection") {
		allowed[w] = true
	}
	for _, w := range words.FindAllString(out.String(), -1) {
		if !allowed[w] {
			t.Fatalf("dry-run output has %q, which is neither a count nor a space name:\n%s", w, out.String())
		}
	}
	for _, leak := range []string{"manual://", "runbook", `"c"`, "c__"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("dry-run output contains %q:\n%s", leak, out.String())
		}
	}
	if !strings.Contains(out.String(), "unplaced -> acme: 3 chunk(s)") {
		t.Fatalf("dry-run output lacks the transition counts:\n%s", out.String())
	}
}

// TestRelabelEdgeCases covers the refusals and the sources a run cannot
// place or address.
func TestRelabelEdgeCases(t *testing.T) {
	ctx := context.Background()
	t.Run("no policy", func(t *testing.T) {
		svc, _ := datedService(t)
		if _, err := svc.Relabel(ctx, RelabelOpts{Place: placeByTag}); err == nil {
			t.Fatal("a relabel with no applied revision ran")
		}
		if _, err := svc.Relabel(ctx, RelabelOpts{Rev: 1}); err == nil {
			t.Fatal("a relabel without a placer ran")
		}
	})
	t.Run("migration running", func(t *testing.T) {
		svc, _ := datedService(t)
		svc.setShadow(svc.active().WithCollection("c_next"))
		if _, err := svc.Relabel(ctx, RelabelOpts{Rev: 1, Place: placeByTag}); err == nil {
			t.Fatal("a relabel ran during a migration")
		}
	})
	t.Run("unplaceable and unaddressable", func(t *testing.T) {
		svc, f := datedService(t)
		f.seedLayout("c", LayoutHybrid,
			map[string]any{"source_uri": "manual://untagged", "chunk_index": 0, "text": "no tag"},
			map[string]any{"text": "no source"})
		rep := relabel(t, svc, true)
		if rep.Failed != 2 || rep.Changed != 0 {
			t.Fatalf("report = %+v, want both chunks failed", rep)
		}
		var out bytes.Buffer
		WriteRelabelReport(&out, rep)
		if !strings.Contains(out.String(), "2 chunk(s) could not be placed") {
			t.Fatalf("output:\n%s", out.String())
		}
	})
	t.Run("dense base to a new hybrid space", func(t *testing.T) {
		svc, f := datedService(t)
		f.seed("c", staleChunk("manual://old", 0, "acme", nil))
		relabel(t, svc, true)
		c := f.collection("c__acme")
		if c == nil || c.layout != LayoutHybrid || len(c.points) != 1 {
			t.Fatalf("acme's collection = %+v", c)
		}
		for _, p := range c.points {
			if !strings.Contains(string(p.Vector), sparseName) || string(p.dense()) != "[1,0]" {
				t.Fatalf("moved point's vectors = %s", p.Vector)
			}
		}
	})
	t.Run("a newer copy in the target wins", func(t *testing.T) {
		svc, f := datedService(t)
		f.seedLayout("c", LayoutHybrid, staleChunk("manual://plan", 0, "acme", nil), staleChunk("manual://plan", 1, "acme", nil))
		f.seedLayout("c__acme", LayoutHybrid, staleChunk("manual://plan", 0, "acme",
			map[string]any{fieldSpace: "acme", fieldLabelsV: float64(2), fieldIngestHash: "newer", "text": "newer"}))
		relabel(t, svc, true)
		got := sourcesIn(f)
		if got["c"]["manual://plan"] != 0 || got["c__acme"]["manual://plan"] != 1 {
			t.Fatalf("collections = %v; the newer copy should stay alone", got)
		}
	})
	t.Run("older longer copy in the target is pruned", func(t *testing.T) {
		svc, f := datedService(t)
		f.seedLayout("c", LayoutHybrid, staleChunk("manual://plan", 0, "acme", nil))
		f.seedLayout("c__acme", LayoutHybrid, staleChunk("manual://plan", 1, "acme", nil), staleChunk("manual://plan", 2, "acme", nil))
		relabel(t, svc, true)
		if got := sourcesIn(f); got["c__acme"]["manual://plan"] != 1 || got["c"]["manual://plan"] != 0 {
			t.Fatalf("collections = %v", got)
		}
	})
}

// TestRelabelChangedPlacementAfterInterruption: a run stopped after copying a
// source to one space, followed by a run that places it in another (its
// repo's visibility or checkout changed meanwhile), leaves the source in the
// second space only: no current-labelled copy stays readable in the first.
func TestRelabelChangedPlacementAfterInterruption(t *testing.T) {
	svc, f := datedService(t)
	f.seedLayout("c", LayoutHybrid, staleChunk("manual://plan", 0, "acme", nil), staleChunk("manual://plan", 1, "acme", nil))
	f.mu.Lock()
	f.failDeletes["c"] = 1
	f.mu.Unlock()
	if _, err := svc.Relabel(context.Background(), RelabelOpts{Rev: 2, Place: placeByTag, Apply: true}); !errors.Is(err, errRelabelStore) {
		t.Fatalf("stopped run returned %v", err)
	}
	if got := sourcesIn(f); got["c__acme"]["manual://plan"] != 2 || got["c"]["manual://plan"] != 2 {
		t.Fatalf("after the stopped run: %v", got)
	}
	toGlobex := func(_ context.Context, m *Meta) error { m.Space = "globex"; return nil }
	// The run that places it elsewhere fails to clear the first copy: the
	// stale copy is kept, so the next run still finds the source.
	f.mu.Lock()
	f.failDeletes["c__acme"] = 1
	f.mu.Unlock()
	if _, err := svc.Relabel(context.Background(), RelabelOpts{Rev: 2, Place: toGlobex, Apply: true}); !errors.Is(err, errRelabelStore) {
		t.Fatalf("run with a failed clean-up returned %v", err)
	}
	if got := sourcesIn(f); got["c"]["manual://plan"] != 2 {
		t.Fatalf("the stale copy went before the clean-up: %v", got)
	}
	if _, err := svc.Relabel(context.Background(), RelabelOpts{Rev: 2, Place: toGlobex, Apply: true}); err != nil {
		t.Fatal(err)
	}
	got := sourcesIn(f)
	if got["c__globex"]["manual://plan"] != 2 || got["c__acme"]["manual://plan"] != 0 || got["c"]["manual://plan"] != 0 {
		t.Fatalf("after the changed placement: %v", got)
	}
}

// TestRelabelSkipsADamagedSource: a source whose chunks cannot be moved (no
// dense vector) is counted as failed and left stale; every other source is
// still relabelled.
func TestRelabelSkipsADamagedSource(t *testing.T) {
	svc, f := datedService(t)
	f.seedLayout("c", LayoutHybrid, staleChunk("manual://bad", 0, "acme", nil), staleChunk("manual://good", 0, "acme", nil))
	f.mu.Lock()
	bad := pointID("manual://bad", 0)
	p := f.collections["c"].points[bad]
	p.Vector = []byte(`{"bm25":{"indices":[1],"values":[1]}}`)
	f.collections["c"].points[bad] = p
	f.mu.Unlock()
	rep := relabel(t, svc, true)
	if rep.Failed != 1 || rep.Changed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if got := sourcesIn(f); got["c"]["manual://bad"] != 1 || got["c__acme"]["manual://good"] != 1 {
		t.Fatalf("collections = %v", got)
	}
}

// TestRelabelStoreErrorsNameNoSpace: a failed store request reaches the
// caller as a generic error, never with Qdrant's text, which names
// collections and so spaces.
func TestRelabelStoreErrorsNameNoSpace(t *testing.T) {
	svc, f := datedService(t)
	seedStale(f)
	f.mu.Lock()
	f.failUpserts["c__acme"] = 5
	f.mu.Unlock()
	_, err := svc.Relabel(context.Background(), RelabelOpts{Rev: 2, Place: placeByTag, Apply: true})
	if !errors.Is(err, errRelabelStore) || strings.Contains(err.Error(), "acme") {
		t.Fatalf("error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Relabel(ctx, RelabelOpts{Rev: 2, Place: placeByTag, Apply: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestWirePointDecode(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		dense   int
		sparse  int
		wantErr bool
	}{
		{`[0.5,0.25]`, 2, 0, false},
		{`{"dense":[1,2,3],"bm25":{"indices":[4],"values":[0.5]}}`, 3, 1, false},
		{`null`, 0, 0, false},
		{``, 0, 0, false},
		{`{"dense":"x"}`, 0, 0, true},
		{`{"bm25":"x"}`, 0, 0, true},
		{`[`, 0, 0, true},
		{`{`, 0, 0, true},
	} {
		p, err := wirePoint{ID: "a", Vector: []byte(tc.raw)}.decode()
		if (err != nil) != tc.wantErr || (err == nil && (len(p.Dense) != tc.dense || len(p.Sparse.Indices) != tc.sparse)) {
			t.Errorf("decode(%s) = %+v, %v", tc.raw, p, err)
		}
	}
	if num(3) != 3 || num(int64(4)) != 4 || num("x") != 0 {
		t.Error("num misreads a stored number")
	}
}

// TestMetaOfKeepsContentHash: a relabel places a source with the hash of
// its stored text, so an edited file placed by the session that wrote that
// text (spaces SP-029) is placed the same way again.
func TestMetaOfKeepsContentHash(t *testing.T) {
	m := metaOf("file:///scratch/a.md", map[string]any{fieldContentHash: "abc", fieldWriter: WriterFile})
	if m.ContentHash != "abc" || m.Writer != WriterFile {
		t.Fatalf("metaOf = %+v", m)
	}
}

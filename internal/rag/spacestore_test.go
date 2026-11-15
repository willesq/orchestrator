package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// placeBy places each write in the space its first tag names, as a test
// stand-in for orchd's placer.
func placeBy(_ context.Context, m *Meta) error {
	m.Space, m.LabelsV = m.Tags[0], 1
	return nil
}

// spacesService is a Service over a recording fake whose writes are placed by
// their first tag.
func spacesService(t *testing.T) (*Service, *fakeQdrant) {
	t.Helper()
	svc, f := datedService(t)
	svc.Place = placeBy
	return svc, f
}

// ingestIn writes text as uri, placed in space.
func ingestIn(t *testing.T, svc *Service, space, uri, text string) {
	t.Helper()
	m := Meta{SourceURI: uri, SourceType: "note", Tags: []string{space}, Origin: Origin{System: OriginClient}}
	if _, err := svc.IngestText(context.Background(), text, m); err != nil {
		t.Fatalf("ingest %s in %s: %v", uri, space, err)
	}
}

// isWrite reports whether a request changes a collection.
func isWrite(r fakeReq) bool {
	switch {
	case r.action == "" && r.method != "GET":
		return true
	case r.action == "index", r.action == "points/delete", r.action == "points/payload", r.action == "points/vectors":
		return true
	case r.action == "points" && r.method == "PUT":
		return true
	}
	return false
}

// TestWritesForASpaceTouchOnlyItsCollection: SP-024.1.a1. A write placed in
// space S stores and prunes only in S's collection. Other collections get no
// write of any kind and keep every point; the only request any of them sees is
// the lookup for an older copy of the same source, filtered on its URI.
func TestWritesForASpaceTouchOnlyItsCollection(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "globex", "manual://globex-note", "globex keeps its own notes")
	ingestIn(t, svc, "quarantine", "manual://unplaced", "nobody placed this one")
	f.seedLayout("c", LayoutHybrid, map[string]any{"source_uri": "manual://legacy", "chunk_index": 0, "text": "stored before spaces"})
	before := map[string]int{}
	for _, n := range []string{"c", "c__globex", "c__quarantine"} {
		before[n] = len(f.collection(n).points)
	}
	f.requests()
	svc.lists = nil // the write lists the collections again, as a fresh orchd would

	ingestIn(t, svc, "acme", "manual://acme-note", "acme's plan for the quarter: ship the billing rewrite, then move "+
		"the deploy pipeline to the new runners, and retire the old dashboards before the audit in spring.")

	reqs := f.requests()
	wrote := 0
	for _, r := range reqs {
		if r.action == "list" {
			continue
		}
		if r.collection == "c__acme" {
			if isWrite(r) {
				wrote++
			}
			continue
		}
		if isWrite(r) {
			t.Fatalf("a write for acme changed %s: %s %s", r.collection, r.method, r.action)
		}
		if r.action != "points/count" || !filtersOn(r.body, "source_uri", "manual://acme-note") {
			t.Fatalf("a write for acme read %s beyond its own source: %s %s %v", r.collection, r.method, r.action, r.body)
		}
	}
	if wrote == 0 {
		t.Fatal("nothing was written to acme's collection")
	}
	acme := f.collection("c__acme")
	if acme == nil || len(acme.points) < 2 {
		t.Fatalf("acme's collection holds %v", acme)
	}
	for _, p := range acme.points {
		if str(p.Payload[fieldSpace]) != "acme" {
			t.Fatalf("acme's collection holds a chunk of %q", p.Payload[fieldSpace])
		}
	}
	for n, want := range before {
		if got := len(f.collection(n).points); got != want {
			t.Errorf("%s holds %d points after acme's write, want %d", n, got, want)
		}
	}
}

// filtersOn reports whether a request body filters key to value.
func filtersOn(body map[string]any, key, value string) bool {
	f, _ := body["filter"].(map[string]any)
	must, _ := f["must"].([]any)
	for _, c := range must {
		m, _ := c.(map[string]any)
		if match, ok := m["match"].(map[string]any); ok && m["key"] == key && str(match["value"]) == value {
			return true
		}
	}
	return false
}

// TestWriteMovesASourceOutOfItsOldSpace: a source lives in one space. When a
// later write places it elsewhere, the old copy is deleted, so neither space
// reads a stale copy of the other's.
func TestWriteMovesASourceOutOfItsOldSpace(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "oss", "repo://oss/lib/README.md", "the library's readme")
	ingestIn(t, svc, "acme", "repo://oss/lib/README.md", "the library's readme, now private")
	if n := len(f.collection("c__oss").points); n != 0 {
		t.Fatalf("the old space still holds %d chunks of the moved source", n)
	}
	if n := len(f.collection("c__acme").points); n == 0 {
		t.Fatal("the new space holds nothing")
	}
	cp, ok, err := svc.Copy(context.Background(), "repo://oss/lib/README.md")
	if err != nil || !ok || cp.Space != "acme" {
		t.Fatalf("Copy = %+v %v %v", cp, ok, err)
	}
}

// TestUnplacedAndSharedWritesUseTheBaseCollection: with no policy every write
// is Shared or unplaced and lands in the base collection; with no space
// collections, no other collection is asked anything (INV-10).
func TestUnplacedAndSharedWritesUseTheBaseCollection(t *testing.T) {
	svc, f := datedService(t)
	svc.Place = placeAll(spaces.Shared, 0, nil)
	if _, err := svc.IngestText(context.Background(), "a shared note", Meta{SourceURI: "manual://s", Origin: Origin{System: OriginClient}}); err != nil {
		t.Fatal(err)
	}
	svc.Place = nil
	if _, err := svc.IngestText(context.Background(), "an unplaced note", Meta{SourceURI: "manual://u"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.requests() {
		if r.collection != "c" && r.action != "list" {
			t.Fatalf("a write with no policy reached %q (%s %s)", r.collection, r.method, r.action)
		}
	}
	if got := len(f.collection("c").texts()); got != 2 {
		t.Fatalf("the base holds %d sources, want 2", got)
	}
}

// readerPolicy has one space of each kind, plus a second isolated space and a
// private space the private reader lists.
func readerPolicy(t *testing.T) *spaces.Resolved {
	t.Helper()
	res, err := spaces.Validate(spaces.Policy{Spaces: []spaces.Space{
		{Name: "oss", Kind: spaces.Public},
		{Name: "acme", Kind: spaces.Isolated},
		{Name: "globex", Kind: spaces.Isolated},
		{Name: "me", Kind: spaces.Private, Reads: []string{"notes"}},
		{Name: "notes", Kind: spaces.Private},
	}}, spaces.Env{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// readable lists what reader may read under res, Quarantine included in the
// candidates so a mistake there shows.
func readable(res *spaces.Resolved, reader string) []string {
	var out []string
	for _, sp := range []string{"oss", "acme", "globex", "me", "notes", spaces.Quarantine} {
		if res.CanRead(reader, sp) {
			out = append(out, sp)
		}
	}
	return out
}

// TestSearchQueriesOnlyTheReadableCollections: SP-024.1.a2, one subtest per
// space kind. Every space holds a chunk matching the query; a search for a
// reader's readable spaces sends requests only to those spaces' collections
// and returns only their chunks.
func TestSearchQueriesOnlyTheReadableCollections(t *testing.T) {
	res := readerPolicy(t)
	for _, tc := range []struct {
		kind   spaces.Kind
		reader string
		want   []string
	}{
		{spaces.Public, "oss", []string{"oss"}},
		{spaces.Isolated, "acme", []string{"acme", "oss"}},
		{spaces.Private, "me", []string{"me", "notes", "oss"}},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			svc, f := spacesService(t)
			for _, sp := range []string{"oss", "acme", "globex", "me", "notes", spaces.Quarantine} {
				ingestIn(t, svc, sp, "manual://"+sp, "the deploy runbook of "+sp)
			}
			f.seedLayout("c", LayoutHybrid, map[string]any{"source_uri": "manual://legacy", "chunk_index": 0, "text": "the deploy runbook, unplaced"})
			allowed := readable(res, tc.reader)
			slices.Sort(allowed)
			if !slices.Equal(allowed, tc.want) {
				t.Fatalf("readable(%s) = %v, want %v", tc.reader, allowed, tc.want)
			}
			f.requests()
			for _, mode := range []string{ModeHybrid, ModeDense} {
				hits, _, err := svc.Search(context.Background(), "deploy runbook", SearchOpts{TopK: 20, Spaces: allowed, Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, h := range hits {
					got = append(got, strings.TrimPrefix(h.SourceURI, "manual://"))
				}
				sort.Strings(got)
				if !slices.Equal(got, tc.want) {
					t.Fatalf("%s search from %s returned %v, want %v", mode, tc.reader, got, tc.want)
				}
			}
			queried := map[string]bool{}
			for _, r := range f.requests() {
				if r.action == "list" {
					t.Fatal("a search for named spaces listed the collections")
				}
				queried[strings.TrimPrefix(r.collection, "c__")] = true
			}
			var names []string
			for n := range queried {
				names = append(names, n)
			}
			sort.Strings(names)
			if !slices.Equal(names, tc.want) {
				t.Fatalf("a search from %s reached %v, want only %v", tc.reader, names, tc.want)
			}
		})
	}
}

// TestSearchFiltersOnSpaceInsideEachCollection: SP-024.1.a3. The payload
// filter on space is the second layer: every request a search sends to a
// space's collection filters on that space, so a chunk that sits in a
// collection under another space's label is not returned.
func TestSearchFiltersOnSpaceInsideEachCollection(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "acme", "manual://acme", "the deploy runbook of acme")
	ingestIn(t, svc, "oss", "manual://oss", "the deploy runbook of oss")
	// A globex chunk misfiled in acme's collection: only the payload layer
	// stops it.
	f.collection("c__acme").points["stray"] = fakePoint{ID: "stray", Payload: map[string]any{
		"source_uri": "manual://globex", "text": "the deploy runbook of globex", "space": "globex", "chunk_index": float64(0)}}
	f.requests()
	for _, mode := range []string{ModeHybrid, ModeDense} {
		hits, _, err := svc.Search(context.Background(), "deploy runbook", SearchOpts{TopK: 10, Spaces: []string{"acme", "oss"}, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.SourceURI == "manual://globex" {
				t.Fatalf("%s search returned a chunk labelled for another space", mode)
			}
		}
		if len(hits) != 2 {
			t.Fatalf("%s search returned %d hits, want 2", mode, len(hits))
		}
	}
	searches := 0
	for _, r := range f.requests() {
		if r.action != "points/search" && r.action != "points/query" {
			continue
		}
		searches++
		space := strings.TrimPrefix(r.collection, "c__")
		if !filtersOn(r.body, fieldSpace, space) {
			t.Fatalf("a search of %s did not filter on space %s: %v", r.collection, space, r.body["filter"])
		}
	}
	if searches != 6 { // hybrid: dense and keyword per collection; dense: one each
		t.Fatalf("%d search requests, want 6", searches)
	}
}

// TestSearchSpacesEdges: an empty list reads nothing and embeds nothing;
// unknown and repeated names are skipped; Shared reads the base without the
// second layer, since chunks stored before spaces carry no space.
func TestSearchSpacesEdges(t *testing.T) {
	svc, f := spacesService(t)
	f.seedLayout("c", LayoutHybrid, map[string]any{"source_uri": "manual://legacy", "chunk_index": 0, "text": "stored before spaces"})
	ingestIn(t, svc, "acme", "manual://acme", "acme's note")
	f.requests()
	hits, _, err := svc.Search(context.Background(), "note", SearchOpts{Spaces: []string{}})
	if err != nil || len(hits) != 0 || len(f.requests()) != 0 {
		t.Fatalf("an empty list searched: %v %v", hits, err)
	}
	hits, _, err = svc.Search(context.Background(), "note", SearchOpts{Spaces: []string{"Not A Space", "", spaces.Shared, spaces.Shared}})
	if err != nil || len(hits) != 1 || hits[0].SourceURI != "manual://legacy" {
		t.Fatalf("a search of shared = %v %v", hits, err)
	}
	for _, r := range f.requests() {
		if r.collection != "c" || filtersOn(r.body, fieldSpace, spaces.Shared) {
			t.Fatalf("a search of shared sent %s %s %v", r.collection, r.action, r.body)
		}
	}
	// A space with no collection yet holds nothing; with others it is left
	// out, and alone it is an empty answer, not an error.
	if hits, _, err = svc.Search(context.Background(), "note", SearchOpts{Spaces: []string{"acme", "globex"}}); err != nil || len(hits) != 1 {
		t.Fatalf("acme and an empty globex = %v %v", hits, err)
	}
	// nil reads every collection, as before SP-025.
	if hits, _, err = svc.Search(context.Background(), "note", SearchOpts{}); err != nil || len(hits) != 2 {
		t.Fatalf("a search of everything = %v %v", hits, err)
	}
}

// TestSpaceCollectionNames: the base keeps unplaced and Shared content; every
// other space, Quarantine included, has base__<space>. Only those names read
// back as the base's.
func TestSpaceCollectionNames(t *testing.T) {
	for space, want := range map[string]string{"": "orch", spaces.Shared: "orch", "acme": "orch__acme", spaces.Quarantine: "orch__quarantine"} {
		if got := SpaceCollection("orch", space); got != want {
			t.Errorf("SpaceCollection(orch, %q) = %q, want %q", space, got, want)
		}
	}
	for name, want := range map[string]string{"orch": "", "orch__acme": "acme", "orch__a_b-c": "a_b-c"} {
		if got, ok := spaceOf("orch", name); !ok || got != want {
			t.Errorf("spaceOf(orch, %q) = %q %v, want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"orch-hybrid", "orch__", "orch__Acme", "orch__shared", "other__acme", "orch__" + strings.Repeat("a", 33)} {
		if _, ok := spaceOf("orch", name); ok {
			t.Errorf("spaceOf(orch, %q) took it for a space collection", name)
		}
	}
}

// TestSearchAcrossCollectionsMatchesOneCollection: a search over several
// space collections returns what one collection holding the same chunks
// returns, scores included, though it fetches dense vectors only for the
// keyword-only hits it returns (SP-024.3). Keyword requests across
// collections ask for no vectors.
func TestSearchAcrossCollectionsMatchesOneCollection(t *testing.T) {
	f, url := newFakeQdrant(t)
	one := &fakeCollection{layout: LayoutHybrid, points: map[string]fakePoint{}}
	f.collections["one"] = one
	for _, sp := range []string{"a", "b"} {
		f.collections["c__"+sp] = &fakeCollection{layout: LayoutHybrid, points: map[string]fakePoint{}}
	}
	for i := range 40 {
		sp := []string{"a", "b"}[i%2]
		id := fmt.Sprintf("p%02d", i)
		score := 1 - float64(i)/100
		pt := fakePoint{ID: id, Vector: json.RawMessage(fmt.Sprintf(`{"dense":[%g,%g]}`, math.Cos(float64(i)/10), math.Sin(float64(i)/10))),
			Payload: map[string]any{"source_uri": fmt.Sprintf("manual://%s/%d", sp, i), "text": "runbook " + id, "space": sp,
				"score": score, "keyword_only": i%3 == 0, "source_type": "note", "chunk_index": float64(0)}}
		one.points[id] = pt
		f.collections["c__"+sp].points[id] = pt
	}
	ctx := context.Background()
	// Ranked below every hit both lists found, the keyword-only hits are
	// returned only when the search asks for many.
	opts := SearchOpts{TopK: 30, Ranking: RankingPlain, PerSource: -1}
	single := NewService(&hookEmbedder{}, NewQdrantStore(url, "one", ""), 40, 4)
	want, _, err := single.Search(ctx, "runbook", opts)
	if err != nil {
		t.Fatal(err)
	}
	multi := NewService(&hookEmbedder{}, NewQdrantStore(url, "c", ""), 40, 4)
	f.requests()
	opts.Spaces = []string{"a", "b"}
	got, _, err := multi.Search(ctx, "runbook", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d hits, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].SourceURI != want[i].SourceURI || got[i].Score != want[i].Score || got[i].Keyword != want[i].Keyword {
			t.Fatalf("hit %d = %s %v %v, want %s %v %v", i, got[i].SourceURI, got[i].Score, got[i].Keyword,
				want[i].SourceURI, want[i].Score, want[i].Keyword)
		}
	}
	fetched, keywordOnly := 0, 0
	for _, h := range got {
		if n, _ := strconv.Atoi(strings.TrimPrefix(h.Text, "runbook p")); n%3 == 0 {
			keywordOnly++
		}
	}
	for _, r := range f.requests() {
		switch r.action {
		case "points/query":
			if r.body["with_vector"] != false {
				t.Fatalf("a keyword request across collections asked for vectors: %v", r.body["with_vector"])
			}
		case "points":
			ids, _ := r.body["ids"].([]any)
			fetched += len(ids)
		}
	}
	if keywordOnly == 0 || fetched != keywordOnly {
		t.Fatalf("fetched %d vectors for %d returned keyword-only hits", fetched, keywordOnly)
	}
}

// TestSearchAcrossCollectionsFallsBackWhenVectorsFail: when the vectors of
// keyword-only hits cannot be read, a configured hybrid search answers by
// meaning alone with a note, and an explicit one fails.
func TestSearchAcrossCollectionsFallsBackWhenVectorsFail(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "a", "manual://a", "deploy runbook a")
	ingestIn(t, svc, "b", "manual://b", "deploy runbook b")
	for _, sp := range []string{"a", "b"} {
		for id, p := range f.collection("c__" + sp).points {
			p.Payload["keyword_only"] = true
			f.collection("c__" + sp).points[id] = p
		}
	}
	f.collection("c__a").points["dense-hit"] = fakePoint{ID: "dense-hit", Vector: json.RawMessage(`[1,0]`),
		Payload: map[string]any{"source_uri": "manual://dense", "text": "found by meaning", "space": "a", "chunk_index": float64(0)}}
	f.failVectorReads = true
	hits, note, err := svc.Search(context.Background(), "deploy runbook", SearchOpts{Spaces: []string{"a", "b"}})
	if err != nil || note == "" || len(hits) != 1 || hits[0].SourceURI != "manual://dense" {
		t.Fatalf("hits %v note %q err %v", hits, note, err)
	}
	if _, _, err := svc.Search(context.Background(), "deploy runbook", SearchOpts{Spaces: []string{"a", "b"}, Mode: ModeHybrid}); !errors.Is(err, ErrHybridUnavailable) {
		t.Fatalf("explicit hybrid = %v", err)
	}
}

// TestTiedScoresFetchTheirCosine: when keyword-only candidates tie on their
// weighted score, the cosine orders them, so their vectors are fetched
// before ranking even when they are not returned; untied candidates that are
// not returned are never fetched.
func TestTiedScoresFetchTheirCosine(t *testing.T) {
	f, url := newFakeQdrant(t)
	f.collections["c__a"] = &fakeCollection{layout: LayoutHybrid, points: map[string]fakePoint{
		"low":  {ID: "low", Vector: json.RawMessage(`{"dense":[0,1]}`)},
		"high": {ID: "high", Vector: json.RawMessage(`{"dense":[1,0]}`)},
		"skip": {ID: "skip", Vector: json.RawMessage(`{"dense":[1,0]}`)},
	}}
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "c", ""), 40, 4)
	targets, _ := svc.readStores(context.Background(), []string{"a"})
	cand := func(id string, score float64) candidate {
		return candidate{hit: SearchHit{ID: id, Payload: map[string]any{"source_uri": "manual://" + id}}, score: score, collection: "c__a", needsCos: true}
	}
	cands := []candidate{cand("low", 0.5), cand("high", 0.5), cand("skip", 0.1)}
	f.requests()
	hits, err := svc.finishLazy(context.Background(), targets, cands, []float32{1, 0}, SearchOpts{TopK: 1, Ranking: RankingPlain, PerSource: -1}, nil)
	if err != nil || len(hits) != 1 || hits[0].SourceURI != "manual://high" || hits[0].Score != 1 {
		t.Fatalf("hits %+v, %v", hits, err)
	}
	for _, r := range f.requests() {
		ids, _ := r.body["ids"].([]any)
		if slices.Contains(ids, any("skip")) {
			t.Fatal("an untied candidate that is not returned had its vector fetched")
		}
	}
}

// TestClientWriteNeverMovesAnotherSpacesSource: a client's note never
// deletes another space's copy of the same source, even if the Ingest RPC's
// own check could not read the store; it may replace content stored in the
// base before spaces, as it always could.
func TestClientWriteNeverMovesAnotherSpacesSource(t *testing.T) {
	svc, f := spacesService(t)
	ctx := context.Background()
	ingestIn(t, svc, "globex", "manual://plan", "globex's plan")
	m := Meta{SourceURI: "manual://plan", Tags: []string{"acme"}, Writer: WriterClient, Origin: Origin{System: OriginClient}}
	if _, err := svc.IngestText(ctx, "acme's overwrite", m); !errors.Is(err, ErrStoredInAnotherSpace) {
		t.Fatalf("err = %v", err)
	}
	if f.collection("c__globex").texts()["manual://plan"] == nil || f.collection("c__acme") != nil {
		t.Fatal("a client write moved another space's source")
	}
	f.seedLayout("c", LayoutHybrid, map[string]any{"source_uri": "manual://old", "chunk_index": 0, "text": "before spaces"})
	m.SourceURI = "manual://old"
	if _, err := svc.IngestText(ctx, "now placed", m); err != nil {
		t.Fatal(err)
	}
	if f.collection("c").texts()["manual://old"] != nil || f.collection("c__acme").texts()["manual://old"] == nil {
		t.Fatal("a client write did not move its own pre-spaces note out of the base")
	}
}

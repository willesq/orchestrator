package rag

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// retagService is a spaces service whose embedder counts its calls, holding
// manual://plan (three chunks) in acme and manual://other in globex.
func retagService(t *testing.T) (*Service, *fakeQdrant, *int) {
	t.Helper()
	svc, f := spacesService(t)
	embeds := 0
	svc.emb = embedCounter{Embedder: &hookEmbedder{}, n: &embeds}
	ingestIn(t, svc, "acme", "manual://plan", "the deploy runbook first part second part third part fourth part fifth part sixth")
	ingestIn(t, svc, "globex", "manual://other", "the deploy runbook of another job")
	if n := sourcesIn(f)["c__acme"]["manual://plan"]; n < 2 {
		t.Fatalf("the seeded source has %d chunk(s); the test needs several", n)
	}
	return svc, f, &embeds
}

func retag(t *testing.T, svc *Service, uri, space string) RetagReport {
	t.Helper()
	rep, err := svc.Retag(context.Background(), RetagOpts{SourceURI: uri, Space: space, Rev: 2})
	if err != nil {
		t.Fatalf("retag %s to %s: %v", uri, space, err)
	}
	return rep
}

// TestRetagMovesEveryChunk: SP-071.1.a1. After a retag the source's chunk
// count in the new collection equals its count before, the old collection
// holds none of it, and the store's total is unchanged. The moved chunks keep
// their point IDs and vectors and are labelled with the new space and the
// applied revision.
func TestRetagMovesEveryChunk(t *testing.T) {
	svc, f, _ := retagService(t)
	before := sourcesIn(f)
	n := before["c__acme"]["manual://plan"]
	all := total(t, f)
	ids := map[string]string{}
	for _, p := range f.collection("c__acme").points {
		ids[p.ID] = string(p.dense())
	}

	rep := retag(t, svc, "manual://plan", "globex")

	after := sourcesIn(f)
	if after["c__globex"]["manual://plan"] != n || rep.Chunks != n || !rep.Moved {
		t.Fatalf("globex holds %d chunk(s) of the source (report %+v), want %d", after["c__globex"]["manual://plan"], rep, n)
	}
	if after["c__acme"]["manual://plan"] != 0 {
		t.Fatalf("acme still holds %d chunk(s) of the source", after["c__acme"]["manual://plan"])
	}
	if got := total(t, f); got != all {
		t.Fatalf("total chunks %d before, %d after", all, got)
	}
	if !slices.Equal(rep.From, []string{"acme"}) {
		t.Fatalf("report From = %v", rep.From)
	}
	moved := 0
	for _, p := range f.collection("c__globex").points {
		if str(p.Payload["source_uri"]) != "manual://plan" {
			continue
		}
		moved++
		if v, ok := ids[p.ID]; !ok || v != string(p.dense()) {
			t.Fatalf("point %s changed its ID or vector", p.ID)
		}
		if str(p.Payload[fieldSpace]) != "globex" || labelsV(p.Payload[fieldLabelsV]) != 2 {
			t.Fatalf("a moved chunk is labelled %v/%v", p.Payload[fieldSpace], p.Payload[fieldLabelsV])
		}
	}
	if moved != n {
		t.Fatalf("%d moved chunk(s), want %d", moved, n)
	}
}

// TestRetagEmbedsNothing: SP-071.1.a2. The fake embedder records no calls
// during a retag, including a retag into a collection that does not exist yet
// and a retag back.
func TestRetagEmbedsNothing(t *testing.T) {
	svc, _, embeds := retagService(t)
	*embeds = 0
	retag(t, svc, "manual://plan", "personal")
	retag(t, svc, "manual://plan", "acme")
	retag(t, svc, "manual://plan", spaces.Shared)
	if *embeds != 0 {
		t.Fatalf("the retags embedded %d time(s)", *embeds)
	}
}

// TestRetagSearchFollowsTheSource: SP-071.1.a3. A search from the new space
// finds the source; a search from the old space does not.
func TestRetagSearchFollowsTheSource(t *testing.T) {
	t.Run("new space finds it", func(t *testing.T) {
		svc, _, _ := retagService(t)
		if got := found(t, svc, "globex"); slices.Contains(got, "manual://plan") {
			t.Fatalf("before the retag globex already reads %v", got)
		}
		retag(t, svc, "manual://plan", "globex")
		if got := found(t, svc, "globex"); !slices.Equal(got, []string{"manual://other", "manual://plan"}) {
			t.Fatalf("after the retag globex reads %v", got)
		}
	})
	t.Run("old space does not", func(t *testing.T) {
		svc, _, _ := retagService(t)
		if got := found(t, svc, "acme"); !slices.Equal(got, []string{"manual://plan"}) {
			t.Fatalf("before the retag acme reads %v", got)
		}
		retag(t, svc, "manual://plan", "globex")
		if got := found(t, svc, "acme"); len(got) != 0 {
			t.Fatalf("after the retag acme still reads %v", got)
		}
	})
}

// TestRetagResumesAfterInterruption: SP-071.1.a4. A retag stopped after the
// copy and before the delete leaves the source in both collections; running
// it again ends with a single copy in the new space, no duplicates and
// nothing lost.
func TestRetagResumesAfterInterruption(t *testing.T) {
	svc, f, embeds := retagService(t)
	n := sourcesIn(f)["c__acme"]["manual://plan"]
	all := total(t, f)
	f.mu.Lock()
	f.failDeletes["c__acme"] = 1
	f.mu.Unlock()
	_, err := svc.Retag(context.Background(), RetagOpts{SourceURI: "manual://plan", Space: "globex", Rev: 2})
	if err == nil {
		t.Fatal("a failed delete did not stop the retag")
	}
	if strings.Contains(err.Error(), "acme") || strings.Contains(err.Error(), "globex") || strings.Contains(err.Error(), "c__") {
		t.Fatalf("the error names a space or a collection: %v", err)
	}
	mid := sourcesIn(f)
	if mid["c__acme"]["manual://plan"] != n || mid["c__globex"]["manual://plan"] != n {
		t.Fatalf("the stopped retag left %v; want the source copied but not deleted", mid)
	}

	rep := retag(t, svc, "manual://plan", "globex")

	if !slices.Equal(rep.From, []string{"acme", "globex"}) || rep.Chunks != n {
		t.Fatalf("resumed report = %+v", rep)
	}
	after := sourcesIn(f)
	if after["c__acme"]["manual://plan"] != 0 || after["c__globex"]["manual://plan"] != n {
		t.Fatalf("after the resumed retag the collections hold %v", after)
	}
	if got := total(t, f); got != all {
		t.Fatalf("total chunks %d before, %d after the resumed retag", all, got)
	}
	if *embeds != 2 {
		t.Fatalf("the retags embedded: %d calls after the two seeding writes", *embeds)
	}
}

// TestRetagEdgeCases covers the refusals, a retag to where the source
// already is, a source two collections hold, and a chunk that cannot move.
func TestRetagEdgeCases(t *testing.T) {
	ctx := context.Background()
	t.Run("refusals", func(t *testing.T) {
		svc, _, _ := retagService(t)
		for name, o := range map[string]RetagOpts{
			"no uri":       {Space: "globex", Rev: 2},
			"no policy":    {SourceURI: "manual://plan", Space: "globex"},
			"bad space":    {SourceURI: "manual://plan", Space: "Not A Space", Rev: 2},
			"empty space":  {SourceURI: "manual://plan", Rev: 2},
			"traversal":    {SourceURI: "manual://plan", Space: "../c", Rev: 2},
			"not stored":   {SourceURI: "manual://missing", Space: "globex", Rev: 2},
			"sep in space": {SourceURI: "manual://plan", Space: "a__b c", Rev: 2},
		} {
			if _, err := svc.Retag(ctx, o); err == nil {
				t.Errorf("%s: a retag ran", name)
			}
		}
		if _, err := svc.Retag(ctx, RetagOpts{SourceURI: "manual://missing", Space: "globex", Rev: 2}); !errors.Is(err, ErrRetagNotStored) {
			t.Errorf("a missing source returned %v", err)
		}
	})
	t.Run("during a migration", func(t *testing.T) {
		svc, f, _ := retagService(t)
		svc.setShadow(svc.active().WithCollection("c_next"))
		if _, err := svc.Retag(ctx, RetagOpts{SourceURI: "manual://plan", Space: "globex", Rev: 2}); err == nil {
			t.Fatal("a retag ran during a migration")
		}
		if sourcesIn(f)["c__acme"]["manual://plan"] == 0 {
			t.Fatal("the refused retag moved the source")
		}
	})
	t.Run("already there is stamped", func(t *testing.T) {
		svc, f, _ := retagService(t)
		f.requests()
		rep := retag(t, svc, "manual://plan", "acme")
		if rep.Moved || rep.Chunks != sourcesIn(f)["c__acme"]["manual://plan"] || !slices.Equal(rep.From, []string{"acme"}) {
			t.Fatalf("report = %+v", rep)
		}
		for _, r := range f.requests() {
			if isWrite(r) && r.collection != "c__acme" {
				t.Fatalf("a retag in place wrote %s %s on %s", r.method, r.action, r.collection)
			}
		}
		for _, p := range f.collection("c__acme").points {
			if labelsV(p.Payload[fieldLabelsV]) != 2 {
				t.Fatalf("a chunk kept labels_v %v", p.Payload[fieldLabelsV])
			}
		}
	})
	t.Run("two old copies end as one", func(t *testing.T) {
		svc, f, _ := retagService(t)
		// A relabel stopped between its copy and its delete left the
		// source in the base and in acme.
		var seed []map[string]any
		for _, p := range f.collection("c__acme").points {
			if str(p.Payload["source_uri"]) == "manual://plan" {
				cp := maps.Clone(p.Payload)
				cp["chunk_index"] = int(num(cp["chunk_index"]))
				seed = append(seed, cp)
			}
		}
		f.seedLayout("c", LayoutHybrid, seed...)
		n := len(seed)
		rep := retag(t, svc, "manual://plan", "globex")
		after := sourcesIn(f)
		if after["c"]["manual://plan"] != 0 || after["c__acme"]["manual://plan"] != 0 || after["c__globex"]["manual://plan"] != n {
			t.Fatalf("after the retag the collections hold %v", after)
		}
		if !slices.Equal(rep.From, []string{"acme", Unplaced}) {
			t.Fatalf("report From = %v", rep.From)
		}
	})
	t.Run("copies of different writes are refused", func(t *testing.T) {
		svc, f, _ := retagService(t)
		// globex holds another write of the same URI: which one is current
		// cannot be told, so neither is deleted.
		f.seedLayout("c__globex", LayoutHybrid, staleChunk("manual://plan", 0, "globex",
			map[string]any{fieldSpace: "globex", fieldLabelsV: float64(1), fieldIngestHash: "another write"}))
		before := sourcesIn(f)
		for _, to := range []string{"personal", "globex", "acme"} {
			if _, err := svc.Retag(ctx, RetagOpts{SourceURI: "manual://plan", Space: to, Rev: 2}); !errors.Is(err, errRetagDiverged) {
				t.Fatalf("retag to %s of diverged copies returned %v", to, err)
			}
		}
		after := sourcesIn(f)
		if after["c__acme"]["manual://plan"] != before["c__acme"]["manual://plan"] || after["c__globex"]["manual://plan"] != 1 || len(after["c__personal"]) != 0 {
			t.Fatalf("a refused retag changed the store: %v", after)
		}
	})
	t.Run("a copy without its first chunk is refused", func(t *testing.T) {
		svc, f, _ := retagService(t)
		f.seedLayout("c__globex", LayoutHybrid, staleChunk("manual://plan", 1, "globex", map[string]any{fieldSpace: "globex"}))
		if _, err := svc.Retag(ctx, RetagOpts{SourceURI: "manual://plan", Space: "globex", Rev: 2}); !errors.Is(err, errRetagDiverged) {
			t.Fatalf("a partial copy returned %v", err)
		}
	})
	t.Run("a chunk with no vector stays", func(t *testing.T) {
		svc, f := spacesService(t)
		f.seedLayout("c__acme", LayoutHybrid, staleChunk("manual://bare", 0, "acme", nil))
		f.mu.Lock()
		for id, p := range f.collections["c__acme"].points {
			p.Vector = nil
			f.collections["c__acme"].points[id] = p
		}
		f.mu.Unlock()
		_, err := svc.Retag(ctx, RetagOpts{SourceURI: "manual://bare", Space: "globex", Rev: 2})
		if err == nil || !strings.Contains(err.Error(), "ingest the source again") {
			t.Fatalf("a chunk with no vector returned %v", err)
		}
		if sourcesIn(f)["c__acme"]["manual://bare"] != 1 {
			t.Fatal("the chunk that could not move was deleted")
		}
	})
	t.Run("a failed copy keeps the old copy", func(t *testing.T) {
		svc, f, _ := retagService(t)
		f.seedLayout("c__globex", LayoutHybrid)
		f.mu.Lock()
		f.failUpserts["c__globex"] = 1
		f.mu.Unlock()
		if _, err := svc.Retag(ctx, RetagOpts{SourceURI: "manual://plan", Space: "globex", Rev: 2}); !errors.Is(err, errRetagStore) {
			t.Fatalf("a failed copy returned %v", err)
		}
		if sourcesIn(f)["c__acme"]["manual://plan"] == 0 {
			t.Fatal("a failed copy deleted the old copy")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		svc, _, _ := retagService(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := svc.Retag(cctx, RetagOpts{SourceURI: "manual://plan", Space: "globex", Rev: 2}); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled retag returned %v", err)
		}
	})
}

// TestWriteRetagReport prints counts and space names, never the source.
func TestWriteRetagReport(t *testing.T) {
	var out bytes.Buffer
	WriteRetagReport(&out, "globex", RetagReport{From: []string{"acme"}, Chunks: 3, Moved: true})
	if got := out.String(); got != "moved 3 chunk(s) to globex (was in acme)\n" {
		t.Fatalf("report = %q", got)
	}
	out.Reset()
	WriteRetagReport(&out, "acme", RetagReport{From: []string{"acme"}, Chunks: 3})
	if got := out.String(); !strings.HasPrefix(got, "stamped 3") {
		t.Fatalf("report = %q", got)
	}
}

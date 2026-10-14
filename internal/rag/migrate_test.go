package rag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// hookEmbedder embeds every text as [1,0] and runs onEmbed, once, the first
// time it sees a text containing trigger. The firing is claimed before the
// callback runs, so an Embed nested inside the callback cannot re-enter it.
type hookEmbedder struct {
	trigger string
	onEmbed func()
	fired   atomic.Bool
}

func (h *hookEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	for _, t := range texts {
		if h.trigger != "" && strings.Contains(t, h.trigger) && h.onEmbed != nil && h.fired.CompareAndSwap(false, true) {
			h.onEmbed()
		}
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (h *hookEmbedder) Dimension(context.Context) (int, error) { return 2, nil }

func words(prefix string, n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = prefix + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
	}
	return strings.Join(w, " ")
}

// seedOld writes sources as the old chunker did: overlapping word windows.
func seedOld(f *fakeQdrant) {
	all := strings.Fields(words("w", 120))
	first := strings.Join(all[:80], " ")
	second := strings.Join(all[20:], " ") // shares 60 words with the first
	f.seed("orchestrator",
		map[string]any{"source_uri": "repo://a/README.md", "source_type": "repo_doc", "scope": "work", "tags": []any{"repo:a"}, "chunk_index": 0, "text": first},
		map[string]any{"source_uri": "repo://a/README.md", "source_type": "repo_doc", "scope": "work", "tags": []any{"repo:a"}, "chunk_index": 1, "text": second},
		map[string]any{"source_uri": "manual://note", "source_type": "manual", "chunk_index": 0, "text": "remember the sync_meta table"},
		map[string]any{"source_uri": "manual://empty", "source_type": "manual", "chunk_index": 0, "text": "   "},
	)
}

func newMigrationService(t *testing.T, emb Embedder) (*Service, *fakeQdrant) {
	t.Helper()
	f, url := newFakeQdrant(t)
	seedOld(f)
	svc := NewService(emb, NewQdrantStore(url, "orchestrator", ""), 40, 4)
	svc.StateDir = t.TempDir()
	return svc, f
}

// recordCommit is a Commit that records what it was asked to switch.
type recordCommit struct{ from, to string }

func (r *recordCommit) commit(from, to string) error { r.from, r.to = from, to; return nil }

func TestMigrateHybridDryRun(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	rep, err := svc.MigrateHybrid(context.Background(), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.From != "orchestrator" || rep.To != "orchestrator-hybrid" || rep.Sources != 3 || rep.OldChunks != 4 ||
		rep.Skipped != 1 || rep.NewChunks == 0 || rep.Applied {
		t.Fatalf("report = %+v", rep)
	}
	if f.collection("orchestrator-hybrid") != nil {
		t.Fatal("a dry run created the target")
	}
	if entries, _ := os.ReadDir(svc.StateDir); len(entries) != 0 {
		t.Fatal("a dry run left a marker")
	}
}

func TestMigrateHybridApply(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	before := len(f.collection("orchestrator").points)
	var rc recordCommit
	rep, err := svc.MigrateHybrid(context.Background(), "", true, rc.commit)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.Skipped != 1 || svc.ActiveCollection() != "orchestrator-hybrid" ||
		rc.from != "orchestrator" || rc.to != "orchestrator-hybrid" {
		t.Fatalf("report = %+v, active = %s, commit = %+v", rep, svc.ActiveCollection(), rc)
	}
	dst := f.collection("orchestrator-hybrid")
	if dst == nil || dst.layout != LayoutHybrid || len(dst.points) != rep.NewChunks {
		t.Fatalf("target = %+v", dst)
	}
	if len(f.collection("orchestrator").points) != before {
		t.Fatal("the backup collection changed")
	}
	readme := dst.texts()["repo://a/README.md"]
	if n := len(strings.Fields(strings.Join(readme, " "))); n >= 120+60 || n < 120 {
		t.Fatalf("README holds %d words; the old overlap was not removed", n)
	}
	for _, p := range dst.points {
		var named map[string]json.RawMessage
		if json.Unmarshal(p.Vector, &named) != nil || named["dense"] == nil || named["bm25"] == nil {
			t.Fatalf("point vector = %s", p.Vector)
		}
		if str(p.Payload["source_uri"]) == "repo://a/README.md" && (str(p.Payload["scope"]) != "work" || str(p.Payload["source_type"]) != "repo_doc") {
			t.Fatalf("provenance lost: %+v", p.Payload)
		}
	}
	if entries, _ := os.ReadDir(svc.StateDir); len(entries) != 0 {
		t.Fatal("a finished migration left its marker")
	}
	// Later writes go to the new collection only.
	if _, err := svc.IngestText(context.Background(), "after the switch", Meta{SourceURI: "manual://later"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := dst.texts()["manual://later"]; !ok {
		t.Fatal("write after the switch missed the new collection")
	}
	if _, ok := f.collection("orchestrator").texts()["manual://later"]; ok {
		t.Fatal("write after the switch reached the backup")
	}
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err == nil || !strings.Contains(err.Error(), "already hybrid") {
		t.Fatalf("rerun err = %v", err)
	}
}

func TestMigrateHybridLiveIngestWins(t *testing.T) {
	emb := &hookEmbedder{trigger: "waa"} // the README's first word: fires while it is copied
	svc, f := newMigrationService(t, emb)
	var wg sync.WaitGroup
	emb.onEmbed = func() {
		// A live ingest of the very source being copied, from another
		// goroutine as in orchd: it waits for the copy's lock on the source.
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.IngestText(context.Background(), "the newest readme", Meta{SourceURI: "repo://a/README.md", SourceType: "repo_doc"}); err != nil {
				t.Error(err)
			}
		}()
	}
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !emb.fired.Load() {
		t.Fatal("the live ingest never ran")
	}
	for _, name := range []string{"orchestrator-hybrid", "orchestrator"} {
		if got := strings.Join(f.collection(name).texts()["repo://a/README.md"], " "); got != "the newest readme" {
			t.Fatalf("%s holds %q; the live write must win", name, got)
		}
	}
}

func TestShadowFailureIsRecopied(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	dst := svc.active().WithCollection("orchestrator-hybrid")
	if err := dst.EnsureCollection(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	svc.setShadow(dst)
	f.mu.Lock()
	f.failUpserts["orchestrator-hybrid"] = 1
	f.mu.Unlock()
	// Reaches the active store, fails on the shadow: the source must not be
	// marked done, so the copy brings it over from the active store.
	if _, err := svc.IngestText(context.Background(), "written during migration", Meta{SourceURI: "manual://flaky"}); err != nil {
		t.Fatalf("a shadow failure surfaced to the caller: %v", err)
	}
	svc.mu.RLock()
	done := svc.touched["manual://flaky"]
	svc.mu.RUnlock()
	if done {
		t.Fatal("a failed shadow write was marked done")
	}
	svc.setShadow(nil)
	if err := os.WriteFile(svc.markerPath("orchestrator-hybrid"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.collection("orchestrator-hybrid").texts()["manual://flaky"], " "); got != "written during migration" {
		t.Fatalf("recopied text = %q", got)
	}
}

func TestMigrateHybridResumeAndRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("an interrupted run resumes and removes what no longer exists", func(t *testing.T) {
		svc, f := newMigrationService(t, &hookEmbedder{})
		dst := svc.active().WithCollection("orchestrator-hybrid")
		if err := dst.EnsureCollection(ctx, 2); err != nil {
			t.Fatal(err)
		}
		// Left by the interrupted run: a source since forgotten.
		if err := dst.Upsert(ctx, []Point{{ID: pointID("manual://forgotten", 0), Vector: []float32{1, 0},
			Payload: map[string]any{"source_uri": "manual://forgotten", "chunk_index": 0, "text": "gone"}}}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(svc.markerPath("orchestrator-hybrid"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		rep, err := svc.MigrateHybrid(ctx, "", true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Removed != 1 {
			t.Fatalf("removed = %d", rep.Removed)
		}
		if _, ok := f.collection("orchestrator-hybrid").texts()["manual://forgotten"]; ok {
			t.Fatal("a forgotten source came back")
		}
	})
	t.Run("a finished target is refused", func(t *testing.T) {
		svc, _ := newMigrationService(t, &hookEmbedder{})
		dst := svc.active().WithCollection("orchestrator-hybrid")
		if err := dst.EnsureCollection(ctx, 2); err != nil {
			t.Fatal(err)
		}
		if err := dst.Upsert(ctx, []Point{{ID: pointID("x", 0), Vector: []float32{1, 0}, Payload: map[string]any{"source_uri": "x"}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.MigrateHybrid(ctx, "", true, nil); err == nil || !strings.Contains(err.Error(), "finished migration") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("name and layout refusals", func(t *testing.T) {
		svc, f := newMigrationService(t, &hookEmbedder{})
		f.seed("taken", map[string]any{"source_uri": "x", "chunk_index": 0, "text": "x"})
		for target, want := range map[string]string{
			"orchestrator": "must differ",
			"bad/name":     "must be letters",
			"taken":        "not hybrid",
		} {
			if _, err := svc.MigrateHybrid(ctx, target, false, nil); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("target %q: err = %v, want %q", target, err, want)
			}
		}
		empty := NewService(&hookEmbedder{}, svc.active().WithCollection("absent"), 40, 4)
		if _, err := empty.MigrateHybrid(ctx, "", false, nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("missing source err = %v", err)
		}
		noState := NewService(&hookEmbedder{}, svc.active(), 40, 4)
		if _, err := noState.MigrateHybrid(ctx, "", true, nil); err == nil || !strings.Contains(err.Error(), "state directory") {
			t.Errorf("no state dir err = %v", err)
		}
	})
	t.Run("a failed commit does not switch", func(t *testing.T) {
		svc, _ := newMigrationService(t, &hookEmbedder{})
		_, err := svc.MigrateHybrid(ctx, "", true, func(string, string) error { return errors.New("disk full") })
		if err == nil || !strings.Contains(err.Error(), "searches stay on orchestrator") {
			t.Fatalf("err = %v", err)
		}
		if svc.ActiveCollection() != "orchestrator" {
			t.Fatal("switched despite the failed commit")
		}
		if _, shadow := svc.stores(); shadow != nil {
			t.Fatal("dual writes left on")
		}
		if _, err := os.Stat(svc.markerPath("orchestrator-hybrid")); err != nil {
			t.Fatal("the marker must stay, so the run can resume")
		}
	})
	t.Run("a cancelled run leaves the active store and stops dual writes", func(t *testing.T) {
		svc, _ := newMigrationService(t, &hookEmbedder{})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := svc.MigrateHybrid(cctx, "", true, nil); err == nil {
			t.Fatal("cancelled run reported success")
		}
		if active, shadow := svc.stores(); active.Collection() != "orchestrator" || shadow != nil {
			t.Fatal("a failed run switched collections or kept dual writes")
		}
	})
	t.Run("oversized sources block the switch", func(t *testing.T) {
		svc, _ := newMigrationService(t, &hookEmbedder{})
		old := maxSourceBytes
		maxSourceBytes = 50
		t.Cleanup(func() { maxSourceBytes = old })
		rep, err := svc.MigrateHybrid(ctx, "", true, nil)
		if err == nil || rep.Oversize == 0 || svc.ActiveCollection() != "orchestrator" || len(rep.Problems) == 0 {
			t.Fatalf("err = %v, report = %+v", err, rep)
		}
	})
}

func TestMalformedAndOrphans(t *testing.T) {
	f, url := newFakeQdrant(t)
	f.seed("orchestrator",
		map[string]any{"source_uri": "manual://noindex", "text": "first part"},
		map[string]any{"source_uri": "manual://noindex", "text": "second part"},
		map[string]any{"text": "belongs to nothing"},
	)
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "orchestrator", ""), 40, 4)
	svc.StateDir = t.TempDir()
	rep, err := svc.MigrateHybrid(context.Background(), "", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Malformed != 1 || rep.Orphans != 1 || len(rep.Problems) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	got := strings.Join(f.collection("orchestrator-hybrid").texts()["manual://noindex"], " ")
	if !strings.Contains(got, "first part") || !strings.Contains(got, "second part") {
		t.Fatalf("malformed source lost text: %q", got)
	}
}

func TestDeleteDuringMigrationReachesBoth(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	dst := svc.active().WithCollection("orchestrator-hybrid")
	if err := dst.EnsureCollection(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	svc.setShadow(dst)
	if _, err := svc.IngestText(context.Background(), "doomed text", Meta{SourceURI: "manual://doomed"}); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.DeleteSource(context.Background(), "manual://doomed"); err != nil || n == 0 {
		t.Fatalf("delete = %d, %v", n, err)
	}
	for _, name := range []string{"orchestrator", "orchestrator-hybrid"} {
		if _, ok := f.collection(name).texts()["manual://doomed"]; ok {
			t.Fatalf("source survived in %s", name)
		}
	}
}

func TestRebuild(t *testing.T) {
	got, malformed := rebuild([]storedChunk{
		{index: 1, text: "[section: Guide › Setup]\n\n## Setup\n\nrun make"},
		{index: 0, text: "# Guide\n\nintro text"},
	})
	if malformed || got != "# Guide\n\nintro text\n\n## Setup\n\nrun make" {
		t.Fatalf("rebuild = %q", got)
	}
	// A split section repeats its heading on each piece; it is kept once.
	got, _ = rebuild([]storedChunk{
		{index: 0, text: "[section: Doc]\n\n## Install\n\nfirst paragraph"},
		{index: 1, text: "[section: Doc]\n\n## Install\n\nsecond paragraph"},
	})
	if strings.Count(got, "## Install") != 1 || !strings.Contains(got, "second paragraph") {
		t.Fatalf("repeated heading = %q", got)
	}
	// Overlap is cut by position, so line breaks and code after it survive.
	shared := words("s", 10)
	got, _ = rebuild([]storedChunk{
		{index: 0, text: "start " + shared},
		{index: 1, text: shared + "\n\n```go\nfunc f() {\n\treturn\n}\n```"},
	})
	if !strings.Contains(got, "```go\nfunc f() {\n\treturn\n}\n```") || strings.Count(got, "saa") != 1 {
		t.Fatalf("overlap cut = %q", got)
	}
	if got, _ := rebuild([]storedChunk{{index: 0, text: "a b c"}, {index: 1, text: "c d e"}}); got != "a b c\n\nc d e" {
		t.Fatalf("short repeat = %q", got)
	}
	all := strings.Fields(words("w", 120))
	text, _ := rebuild([]storedChunk{{index: 0, text: strings.Join(all[:80], " ")}, {index: 1, text: strings.Join(all[20:], " ")}})
	if strings.Join(strings.Fields(text), " ") != strings.Join(all, " ") {
		t.Fatal("old windows did not rebuild to the original words")
	}
	if _, malformed := rebuild([]storedChunk{{index: 0, text: "a"}, {index: 0, text: "b"}}); !malformed {
		t.Fatal("a repeated index was not flagged")
	}
	if cutWords("one two", 5) != "" || overlap([]string{"x"}, nil) != 0 {
		t.Fatal("edge cases")
	}
}

func TestLayoutResetsWhenCollectionVanishes(t *testing.T) {
	f, url := newFakeQdrant(t)
	st := NewQdrantStore(url, "c", "")
	if err := st.EnsureCollection(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.collections, "c")
	f.mu.Unlock()
	if err := st.Upsert(context.Background(), []Point{{ID: "a", Vector: []float32{1, 0}}}); err == nil {
		t.Fatal("upsert into a vanished collection succeeded")
	}
	if err := writePoints(context.Background(), st, 2, "x", []Point{{ID: pointID("x", 0), Vector: []float32{1, 0}, Payload: map[string]any{"source_uri": "x", "chunk_index": 0}}}); err != nil {
		t.Fatalf("the next write did not recreate the collection: %v", err)
	}
}

// A live write to a source the copy has already done, failing on the target,
// must be copied again before the switch, not left stale.
func TestFailedWriteAfterCopyIsRecopied(t *testing.T) {
	emb := &hookEmbedder{trigger: "waa"} // fires while the README (copied last) is copied
	svc, f := newMigrationService(t, emb)
	if svc.stripe("manual://note") == svc.stripe("repo://a/README.md") {
		t.Fatal("test sources share a lock stripe; pick other names")
	}
	emb.onEmbed = func() {
		// manual://note is already copied. Its live update reaches the active
		// store but fails on the target.
		f.mu.Lock()
		f.failUpserts["orchestrator-hybrid"] = 1
		f.mu.Unlock()
		if _, err := svc.IngestText(context.Background(), "the updated note", Meta{SourceURI: "manual://note", SourceType: "manual"}); err != nil {
			t.Error(err)
		}
	}
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err != nil {
		t.Fatal(err)
	}
	if !emb.fired.Load() {
		t.Fatal("the live write never ran")
	}
	if got := strings.Join(f.collection("orchestrator-hybrid").texts()["manual://note"], " "); got != "the updated note" {
		t.Fatalf("the switched collection holds %q; the failed write was not re-copied", got)
	}
}

func TestMarkerClearedBeforeCommit(t *testing.T) {
	svc, _ := newMigrationService(t, &hookEmbedder{})
	sawMarker := true
	if _, err := svc.MigrateHybrid(context.Background(), "", true, func(string, string) error {
		_, err := os.Stat(svc.markerPath("orchestrator-hybrid"))
		sawMarker = err == nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sawMarker {
		t.Fatal("the marker still existed when the switch was recorded")
	}
}

func TestSameTitleSectionsAreNotMerged(t *testing.T) {
	got, _ := rebuild([]storedChunk{
		{index: 0, text: "[section: Guide › Install]\n\n## Example\n\nfirst example"},
		{index: 1, text: "[section: Guide › Usage]\n\n## Example\n\nsecond example"},
	})
	if strings.Count(got, "## Example") != 2 {
		t.Fatalf("two sections with one title were merged: %q", got)
	}
}

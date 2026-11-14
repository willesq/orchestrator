package rag

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/willesq/orchestrator/internal/bm25"
	"github.com/willesq/orchestrator/internal/spaces"
)

// seedSpaces adds hybrid space collections under base, each holding one
// source labelled with its space.
func seedSpaces(f *fakeQdrant, base string, names ...string) {
	for _, sp := range names {
		f.seedLayout(SpaceCollection(base, sp), LayoutHybrid,
			map[string]any{"source_uri": "manual://" + sp, "source_type": "manual", "chunk_index": 0, "text": "the notes of " + sp,
				"space": sp, "labels_v": float64(2)})
	}
}

// TestMigrateHybridCopiesEverySpaceCollection: SP-024.2.a1. A migration copies
// the base and every space collection to the same space's collection under
// the target, and switches them together: after it, a write placed in a space
// lands under the new base.
func TestMigrateHybridCopiesEverySpaceCollection(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	seedSpaces(f, "orchestrator", "acme", spaces.Quarantine)
	ctx := context.Background()

	plan, err := svc.MigrateHybrid(ctx, "", false, nil)
	if err != nil || plan.Sources != 5 || plan.OldChunks != 6 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	var rc recordCommit
	rep, err := svc.MigrateHybrid(ctx, "", true, rc.commit)
	if err != nil || !rep.Applied || rep.Sources != 5 {
		t.Fatalf("apply = %+v, %v", rep, err)
	}
	if rc.from != "orchestrator" || rc.to != "orchestrator-hybrid" {
		t.Fatalf("commit = %+v", rc)
	}
	for _, sp := range []string{"acme", spaces.Quarantine} {
		dst := f.collection("orchestrator-hybrid__" + sp)
		if dst == nil || dst.layout != LayoutHybrid {
			t.Fatalf("%s was not copied: %+v", sp, dst)
		}
		if got := dst.texts()["manual://"+sp]; len(got) != 1 || got[0] != "the notes of "+sp {
			t.Fatalf("%s's copy holds %v", sp, got)
		}
		for _, p := range dst.points {
			if str(p.Payload[fieldSpace]) != sp || labelsV(p.Payload[fieldLabelsV]) != 2 {
				t.Fatalf("%s's copy lost its placement: %v", sp, p.Payload)
			}
		}
		if len(f.collection("orchestrator__"+sp).points) != 1 {
			t.Fatalf("the old %s collection was modified", sp)
		}
	}
	if got := len(f.collection("orchestrator-hybrid").texts()); got != 2 {
		t.Fatalf("the base copy holds %d sources, want 2", got)
	}
	svc.Place = placeBy
	ingestIn(t, svc, "acme", "manual://after", "written after the switch")
	if f.collection("orchestrator-hybrid__acme").texts()["manual://after"] == nil {
		t.Fatal("a write after the switch did not reach acme's collection under the new base")
	}
	if f.collection("orchestrator__acme").texts()["manual://after"] != nil {
		t.Fatal("a write after the switch reached the old base's acme collection")
	}
}

// TestMigrateHybridLiveWriteToANewSpace: a write placed in a space with no
// collection yet, made while the copy runs, reaches that space's collection
// under both bases, so nothing is lost at the switch.
func TestMigrateHybridLiveWriteToANewSpace(t *testing.T) {
	emb := &hookEmbedder{trigger: "waa"}
	svc, f := newMigrationService(t, emb)
	svc.Place = placeBy
	var wg sync.WaitGroup
	emb.onEmbed = func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ingestIn(t, svc, "globex", "manual://globex-live", "globex wrote this mid-migration")
		}()
	}
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if f.collection("orchestrator-hybrid__globex").texts()["manual://globex-live"] == nil {
		t.Fatal("the live write is missing from globex's collection under the new base")
	}
}

// TestMigrateHybridRefusesAFinishedSpaceTarget: a space collection under the
// target that holds points with no marker is a finished migration; copying
// into it again would revert writes, so the run is refused. A target named
// like a space collection is refused too.
func TestMigrateHybridRefusesAFinishedSpaceTarget(t *testing.T) {
	svc, f := newMigrationService(t, &hookEmbedder{})
	seedSpaces(f, "orchestrator", "acme")
	seedSpaces(f, "orchestrator-hybrid", "acme")
	if _, err := svc.MigrateHybrid(context.Background(), "", true, nil); err == nil || !strings.Contains(err.Error(), "finished migration") {
		t.Fatalf("err = %v", err)
	}
	f.seed("orchestrator-next__acme")
	if _, err := svc.MigrateHybrid(context.Background(), "orchestrator-next", false, nil); err == nil || !strings.Contains(err.Error(), "not hybrid") {
		t.Fatalf("a dense space target: %v", err)
	}
	if _, err := svc.MigrateHybrid(context.Background(), "orchestrator__x", false, nil); err == nil || !strings.Contains(err.Error(), spaceSep) {
		t.Fatalf("a target named like a space collection: %v", err)
	}
}

// TestReindexKeywordsCoversEverySpaceCollection: SP-024.2.a1. A keyword
// re-index counts and re-indexes the stale chunks of the base and of every
// space collection; a dense space collection is refused.
func TestReindexKeywordsCoversEverySpaceCollection(t *testing.T) {
	f, svc := hybridFake(t, 5)
	seedSpaces(f, "c", "acme", spaces.Quarantine)
	ctx := context.Background()
	plan, err := svc.ReindexKeywords(ctx, false)
	if err != nil || plan.Total != 8 || plan.Stale != 8 || plan.Collection != "c" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	rep, err := svc.ReindexKeywords(ctx, true)
	if err != nil || rep.Updated != 8 {
		t.Fatalf("apply = %+v, %v", rep, err)
	}
	for _, name := range []string{"c", "c__acme", "c__quarantine"} {
		for id, p := range f.collection(name).points {
			if v, _ := p.Payload[fieldBM25Version].(float64); int(v) != bm25.TokenizerVersion {
				t.Fatalf("%s/%s not re-indexed", name, id)
			}
		}
	}
	f.seed("c__globex", map[string]any{"source_uri": "manual://g", "chunk_index": 0, "text": "dense"})
	svc.lists = nil
	if _, err := svc.ReindexKeywords(ctx, false); err == nil || !strings.Contains(err.Error(), "c__globex") {
		t.Fatalf("a dense space collection: %v", err)
	}
}

// TestDropBackupDropsEverySpaceCollection: SP-024.2.a1. Dropping a backup
// drops its space collections with it, leaves the active base's alone, and
// refuses a collection that is one of the active base's spaces.
func TestDropBackupDropsEverySpaceCollection(t *testing.T) {
	f, url := newFakeQdrant(t)
	f.seed("backup", map[string]any{"source_uri": "manual://old", "chunk_index": 0, "text": "old"})
	seedSpaces(f, "backup", "acme", spaces.Quarantine)
	seedSpaces(f, "c", "acme")
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "c", ""), 40, 4)
	svc.StateDir = t.TempDir()
	ctx := context.Background()

	plan, err := svc.DropCollection(ctx, "backup", false)
	if err != nil || plan.Points != 3 || plan.Dropped || !slices.Equal(plan.Spaces, []string{"acme", spaces.Quarantine}) {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	rep, err := svc.DropCollection(ctx, "backup", true)
	if err != nil || !rep.Dropped {
		t.Fatalf("drop = %+v, %v", rep, err)
	}
	for _, n := range []string{"backup", "backup__acme", "backup__quarantine"} {
		if f.collection(n) != nil {
			t.Fatalf("%s survived the drop", n)
		}
	}
	if f.collection("c__acme") == nil {
		t.Fatal("the drop reached the active base's space collection")
	}
	if _, err := svc.DropCollection(ctx, "c__acme", true); err == nil || !strings.Contains(err.Error(), "is using") {
		t.Fatalf("dropping an active space collection: %v", err)
	}
	if _, err := svc.DropCollection(ctx, "backup", false); !errors.Is(err, ErrNoCollection) {
		t.Fatalf("a dropped backup: %v", err)
	}
	// A backup whose base is gone still has its space collections dropped.
	seedSpaces(f, "older", "acme")
	if rep, err := svc.DropCollection(ctx, "older", true); err != nil || !rep.Dropped || f.collection("older__acme") != nil {
		t.Fatalf("drop of space collections only = %+v, %v", rep, err)
	}
}

// TestWriteCacheIsKeyedBySpace: SP-024.2.a2. The unchanged-write check is a
// cache of what is stored, and it is read in the write's own space only: the
// same source and text stored for acme is never taken as stored for globex,
// and the other way round, so each write is embedded and lands in its space.
func TestWriteCacheIsKeyedBySpace(t *testing.T) {
	svc, f := spacesService(t)
	var embeds int
	emb := &hookEmbedder{}
	svc.emb = embedCounter{Embedder: emb, n: &embeds}
	text := "the shared runbook text"
	ingestIn(t, svc, "acme", "manual://runbook", text)
	ingestIn(t, svc, "acme", "manual://runbook", text)
	if embeds != 1 {
		t.Fatalf("an unchanged write in the same space was embedded again (%d embeds)", embeds)
	}
	ingestIn(t, svc, "globex", "manual://runbook", text)
	if embeds != 2 {
		t.Fatal("a write cached as stored for acme was skipped for globex")
	}
	if f.collection("c__globex").texts()["manual://runbook"] == nil || len(f.collection("c__acme").points) != 0 {
		t.Fatal("the globex write did not move the source")
	}
	ingestIn(t, svc, "acme", "manual://runbook", text)
	if embeds != 3 || f.collection("c__acme").texts()["manual://runbook"] == nil {
		t.Fatal("a write cached as stored for globex was skipped for acme")
	}
}

// embedCounter counts Embed calls.
type embedCounter struct {
	Embedder
	n *int
}

func (c embedCounter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	*c.n++
	return c.Embedder.Embed(ctx, texts)
}

// TestStoreCachesAreKeyedByCollection: SP-024.2.a2. Each space's store keeps
// its own cached layout, and the list of space collections is cached per
// base: a layout known for acme's collection is never used for globex's, and
// a list cached for one base is never read for another.
func TestStoreCachesAreKeyedByCollection(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "acme", "manual://a", "acme's note")
	acme, _ := svc.storeFor("acme")
	globex, _ := svc.storeFor("globex")
	if acme == globex || acme.Collection() != "c__acme" || globex.Collection() != "c__globex" {
		t.Fatalf("stores %s and %s", acme.Collection(), globex.Collection())
	}
	if again, _ := svc.storeFor("acme"); again != acme {
		t.Fatal("acme's store is not reused")
	}
	if l, _ := acme.Layout(context.Background()); l != LayoutHybrid {
		t.Fatalf("acme layout = %v", l)
	}
	if l, _ := globex.Layout(context.Background()); l != LayoutUnknown {
		t.Fatalf("globex's store took acme's layout: %v", l)
	}
	names, err := svc.spaceCollections(context.Background(), svc.active(), false)
	if err != nil || !slices.Equal(names, []string{"", "acme"}) {
		t.Fatalf("c's spaces = %v, %v", names, err)
	}
	seedSpaces(f, "other", "globex")
	other := svc.active().WithCollection("other")
	if names, err := svc.spaceCollections(context.Background(), other, false); err != nil || !slices.Equal(names, []string{"", "globex"}) {
		t.Fatalf("other's spaces = %v, %v; c's list was served for another base", names, err)
	}
	if names, _ := svc.spaceCollections(context.Background(), svc.active(), false); !slices.Equal(names, []string{"", "acme"}) {
		t.Fatalf("c's cached list became %v", names)
	}
}

// TestDeleteAndReadsCoverEverySpace: a forget removes the source from every
// space; inventory, documents and the writer listing see every space.
func TestDeleteAndReadsCoverEverySpace(t *testing.T) {
	svc, f := spacesService(t)
	ctx := context.Background()
	ingestIn(t, svc, "acme", "manual://a", "acme's note")
	m := Meta{SourceURI: "repo://o/r/x.md", SourceType: "repo_doc", Tags: []string{"oss"}, Writer: WriterSources, Origin: Origin{System: OriginFS}}
	if _, err := svc.IngestText(ctx, "a synced doc", m); err != nil {
		t.Fatal(err)
	}
	// A stale copy left in a second space (written before moves existed).
	f.collection("c__acme").points["stale"] = fakePoint{ID: "stale", Payload: map[string]any{
		"source_uri": "repo://o/r/x.md", "chunk_index": float64(5), "text": "stale", "writer": WriterSources}}

	list, distinct, chunks, err := svc.ListSources(ctx, ListSourcesOpts{})
	if err != nil || distinct != 2 || chunks != 3 || len(list) != 2 {
		t.Fatalf("ListSources = %v %d %d %v", list, distinct, chunks, err)
	}
	if doc, err := svc.Document(ctx, "manual://a"); err != nil || doc.Text != "acme's note" {
		t.Fatalf("Document = %+v %v", doc, err)
	}
	if _, err := svc.Document(ctx, "manual://none"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("a missing document: %v", err)
	}
	if uris, err := svc.SourcesWrittenBy(ctx, "repo://", WriterSources); err != nil || !slices.Equal(uris, []string{"repo://o/r/x.md"}) {
		t.Fatalf("SourcesWrittenBy = %v %v", uris, err)
	}
	if ok, err := svc.HasSource(ctx, "manual://a"); err != nil || !ok {
		t.Fatalf("HasSource = %v %v", ok, err)
	}
	if ok, err := svc.HasSource(ctx, "manual://none"); err != nil || ok {
		t.Fatalf("HasSource(none) = %v %v", ok, err)
	}
	n, err := svc.DeleteSource(ctx, "repo://o/r/x.md")
	if err != nil || n != 2 {
		t.Fatalf("DeleteSource = %d %v", n, err)
	}
	for _, name := range []string{"c__oss", "c__acme"} {
		if f.collection(name).texts()["repo://o/r/x.md"] != nil {
			t.Fatalf("%s still holds the forgotten source", name)
		}
	}
	if ok, _ := svc.HasSource(ctx, "manual://a"); !ok {
		t.Fatal("the forget reached another source")
	}
}

// TestSpaceListFailures: a failed listing fails a write to a space and a
// forget, rather than leaving copies behind, while a search degrades to the
// base collection.
func TestSpaceListFailures(t *testing.T) {
	svc, f := spacesService(t)
	ctx := context.Background()
	ingestIn(t, svc, "acme", "manual://a", "acme's note")
	broken := NewService(&hookEmbedder{}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 40, 4)
	broken.Place = placeBy
	if _, err := broken.IngestText(ctx, "x", Meta{SourceURI: "manual://x", Tags: []string{"acme"}, Origin: Origin{System: OriginClient}}); err == nil {
		t.Fatal("a write to a space went ahead without the list of collections")
	}
	if _, err := broken.DeleteSource(ctx, "manual://x"); err == nil {
		t.Fatal("a forget went ahead without the list of collections")
	}
	for _, read := range []func() error{
		func() error { _, _, _, err := broken.ListSources(ctx, ListSourcesOpts{}); return err },
		func() error { _, err := broken.HasSource(ctx, "manual://x"); return err },
		func() error { _, _, err := broken.Copy(ctx, "manual://x"); return err },
		func() error { _, err := broken.Document(ctx, "manual://x"); return err },
		func() error { _, err := broken.SourcesWrittenBy(ctx, "", WriterSources); return err },
	} {
		if err := read(); err == nil {
			t.Fatal("a read of every space succeeded without the list of collections")
		}
	}
	_ = f
	if _, _, err := broken.Search(ctx, "note", SearchOpts{Mode: ModeDense}); err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("search = %v", err)
	}
	if got := fmt.Sprint(svc.ActiveCollection()); got != "c" {
		t.Fatalf("active = %s", got)
	}
}

// TestListingNeverHidesANotedCollection: a listing answered before a write
// created a space's collection does not hide that collection once cached.
func TestListingNeverHidesANotedCollection(t *testing.T) {
	svc, f := spacesService(t)
	base := svc.active()
	f.onList = func() {
		// The write lands while the listing is in flight; Qdrant answered
		// without it.
		f.onList = nil
		svc.noteCollection(base, "acme")
	}
	names, err := svc.spaceCollections(context.Background(), base, true)
	if err != nil || !slices.Contains(names, "acme") {
		t.Fatalf("spaces = %v, %v", names, err)
	}
	if names, _ := svc.spaceCollections(context.Background(), base, false); !slices.Contains(names, "acme") {
		t.Fatalf("the cached list lost acme: %v", names)
	}
	svc.forgetCollections(base.Collection())
	if names, _ := svc.spaceCollections(context.Background(), base, true); slices.Contains(names, "acme") {
		t.Fatalf("a dropped base still lists acme: %v", names)
	}
}

// TestDropRefusesABaseThatReadsAsASpace: when the active collection's name
// reads as a space collection of the one asked for (x__y under x), the drop
// is refused rather than delete the active store.
func TestDropRefusesABaseThatReadsAsASpace(t *testing.T) {
	f, url := newFakeQdrant(t)
	f.seed("orchestrator")
	seedSpaces(f, "orchestrator", "v2")
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "orchestrator__v2", ""), 40, 4)
	svc.StateDir = t.TempDir()
	if _, err := svc.DropCollection(context.Background(), "orchestrator", true); err == nil || !strings.Contains(err.Error(), "is using") {
		t.Fatalf("err = %v", err)
	}
	if f.collection("orchestrator__v2") == nil {
		t.Fatal("the active collection was dropped")
	}
}

// TestCopyReportsTheCopyThatRefusesMost: should a source be found in more
// than one collection, Copy reports a sync writer's copy first, then one
// placed in a space, so a client cannot replace a synced doc through a
// stray base copy.
func TestCopyReportsTheCopyThatRefusesMost(t *testing.T) {
	svc, f := spacesService(t)
	ctx := context.Background()
	f.seedLayout("c", LayoutHybrid, map[string]any{"source_uri": "repo://o/r/a.md", "chunk_index": 0, "text": "a client's copy", "writer": WriterClient})
	f.seedLayout("c__acme", LayoutHybrid, map[string]any{"source_uri": "repo://o/r/a.md", "chunk_index": 0, "text": "the synced doc",
		"writer": WriterDocs, "space": "acme"})
	f.seedLayout("c__globex", LayoutHybrid, map[string]any{"source_uri": "manual://n", "chunk_index": 0, "text": "a note", "writer": WriterClient, "space": "globex"})
	f.seedLayout("c__oss", LayoutHybrid)
	if cp, ok, err := svc.Copy(ctx, "repo://o/r/a.md"); err != nil || !ok || cp.Writer != WriterDocs || cp.Space != "acme" {
		t.Fatalf("Copy = %+v %v %v", cp, ok, err)
	}
	f.collection("c").points[pointID("manual://n", 0)] = fakePoint{ID: pointID("manual://n", 0), Payload: map[string]any{
		"source_uri": "manual://n", "chunk_index": float64(0), "writer": WriterClient}}
	if cp, ok, err := svc.Copy(ctx, "manual://n"); err != nil || !ok || cp.Space != "globex" {
		t.Fatalf("Copy = %+v %v %v", cp, ok, err)
	}
}

// TestClientWriteRefusesABaseCopyOfAnotherSpace: a base chunk that SP-023
// placed in another space is that space's; a client cannot move it.
func TestClientWriteRefusesABaseCopyOfAnotherSpace(t *testing.T) {
	svc, f := spacesService(t)
	f.seedLayout("c", LayoutHybrid, map[string]any{"source_uri": "manual://plan", "chunk_index": 0, "text": "globex's plan", "space": "globex"})
	m := Meta{SourceURI: "manual://plan", Tags: []string{"acme"}, Writer: WriterClient, Origin: Origin{System: OriginClient}}
	if _, err := svc.IngestText(context.Background(), "acme's overwrite", m); !errors.Is(err, ErrStoredInAnotherSpace) {
		t.Fatalf("err = %v", err)
	}
	if f.collection("c").texts()["manual://plan"] == nil {
		t.Fatal("the base copy was deleted")
	}
	// The same space's own pre-split copy moves into its collection.
	m.Tags = []string{"globex"}
	if _, err := svc.IngestText(context.Background(), "globex's new plan", m); err != nil {
		t.Fatal(err)
	}
	if f.collection("c").texts()["manual://plan"] != nil || f.collection("c__globex").texts()["manual://plan"] == nil {
		t.Fatal("globex's own copy did not move")
	}
}

// TestSharedWriteClearsSpaceCopies: once a policy is removed, a write is
// Shared and lands in the base; a copy a policy put in a space collection is
// removed, so the source lives in one collection. A failed listing does not
// fail a Shared write.
func TestSharedWriteClearsSpaceCopies(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "acme", "manual://doc", "under the policy")
	svc.Place = placeAll(spaces.Shared, 0, nil)
	if _, err := svc.IngestText(context.Background(), "after the policy", Meta{SourceURI: "manual://doc", Origin: Origin{System: OriginClient}}); err != nil {
		t.Fatal(err)
	}
	if f.collection("c__acme").texts()["manual://doc"] != nil || f.collection("c").texts()["manual://doc"] == nil {
		t.Fatal("the shared write did not move the source to the base")
	}
	broken := NewService(&hookEmbedder{}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 40, 4)
	if err := broken.removeElsewhere(context.Background(), Meta{SourceURI: "manual://x", Space: spaces.Shared}); err != nil {
		t.Fatalf("a shared write failed on the listing: %v", err)
	}
}

// TestExplicitHybridErrorNamesNoSpace: an explicit hybrid search that meets
// a collection without a keyword index names the base, never a space's
// collection.
func TestExplicitHybridErrorNamesNoSpace(t *testing.T) {
	svc, f := spacesService(t)
	ingestIn(t, svc, "oss", "manual://o", "oss note")
	f.seed("c__acme", map[string]any{"source_uri": "manual://a", "chunk_index": 0, "text": "dense acme note", "space": "acme"})
	_, _, err := svc.Search(context.Background(), "note", SearchOpts{Spaces: []string{"acme", "oss"}, Mode: ModeHybrid})
	if !errors.Is(err, ErrHybridUnavailable) || strings.Contains(err.Error(), "acme") {
		t.Fatalf("err = %v", err)
	}
	f.seed("c", map[string]any{"source_uri": "manual://b", "chunk_index": 0, "text": "dense base note"})
	_, _, err = svc.Search(context.Background(), "note", SearchOpts{Spaces: []string{spaces.Shared, "oss"}, Mode: ModeHybrid})
	if !errors.Is(err, ErrHybridUnavailable) || !strings.Contains(err.Error(), `collection "c"`) {
		t.Fatalf("err = %v", err)
	}
}

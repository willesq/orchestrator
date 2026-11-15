package rag

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// quarantineFixture is a store whose quarantine holds q1 (2 chunks) and q2
// (1 chunk), with a third source in space "work".
func quarantineFixture(t *testing.T) (*Service, *fakeQdrant) {
	t.Helper()
	svc, f := datedService(t)
	at := float64(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix())
	f.seed(SpaceCollection("c", spaces.Quarantine),
		map[string]any{"source_uri": "file:///q1", "source_type": "file", "chunk_index": 0, fieldIngestedAt: at},
		map[string]any{"source_uri": "file:///q1", "source_type": "file", "chunk_index": 1, fieldIngestedAt: at + 60},
		map[string]any{"source_uri": "https://q2", "source_type": "web", "chunk_index": 0},
	)
	f.seed(SpaceCollection("c", "work"),
		map[string]any{"source_uri": "file:///w", "source_type": "file", "chunk_index": 0},
	)
	return svc, f
}

func TestListQuarantineShowsSourcesAndCounts(t *testing.T) {
	svc, _ := quarantineFixture(t)
	got, total, err := svc.ListQuarantine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(got) != 2 {
		t.Fatalf("total=%d sources=%v", total, got)
	}
	want := time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC).Unix()
	if got[0].SourceURI != "file:///q1" || got[0].Chunks != 2 || got[0].SourceType != "file" || got[0].IngestedAt != want {
		t.Fatalf("q1 = %+v", got[0])
	}
	if got[1].SourceURI != "https://q2" || got[1].Chunks != 1 || got[1].IngestedAt != 0 {
		t.Fatalf("q2 = %+v", got[1])
	}
}

func TestListQuarantineWithNoCollectionIsEmpty(t *testing.T) {
	svc, _ := datedService(t)
	got, total, err := svc.ListQuarantine(context.Background())
	if err != nil || total != 0 || len(got) != 0 {
		t.Fatalf("got %v %d %v", got, total, err)
	}
}

func TestPurgeQuarantineCountsUnlessApplied(t *testing.T) {
	svc, f := quarantineFixture(t)
	ctx := context.Background()
	res, err := svc.PurgeQuarantine(ctx, nil, true, false)
	if err != nil || res != (PurgeResult{Sources: 2, Chunks: 3}) {
		t.Fatalf("dry run = %+v %v", res, err)
	}
	for _, r := range f.requests() {
		if isWrite(r) {
			t.Fatalf("dry run wrote: %+v", r)
		}
	}
	if n := len(f.collection(SpaceCollection("c", spaces.Quarantine)).points); n != 3 {
		t.Fatalf("dry run left %d points, want 3", n)
	}
}

func TestPurgeQuarantineNamedDeletesOnlyThere(t *testing.T) {
	svc, f := quarantineFixture(t)
	res, err := svc.PurgeQuarantine(context.Background(), []string{"file:///q1", "file:///q1"}, false, true)
	if err != nil || res != (PurgeResult{Sources: 1, Chunks: 2, Purged: true}) {
		t.Fatalf("purge = %+v %v", res, err)
	}
	if n := len(f.collection(SpaceCollection("c", spaces.Quarantine)).points); n != 1 {
		t.Fatalf("quarantine holds %d points, want 1", n)
	}
	if n := len(f.collection(SpaceCollection("c", "work")).points); n != 1 {
		t.Fatalf("work lost points: %d", n)
	}
}

func TestPurgeQuarantineAllEmptiesIt(t *testing.T) {
	svc, f := quarantineFixture(t)
	res, err := svc.PurgeQuarantine(context.Background(), nil, true, true)
	if err != nil || res != (PurgeResult{Sources: 2, Chunks: 3, Purged: true}) {
		t.Fatalf("purge = %+v %v", res, err)
	}
	if n := len(f.collection(SpaceCollection("c", spaces.Quarantine)).points); n != 0 {
		t.Fatalf("quarantine holds %d points", n)
	}
	if n := len(f.collection(SpaceCollection("c", "work")).points); n != 1 {
		t.Fatalf("work lost points: %d", n)
	}
}

func TestPurgeQuarantineRefusesSourcesElsewhere(t *testing.T) {
	svc, f := quarantineFixture(t)
	// A source held by another space is refused exactly like one that
	// exists nowhere, and nothing is deleted, not even the named q1.
	for _, uri := range []string{"file:///w", "file:///nowhere"} {
		_, err := svc.PurgeQuarantine(context.Background(), []string{"file:///q1", uri}, false, true)
		if !errors.Is(err, ErrNotQuarantined) {
			t.Fatalf("%s: err = %v", uri, err)
		}
	}
	if n := len(f.collection(SpaceCollection("c", spaces.Quarantine)).points); n != 3 {
		t.Fatalf("quarantine holds %d points, want 3", n)
	}
	if n := len(f.collection(SpaceCollection("c", "work")).points); n != 1 {
		t.Fatalf("work lost points: %d", n)
	}
}

func TestPurgeQuarantineNeedsNamesOrAll(t *testing.T) {
	svc, _ := quarantineFixture(t)
	for _, tc := range []struct {
		uris []string
		all  bool
	}{{nil, false}, {[]string{}, false}, {[]string{"file:///q1"}, true}} {
		if _, err := svc.PurgeQuarantine(context.Background(), tc.uris, tc.all, true); err == nil {
			t.Fatalf("uris=%v all=%v: no error", tc.uris, tc.all)
		}
	}
}

func TestPurgeQuarantineReachesTheShadow(t *testing.T) {
	svc, f := quarantineFixture(t)
	f.seed(SpaceCollection("spare", spaces.Quarantine),
		map[string]any{"source_uri": "file:///q1", "source_type": "file", "chunk_index": 0})
	svc.setShadow(svc.active().WithCollection("spare"))
	defer svc.setShadow(nil)
	if _, err := svc.PurgeQuarantine(context.Background(), []string{"file:///q1"}, false, true); err != nil {
		t.Fatal(err)
	}
	if n := len(f.collection(SpaceCollection("spare", spaces.Quarantine)).points); n != 0 {
		t.Fatalf("shadow holds %d points", n)
	}
}

func TestPurgeQuarantineReportsDeleteFailure(t *testing.T) {
	svc, f := quarantineFixture(t)
	f.failDeletes[SpaceCollection("c", spaces.Quarantine)] = 1
	res, err := svc.PurgeQuarantine(context.Background(), nil, true, true)
	if err == nil || res.Purged {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

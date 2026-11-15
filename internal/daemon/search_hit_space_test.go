package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSearchHitSpaceIsReadable (SP-083, INV-01): every hit a non-operator
// search returns names its stored space, and that space is always one the
// caller reads, whichever read shape it asks.
func TestSearchHitSpaceIsReadable(t *testing.T) {
	e := newReadEnv(t)
	callers := map[string]context.Context{
		"a session in acme":      inSpace("acme"),
		"a session in globex":    inSpace("globex"),
		"a session in personal":  inSpace("personal"),
		"a task in acme":         as(resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-1", Space: "acme"}}),
		"an unidentified caller": as(resolvedCaller{}),
	}
	for name, ctx := range callers {
		readable := e.s.readScope(ctx, orchdv1.Orchd_Search_FullMethodName).spaces
		if readable == nil {
			t.Fatalf("%s: the read is not filtered", name)
		}
		seen := 0
		for _, sp := range append(slices.Clone(readSpaces), "quarantine", "legacy") {
			for _, req := range []*orchdv1.SearchRequest{
				{Query: memoText(sp, 3), TopK: 100},
				{Query: memoText(sp, 3), TopK: 100, Mode: "dense"},
				{Query: memoText(sp, 3), TopK: 100, Ranking: "plain", PerSource: -1},
			} {
				rep, err := e.s.Search(ctx, req)
				if err != nil {
					t.Fatalf("%s: Search: %v", name, err)
				}
				for _, h := range rep.GetHits() {
					seen++
					if !slices.Contains(readable, h.GetSpace()) {
						t.Errorf("%s (reads %v): hit %s names space %q", name, readable, h.GetSourceUri(), h.GetSpace())
					}
				}
			}
		}
		if seen == 0 {
			t.Errorf("%s: no hits, so nothing was checked", name)
		}
	}
	// The operator reads every space, and each hit names its own.
	rep, err := e.s.Search(appOperatorCtx, &orchdv1.SearchRequest{Query: memoText("globex", 3), TopK: 5, Ranking: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if hits := rep.GetHits(); len(hits) == 0 || hits[0].GetSpace() != "globex" || hits[0].GetSourceUri() != memoURI("globex", 3) {
		t.Errorf("the operator's top hit = %v, want globex's memo named globex", hits)
	}
}

// TestDirectMessageHitsNameTenantAndSpace (SP-083): the operator's DM search
// returns each chat hit with the tenant and space stored with it, not the
// tenant's placement now; anything that is not chat has no tenant.
func TestDirectMessageHitsNameTenantAndSpace(t *testing.T) {
	e := newReadEnv(t)
	seed := func(collection, id, space, uri, system, tenant, kind, text string) {
		e.q.seed(collection, id, map[string]any{
			"text": text, "source_uri": uri, "source_type": feeds.SourceType, "chunk_index": 0.0, "space": space,
			"writer": rag.WriterChat, "chat_kind": kind, "labels_v": e.labels,
			"origin": map[string]any{"system": system, "tenant": tenant, "container_kind": kind},
		})
	}
	// Reason: the policy binds no chat tenant, so a placement computed now
	// would quarantine both; only the stored payload names acme and globex.
	seed("c__acme", "dm-acme", "acme", acmeDMURI, "slack", "TACME", rag.ChatDM, acmeDMText)
	seed("c__globex", "dm-globex", "globex", globexDMURI, "teams", "globex-tenant", rag.ChatGroupDM, globexDMText)
	rep, err := e.s.SearchDirectMessages(appOperatorCtx, &orchdv1.SearchDirectMessagesRequest{
		Search: &orchdv1.SearchRequest{Query: "merger codename bluefin", TopK: 100}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{acmeDMURI: {"slack:TACME", "acme"}, globexDMURI: {"teams:globex-tenant", "globex"}}
	found := 0
	for _, h := range rep.GetHits() {
		w, ok := want[h.GetSourceUri()]
		switch {
		case ok:
			found++
			if h.GetTenant() != w[0] || h.GetSpace() != w[1] {
				t.Errorf("%s: tenant %q space %q, want %q %q", h.GetSourceUri(), h.GetTenant(), h.GetSpace(), w[0], w[1])
			}
		case h.GetTenant() != "":
			t.Errorf("%s is not chat but names tenant %q", h.GetSourceUri(), h.GetTenant())
		}
	}
	if found != len(want) {
		t.Errorf("the DM search found %d of %d DMs", found, len(want))
	}
}

// TestSearchHitSpaceEmptyWithoutPolicy (SP-083): with no policy applied
// (and so with spaces off, whose engine reports none) a hit names no space,
// though its chunk is stamped shared, while a chat hit still names its
// tenant.
func TestSearchHitSpaceEmptyWithoutPolicy(t *testing.T) {
	q, url := newSearchQdrant(t)
	s := spacesServer(t)
	s.rag = newSearchRAG(url)
	q.seed("c", "dm", map[string]any{
		"text": acmeDMText, "source_uri": acmeDMURI, "source_type": feeds.SourceType, "chunk_index": 0.0, "space": "shared",
		"writer": rag.WriterChat, "chat_kind": rag.ChatDM, "labels_v": 0.0,
		"origin": map[string]any{"system": "slack", "tenant": "TACME"},
	})
	rep, err := s.SearchDirectMessages(operatorCtx, &orchdv1.SearchDirectMessagesRequest{
		Search: &orchdv1.SearchRequest{Query: acmeDMText, TopK: 10}})
	if err != nil {
		t.Fatal(err)
	}
	hits := rep.GetHits()
	if len(hits) != 1 || hits[0].GetSpace() != "" || hits[0].GetTenant() != "slack:TACME" {
		t.Fatalf("hits with no policy = %v, want one DM with no space and tenant slack:TACME", hits)
	}
}

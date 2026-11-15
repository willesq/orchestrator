package teams

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/rag"
)

const (
	tenant   = "0a1b2c3d-0000-4000-8000-00000000c0de"
	dmChat   = "19:u-alex_u-sam@unq.gbl.spaces"
	group    = "19:group-launch@thread.v2"
	meeting  = "19:meeting_standup@thread.v2"
	general  = "19:general@thread.tacv2"
	incident = "19:incidents@thread.tacv2"
	root1    = "1731672000000"
	root2    = "1731675600000"
)

func uri(container, thread string) string {
	return "teams://" + tenant + "/" + container + "/" + thread
}

// store is a chat store keyed by URI, as rag keeps one source per URI: a
// write replaces what the URI held.
type store struct {
	docs   map[string]string
	metas  map[string]rag.Meta
	writes []string
}

func newStore() *store { return &store{docs: map[string]string{}, metas: map[string]rag.Meta{}} }

func (s *store) IngestText(_ context.Context, text string, m rag.Meta) (int, error) {
	s.docs[m.SourceURI] = text
	s.metas[m.SourceURI] = m
	s.writes = append(s.writes, m.SourceURI)
	return 1, nil
}

func (s *store) take() []string {
	w := s.writes
	s.writes = nil
	slices.Sort(w)
	return w
}

// allowed is the allowlist the operator gives: the tenant's DMs, the
// meeting chat and the General channel; not the Incidents channel.
func allowed() feeds.Allowlist {
	return feeds.Allowlist{
		DirectMessages: []string{"teams:" + tenant},
		Containers:     map[string][]string{"teams:" + strings.ToUpper(tenant): {meeting, general}},
	}
}

type env struct {
	g     *fakeGraph
	conn  *Connector
	store *store
	path  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "teams-state.json")
	st, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	g := newFakeGraph(t)
	return &env{g: g, conn: &Connector{Graph: g, Tenant: tenant, Allow: allowed(), State: st}, store: newStore(), path: path}
}

// sync runs feeds.Sync as orchd would, with the state reopened from its
// file each run, as a new connector process would see it.
func (e *env) sync(t *testing.T) feeds.Report {
	t.Helper()
	st, err := OpenState(e.path)
	if err != nil {
		t.Fatal(err)
	}
	e.conn.State = st
	e.g.reset()
	rep, err := feeds.Sync(context.Background(), e.conn, e.conn.Allow, e.store)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return rep
}

var day1 = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

// TestFirstSync is SP-062.1.a1 (first sync): every allowed conversation is
// stored once, as a record of facts under its URI, rendered from Graph's
// HTML; the Incidents channel, not on the allowlist, is never read.
func TestFirstSync(t *testing.T) {
	e := newEnv(t)
	rep := e.sync(t)
	want := []string{uri(general, root1), uri(general, root2), uri(dmChat, "2026-11-14"), uri(group, "2026-11-14"), uri(meeting, "2026-11-14")}
	slices.Sort(want)
	if got := e.store.take(); !slices.Equal(got, want) {
		t.Fatalf("stored %v, want %v", got, want)
	}
	if rep != (feeds.Report{Stored: 5}) {
		t.Errorf("report = %+v", rep)
	}
	if n := e.g.asked(incident); n != 0 {
		t.Errorf("the Incidents channel, not allowed, was read %d times", n)
	}
	for u, w := range map[string]struct {
		kind, parts string
		text        []string
	}{
		uri(dmChat, "2026-11-14"):  {rag.ChatDM, "u-alex,u-sam", []string{"[2026-11-14 09:00 UTC] Alex: can you look at my patch?\n", "Sam: after lunch"}},
		uri(group, "2026-11-14"):   {rag.ChatGroupDM, "u-kim", []string{"Kim: launch is thursday & not friday"}},
		uri(meeting, "2026-11-14"): {rag.ChatMeeting, "u-alex", []string{"standup notes"}},
		uri(general, root1):        {rag.ChatPublicChannel, "u-kim,u-sam", []string{"Kim: release train moves to thursday\n", "Sam: noted"}},
		uri(general, root2):        {rag.ChatPublicChannel, "u-alex", []string{"lunch order is in"}},
	} {
		m := e.store.metas[u]
		if m.Origin.ContainerKind != w.kind || m.Origin.Participants != w.parts || m.Origin.Tenant != tenant || m.Origin.System != "teams" {
			t.Errorf("%s: origin %+v", u, m.Origin)
		}
		for _, s := range w.text {
			if !strings.Contains(e.store.docs[u], s) {
				t.Errorf("%s: text %q lacks %q", u, e.store.docs[u], s)
			}
		}
		if strings.Contains(e.store.docs[u], "<") || strings.Contains(e.store.docs[u], "systemEvent") {
			t.Errorf("%s: markup or a system event stored: %q", u, e.store.docs[u])
		}
	}
	if m := e.store.metas[uri(dmChat, "2026-11-14")]; !m.StartedAt.Equal(day1.Add(9*time.Hour)) || !m.EndedAt.Equal(day1.Add(9*time.Hour+time.Minute)) {
		t.Errorf("DM dates %v..%v", m.StartedAt, m.EndedAt)
	}
}

// TestIncrementalSyncFromWatermark is SP-062.1.a1 (incremental sync): a
// second run asks Graph only for chat messages changed since each chat's
// watermark, stores only the new conversations, and a run with nothing new
// stores nothing.
func TestIncrementalSyncFromWatermark(t *testing.T) {
	e := newEnv(t)
	e.sync(t)
	e.store.take()
	day2 := day1.AddDate(0, 0, 1)
	e.g.post(meeting, "1731747600000", "", "u-sam", "retro moved to friday", day2.Add(9*time.Hour))
	e.g.post(general, "1731751200000", "", "u-kim", "new thread: on-call rota", day2.Add(10*time.Hour))
	rep := e.sync(t)
	want := []string{uri(general, "1731751200000"), uri(meeting, "2026-11-15")}
	if got := e.store.take(); !slices.Equal(got, want) || rep.Stored != 2 {
		t.Fatalf("stored %v (%+v), want %v", got, rep, want)
	}
	if !strings.Contains(e.store.docs[uri(meeting, "2026-11-15")], "retro moved to friday") ||
		strings.Contains(e.store.docs[uri(meeting, "2026-11-15")], "standup notes") {
		t.Errorf("the new day holds %q", e.store.docs[uri(meeting, "2026-11-15")])
	}
	st, err := OpenState(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Watermarks["teams://"+tenant+"/"+meeting]; !got.Equal(day2.Add(9 * time.Hour)) {
		t.Errorf("meeting watermark = %v", got)
	}
	if got := st.Watermarks["teams://"+tenant+"/"+dmChat]; !got.Equal(day1.Add(9*time.Hour + time.Minute)) {
		t.Errorf("DM watermark = %v", got)
	}
	// A message changed in the very millisecond of the watermark after the
	// last run is still read; the ones the last run had are not again.
	e.g.post(meeting, "1731747600001", "", "u-kim", "same millisecond", day2.Add(9*time.Hour))
	if rep := e.sync(t); rep.Stored != 1 || !strings.Contains(e.store.docs[uri(meeting, "2026-11-15")], "same millisecond") {
		t.Errorf("a message at the watermark was missed: %+v", rep)
	}
	e.store.take()
	if st, _ := OpenState(e.path); len(st.AtMark["teams://"+tenant+"/"+meeting]) != 2 {
		t.Errorf("at-mark IDs = %v", st.AtMark)
	}
	// Nothing changed: nothing is stored.
	if rep := e.sync(t); rep.Stored != 0 || len(e.store.take()) != 0 {
		t.Errorf("an unchanged run stored %+v", rep)
	}
}

// TestIncrementalChatAsksSinceWatermark: the chat request of a later run
// carries the watermark as Graph's lastModifiedDateTime filter.
func TestIncrementalChatAsksSinceWatermark(t *testing.T) {
	e := newEnv(t)
	e.sync(t)
	var got []string
	inner := e.g
	e.conn.Graph = GraphFunc(func(ctx context.Context, raw string) ([]byte, error) {
		if strings.Contains(raw, "chats/"+dmChat) {
			got = append(got, raw)
		}
		return inner.Get(ctx, raw)
	})
	e.sync(t)
	if len(got) != 1 || !strings.Contains(got[0], "lastModifiedDateTime+gt+2026-11-14T09%3A00%3A59.999Z") {
		t.Errorf("DM requests = %v", got)
	}
}

// TestThreadGrowthReplacesURI is SP-062.1.a1 (thread growth): a reply to a
// channel thread, a new message on a chat's day and an edit are each stored
// again whole under the conversation's URI, replacing it.
func TestThreadGrowthReplacesURI(t *testing.T) {
	e := newEnv(t)
	e.sync(t)
	e.store.take()
	later := day1.Add(20 * time.Hour)
	e.g.post(general, "1731744000000", root1, "u-alex", "thursday works", later)
	e.g.post(dmChat, "1731744060000", "", "u-alex", "thanks!", later.Add(time.Minute))
	e.g.edit(group, "1731664860000", "launch is friday after all", later.Add(2*time.Minute))
	rep := e.sync(t)
	want := []string{uri(general, root1), uri(dmChat, "2026-11-14"), uri(group, "2026-11-14")}
	slices.Sort(want)
	if got := e.store.take(); !slices.Equal(got, want) || rep.Stored != 3 {
		t.Fatalf("stored %v (%+v), want %v", got, rep, want)
	}
	if len(e.store.docs) != 5 {
		t.Errorf("growth added URIs: %d stored", len(e.store.docs))
	}
	thread := e.store.docs[uri(general, root1)]
	for _, s := range []string{"release train moves to thursday", "Sam: noted", "u-alex: thursday works"} {
		if !strings.Contains(thread, s) {
			t.Errorf("the grown thread lacks %q: %q", s, thread)
		}
	}
	if p := e.store.metas[uri(general, root1)].Origin.Participants; p != "u-alex,u-kim,u-sam" {
		t.Errorf("participants = %s", p)
	}
	dm := e.store.docs[uri(dmChat, "2026-11-14")]
	if !strings.Contains(dm, "can you look at my patch?") || !strings.Contains(dm, "thanks!") {
		t.Errorf("the grown DM day = %q", dm)
	}
	if g := e.store.docs[uri(group, "2026-11-14")]; !strings.Contains(g, "friday after all") || strings.Contains(g, "thursday") {
		t.Errorf("the edited group day = %q", g)
	}
	if !e.store.metas[uri(general, root1)].EndedAt.Equal(later) {
		t.Errorf("thread ends %v", e.store.metas[uri(general, root1)].EndedAt)
	}
}

// TestRemovedFromAllowlistStopsSyncing is SP-062.1.a1 (allowlist removal):
// once the operator takes the General channel off the allowlist its
// messages are no longer read or stored, and its watermark is forgotten, so
// allowing it again reads it from the start.
func TestRemovedFromAllowlistStopsSyncing(t *testing.T) {
	e := newEnv(t)
	e.sync(t)
	e.store.take()
	e.conn.Allow.Containers = map[string][]string{"teams:" + tenant: {meeting}}
	e.g.post(general, "1731751200000", "", "u-kim", "posted while not allowed", day1.Add(30*time.Hour))
	rep := e.sync(t)
	if got := e.store.take(); len(got) != 0 || rep.Stored != 0 {
		t.Fatalf("stored %v after removal", got)
	}
	if n := e.g.asked(general + "/messages"); n != 0 {
		t.Errorf("the removed channel's messages were read %d times", n)
	}
	st, _ := OpenState(e.path)
	for k := range st.Watermarks {
		if strings.Contains(k, general) {
			t.Errorf("watermark kept for %s", k)
		}
	}
	for k := range st.Hashes {
		if strings.Contains(k, general) {
			t.Errorf("hash kept for %s", k)
		}
	}
	// Allowed again: read from the start, the message posted meanwhile included.
	e.conn.Allow = allowed()
	e.sync(t)
	got := e.store.take()
	if !slices.Contains(got, uri(general, "1731751200000")) || !slices.Contains(got, uri(general, root1)) {
		t.Errorf("re-allowed channel stored %v", got)
	}
	// Removing every Teams entry reads nothing and forgets everything.
	e.conn.Allow = feeds.Allowlist{Containers: map[string][]string{"slack:T1": {"C1"}}}
	if rep := e.sync(t); rep != (feeds.Report{}) || len(e.g.requests) != 0 {
		t.Errorf("an allowlist without Teams read %v (%+v)", e.g.requests, rep)
	}
	if st, _ := OpenState(e.path); len(st.Watermarks) != 0 || len(st.Hashes) != 0 {
		t.Errorf("state kept %+v", st)
	}
}

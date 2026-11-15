package slack

import (
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/rag"
)

// TestSlackFirstSync is SP-061.1.a1 (first sync): against the fake Slack
// API, a first sync stores the DM and group DM first, then the allowlisted
// private channel (a day's messages outside threads, and the thread under
// its ts), then the public channel; nothing off the allowlist is read, and
// each conversation's watermark is saved.
func TestSlackFirstSync(t *testing.T) {
	e := newSyncEnv(t)
	rep := e.sync(t)
	want := []string{
		uri("D1", "2026-11-15"),
		uri("GMPIM1", "2026-11-15"),
		uri("CPRIV", "2026-11-15"),
		uri("CPRIV", e.thread),
		uri("CPUB", "2026-11-15"),
	}
	if got := e.ing.uris(); !slices.Equal(got, want) || rep.Stored != 5 {
		t.Fatalf("stored %v (%+v), want %v", got, rep, want)
	}
	kinds := map[string]string{}
	for _, m := range e.ing.writes {
		kinds[m.Origin.Container] = m.Origin.ContainerKind
		if m.Space != "" || m.Writer != rag.WriterChat || m.Origin.Tenant != "T1" || m.Origin.System != "slack" {
			t.Errorf("%s: meta %+v", m.SourceURI, m)
		}
	}
	if kinds["D1"] != rag.ChatDM || kinds["GMPIM1"] != rag.ChatGroupDM || kinds["CPRIV"] != rag.ChatPrivateChannel || kinds["CPUB"] != rag.ChatPublicChannel {
		t.Errorf("kinds = %v", kinds)
	}
	if dm := e.ing.writes[0]; dm.Origin.Participants != "UBOB,UME" {
		t.Errorf("DM participants = %q", dm.Origin.Participants)
	}
	day := e.ing.byURI[uri("CPRIV", "2026-11-15")]
	thread := e.ing.byURI[uri("CPRIV", e.thread)]
	for _, s := range []string{"release train moves to thursday (thread: 2 replies)", "who has the changelog?", "#release"} {
		if !strings.Contains(day, s) {
			t.Errorf("day record lacks %q:\n%s", s, day)
		}
	}
	if strings.Contains(day, "joined") || strings.Contains(day, "ok for web") {
		t.Errorf("day record holds a join or a reply:\n%s", day)
	}
	for _, s := range []string{"release train moves to thursday", "ok for mobile", "ok for web"} {
		if !strings.Contains(thread, s) {
			t.Errorf("thread record lacks %q:\n%s", s, thread)
		}
	}
	if len(e.api.called("", "COTHER")) != 0 {
		t.Error("a channel off the allowlist was read")
	}
	for _, c := range e.api.called("conversations.history", "") {
		if want := formatTS(testNow.Add(-DefaultHistory)); c.params["oldest"] != want {
			t.Errorf("first sync of %s read from %q, want the default history %q", c.params["channel"], c.params["oldest"], want)
		}
	}
	st, err := e.state.Load()
	if err != nil {
		t.Fatal(err)
	}
	priv := st.Channels["CPRIV"]
	if priv.Latest == "" || priv.Threads[e.thread] == "" || len(priv.Hashes) != 2 || priv.Kind != feeds.PrivateChannel || len(st.Channels) != 4 {
		t.Errorf("state = %+v", st)
	}
}

// TestSlackIncrementalSync is SP-061.1.a1 (incremental sync): a second run
// reads each conversation from its watermark (the watermark's day, not the
// whole history), sends again only the day that gained a message, and a run
// with nothing new sends nothing.
func TestSlackIncrementalSync(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t)
	st, _ := e.state.Load()
	e.api.post("CPRIV", 50, "UBOB", "changelog is in the wiki", "")
	n := len(e.ing.writes)
	e.sync(t)
	if got := e.since(n); !slices.Equal(got, []string{uri("CPRIV", "2026-11-15")}) {
		t.Fatalf("second run stored %v, want only the private channel's day", got)
	}
	if text := e.ing.byURI[uri("CPRIV", "2026-11-15")]; !strings.Contains(text, "who has the changelog?") || !strings.Contains(text, "changelog is in the wiki") {
		t.Errorf("the day record is not whole:\n%s", text)
	}
	// The lookback reaches back further than the watermark's day here, so
	// each conversation is read from the lookback, not its whole history.
	for _, c := range e.api.called("conversations.history", "") {
		if want := formatTS(testNow.Add(-DefaultLookback)); c.params["oldest"] != want {
			t.Errorf("history of %s read from %q on an incremental run, want %q", c.params["channel"], c.params["oldest"], want)
		}
	}
	if len(e.api.called("conversations.replies", "")) != 0 {
		t.Error("an unchanged thread was read again")
	}
	st2, _ := e.state.Load()
	if !after(st2.Channels["CPRIV"].Latest, st.Channels["CPRIV"].Latest) || st2.Channels["CPUB"].Latest != st.Channels["CPUB"].Latest {
		t.Errorf("watermarks %+v then %+v", st.Channels, st2.Channels)
	}
	n = len(e.ing.writes)
	e.sync(t)
	if got := e.since(n); len(got) != 0 {
		t.Errorf("a run with nothing new stored %v", got)
	}
	// Dedupe by content hash: with the watermark set back, the days and
	// thread read again are unchanged and not sent.
	st3, _ := e.state.Load()
	cs := st3.Channels["CPRIV"]
	cs.Latest, cs.Threads = "", nil
	st3.Channels["CPRIV"] = cs
	if err := e.state.Save(st3); err != nil {
		t.Fatal(err)
	}
	n = len(e.ing.writes)
	e.sync(t)
	if got := e.since(n); len(got) != 0 {
		t.Errorf("unchanged records were sent again: %v", got)
	}
}

// TestSlackThreadGrowth is SP-061.1.a1 (thread growth): a reply to a
// thread already stored sends the whole thread again under the same URI, so
// the stored copy is replaced, not duplicated; the day record is untouched.
func TestSlackThreadGrowth(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t)
	uris := len(e.ing.byURI)
	e.api.post("CPRIV", 55, "UDAN", "ok for desktop too", e.thread)
	n := len(e.ing.writes)
	e.sync(t)
	if got := e.since(n); !slices.Equal(got, []string{uri("CPRIV", e.thread)}) {
		t.Fatalf("after a reply stored %v, want the thread's URI only", got)
	}
	text := e.ing.byURI[uri("CPRIV", e.thread)]
	for _, s := range []string{"release train moves to thursday", "ok for mobile", "ok for web", "ok for desktop too"} {
		if !strings.Contains(text, s) {
			t.Errorf("grown thread lacks %q:\n%s", s, text)
		}
	}
	if len(e.ing.byURI) != uris {
		t.Errorf("thread growth added a URI: %d, was %d", len(e.ing.byURI), uris)
	}
	st, _ := e.state.Load()
	if got := st.Channels["CPRIV"].Threads[e.thread]; got != e.api.convs["CPRIV"].msgs[len(e.api.convs["CPRIV"].msgs)-1].TS {
		t.Errorf("thread watermark = %q", got)
	}
}

// TestSlackAllowlistRemoval is SP-061.1.a1 (allowlist removal): a channel
// taken off the allowlist is no longer read at all, its new messages are
// not stored, and its watermark is dropped; turning DMs off stops reading
// DMs the same way.
func TestSlackAllowlistRemoval(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t)
	e.conn.Allow = feeds.Allowlist{Containers: map[string][]string{"slack:T1": {"CPUB"}}}
	e.api.post("CPRIV", 60, "UBOB", "secret plans", "")
	e.api.post("D1", 61, "UBOB", "another DM", "")
	e.api.post("CPUB", 62, "UDAN", "coffee?", "")
	n := len(e.ing.writes)
	e.sync(t)
	if got := e.since(n); !slices.Equal(got, []string{uri("CPUB", "2026-11-15")}) {
		t.Fatalf("stored %v, want the public channel only", got)
	}
	if calls := e.api.called("", "CPRIV"); len(calls) != 0 {
		t.Errorf("the removed channel was still read: %v", calls)
	}
	if calls := e.api.called("conversations.list", ""); len(calls) != 0 {
		t.Errorf("DMs were listed with DMs off: %v", calls)
	}
	for _, text := range e.ing.texts[n:] {
		if strings.Contains(text, "secret plans") || strings.Contains(text, "another DM") {
			t.Errorf("stored a removed conversation's message: %s", text)
		}
	}
	st, _ := e.state.Load()
	if _, ok := st.Channels["CPRIV"]; ok || len(st.Channels) != 1 {
		t.Errorf("state kept removed conversations: %+v", st.Channels)
	}
	// Nothing allowlisted for the workspace: the connector process is not
	// even started.
	e.conn.Allow = feeds.Allowlist{}
	e.sync(t)
	if calls := e.api.called("", ""); len(calls) != 0 {
		t.Errorf("an empty allowlist still called Slack: %v", calls)
	}
}

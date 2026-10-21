package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/session"
)

// storeRecorder records what was stored, failing when err is set.
type storeRecorder struct {
	mu    sync.Mutex
	err   error
	texts map[string]string // source URI -> text
}

func (s *storeRecorder) IngestText(_ context.Context, text string, m rag.Meta) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	if s.texts == nil {
		s.texts = map[string]string{}
	}
	s.texts[m.SourceURI] = text
	return 1, nil
}

func (s *storeRecorder) stored(uri string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.texts[uri]
	return t, ok
}

var afterAll = time.Now().Add(time.Hour)

func TestStoreSessions(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := handoff.Dir()
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type":"user","message":{"role":"user","content":"token `+secret+` add retries"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s1", Summary: "the excerpt"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []handoff.Session{
		{ID: "s1", TranscriptPath: tr, Record: true, Summarize: true},
		{ID: "s2", TranscriptPath: tr},
		{ID: "gone", TranscriptPath: "/nonexistent/x.jsonl"},
		{ID: "notjsonl", TranscriptPath: "/etc/hosts"},
	} {
		if err := handoff.PutSession(dir, s); err != nil {
			t.Fatal(err)
		}
	}
	st := &storeRecorder{}
	q := newSummaryQueue()
	storeSessions(context.Background(), st, q, dir, afterAll)

	text, ok := st.stored("session://s1/transcript")
	if !ok || !strings.Contains(text, "add retries") || strings.Contains(text, secret) {
		t.Fatalf("transcript = %q, %v", text, ok)
	}
	if rec, _ := st.stored("session://s1/record"); rec != "the excerpt" {
		t.Fatalf("record = %q", rec)
	}
	if _, ok := st.stored("session://s2/record"); ok {
		t.Fatal("stored a record the hook already stored")
	}
	if _, ok := st.stored("session://gone/transcript"); ok {
		t.Fatal("stored a missing transcript")
	}
	if left, _ := handoff.Sessions(dir, afterAll); len(left) != 0 {
		t.Fatalf("entries left: %+v", left)
	}
	// Only s1 asked for its summary to be queued.
	select {
	case j := <-q.jobs:
		if j.SessionID != "s1" {
			t.Fatalf("queued %+v", j)
		}
	default:
		t.Fatal("s1's summary was not queued")
	}
	if len(q.jobs) != 0 {
		t.Fatalf("%d extra summaries queued", len(q.jobs))
	}
}

func TestStoreSessionsRetriesThenGivesUp(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := handoff.Dir()
	if err := handoff.PutSession(dir, handoff.Session{ID: "s1", TranscriptPath: transcript(t)}); err != nil {
		t.Fatal(err)
	}
	st := &storeRecorder{err: errors.New("qdrant down")}
	backdate := func() { // as if the backoff had passed
		p := filepath.Join(dir, "sessions", "s1.json")
		old := time.Now().Add(-24 * time.Hour)
		_ = os.Chtimes(p, old, old)
	}
	for i := 1; i < maxHandoffAttempts; i++ {
		storeSessions(context.Background(), st, nil, dir, afterAll)
		left, _ := handoff.Sessions(dir, afterAll)
		if len(left) != 1 || left[0].Attempts != i {
			t.Fatalf("after %d tries: %+v", i, left)
		}
		// Not tried again until its backoff has passed.
		storeSessions(context.Background(), st, nil, dir, afterAll)
		if again, _ := handoff.Sessions(dir, afterAll); again[0].Attempts != i {
			t.Fatalf("retried within the backoff: %+v", again)
		}
		backdate()
	}
	storeSessions(context.Background(), st, nil, dir, afterAll)
	if left, _ := handoff.Sessions(dir, afterAll); len(left) != 0 {
		t.Fatalf("not given up: %+v", left)
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "sessions", "s1.json")); err != nil {
		t.Fatalf("given-up entry not kept: %v", err)
	}
}

func TestDue(t *testing.T) {
	now := time.Now()
	if !due(0, now) || due(1, now) || !due(1, now.Add(-handoffBackoff)) ||
		due(3, now.Add(-3*handoffBackoff)) || !due(3, now.Add(-4*handoffBackoff)) || !due(50, now.Add(-1000*time.Hour)) {
		t.Fatal("backoff schedule")
	}
}

func TestSummaryAndRecordDoNotRace(t *testing.T) {
	// The summarizer writes its summary to disk before storing it; a hand-off
	// storing the record afterwards stores the summary, not the excerpt.
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s1", Summary: "the excerpt"}); err != nil {
		t.Fatal(err)
	}
	st := &storeRecorder{}
	summarizeOne(context.Background(), fakeSummarizer{summary: "the summary"}, st,
		summaryJob{SessionID: "s1", TranscriptPath: transcript(t)}, time.Second)
	if err := storeRecord(context.Background(), st, handoff.Session{ID: "s1"}, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st.stored("session://s1/record"); rec != "the summary" {
		t.Fatalf("record = %q", rec)
	}
}

func TestStoreFiles(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := handoff.Dir()
	docs := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(docs, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	doc := write("guide.md", "# Guide\n\nuse "+secret+" here")
	keystore := write("keys.md", strings.Repeat("ghp_"+strings.Repeat("z9Y8", 9)+"\n", 30))
	code := write("main.go", "package main")
	big := write("big.md", strings.Repeat("x", maxHandoffDoc+1))
	outside := write("secret.txt", "not documentation")
	link := filepath.Join(docs, "link.md") // a .md name pointing at a non-doc file
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{doc, keystore, code, big, link, filepath.Join(docs, "deleted.md")} {
		if err := handoff.PutFile(dir, handoff.File{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	st := &storeRecorder{}
	storeFiles(context.Background(), st, dir, afterAll)
	text, ok := st.stored("file://" + doc)
	if !ok || !strings.Contains(text, "# Guide") || strings.Contains(text, secret) {
		t.Fatalf("doc = %q, %v", text, ok)
	}
	for _, p := range []string{keystore, code, big, link, filepath.Join(docs, "deleted.md")} {
		if _, ok := st.stored("file://" + p); ok {
			t.Errorf("stored %s", p)
		}
	}
	if left, _ := handoff.Files(dir, afterAll); len(left) != 0 {
		t.Fatalf("entries left: %+v", left)
	}

	// A failing store keeps the entry for another try.
	if err := handoff.PutFile(dir, handoff.File{Path: doc}); err != nil {
		t.Fatal(err)
	}
	storeFiles(context.Background(), &storeRecorder{err: errors.New("down")}, dir, afterAll)
	if left, _ := handoff.Files(dir, afterAll); len(left) != 1 || left[0].Attempts != 1 {
		t.Fatalf("after a failure: %+v", left)
	}
	// Within the backoff it waits.
	storeFiles(context.Background(), &storeRecorder{}, dir, afterAll)
	if left, _ := handoff.Files(dir, afterAll); len(left) != 1 || left[0].Attempts != 1 {
		t.Fatalf("after a failure: %+v", left)
	}
}

func TestHandoffUnitStoresOnStart(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := handoff.Dir()
	savedSettle := handoffSettle
	handoffSettle = -time.Hour // take entries at once
	t.Cleanup(func() { handoffSettle = savedSettle })
	if err := handoff.PutSession(dir, handoff.Session{ID: "s1", TranscriptPath: transcript(t)}); err != nil {
		t.Fatal(err)
	}
	st := &storeRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := handoffUnit(st, nil, dir)
	if u.Name != UnitHandoff {
		t.Fatalf("name = %s", u.Name)
	}
	if err := u.Run(ctx, func() {}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := st.stored("session://s1/transcript"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the unit did not store the handed-off session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPRSweepDue(t *testing.T) {
	dir := t.TempDir()
	poll := time.Minute
	if !prSweepDue(dir, time.Time{}, poll) {
		t.Fatal("first tick must sweep")
	}
	if prSweepDue(dir, time.Now(), poll) {
		t.Fatal("swept again before poll passed")
	}
	if !prSweepDue(dir, time.Now().Add(-2*poll), poll) {
		t.Fatal("did not sweep after poll")
	}
	if err := handoff.RequestPRSweep(dir); err != nil {
		t.Fatal(err)
	}
	if !prSweepDue(dir, time.Now(), poll) || prSweepDue(dir, time.Now(), poll) {
		t.Fatal("a request sweeps once")
	}
}

// metaRecorder keeps the metadata each store was given, by source URI.
type metaRecorder struct {
	mu    sync.Mutex
	metas map[string]rag.Meta
	texts map[string]string
}

func (m *metaRecorder) IngestText(_ context.Context, text string, meta rag.Meta) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.metas == nil {
		m.metas, m.texts = map[string]rag.Meta{}, map[string]string{}
	}
	m.metas[meta.SourceURI], m.texts[meta.SourceURI] = meta, text
	return 1, nil
}

func TestHandedOffSessionsAreDated(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","timestamp":"2026-10-20T09:00:00Z","message":{"role":"user","content":"start"}}` + "\n" +
		`{"type":"user","timestamp":"2026-10-21T16:45:00Z","message":{"role":"user","content":"what did we do yesterday"}}` + "\n"
	if err := os.WriteFile(tr, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A record written before sessions were dated.
	if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "old", Summary: "the excerpt", TranscriptPath: tr}); err != nil {
		t.Fatal(err)
	}
	m := &metaRecorder{}
	if err := storeSession(context.Background(), m, handoff.Session{ID: "old", TranscriptPath: tr, Record: true}); err != nil {
		t.Fatal(err)
	}
	if meta := m.metas["session://old/transcript"]; !meta.Timeline || !strings.Contains(m.texts["session://old/transcript"], "[2026-10-21T16:45Z] user") {
		t.Fatalf("transcript meta = %+v, text %q", meta, m.texts["session://old/transcript"])
	}
	rmeta := m.metas["session://old/record"]
	if rmeta.StartedAt.Format(time.RFC3339) != "2026-10-20T09:00:00Z" || rmeta.EndedAt.Format(time.RFC3339) != "2026-10-21T16:45:00Z" ||
		!strings.HasPrefix(m.texts["session://old/record"], "Session 2026-10-20 09:00 to 2026-10-21 16:45 UTC") {
		t.Fatalf("record meta = %+v, text %q", rmeta, m.texts["session://old/record"])
	}
	// The record on disk now carries its dates too.
	if rec, _ := session.ReadRecord(paths.SessionsDir(), "old"); rec.StartedAt != "2026-10-20T09:00:00Z" || rec.Summary != "the excerpt" {
		t.Fatalf("record on disk = %+v", rec)
	}
}

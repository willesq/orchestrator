package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
)

func testClient(f *fakeSlack) *client {
	return &client{base: f.url(), token: testToken, http: http.DefaultClient,
		sleep: func(context.Context, time.Duration) error { return nil }}
}

// TestClientErrors: rate limits are waited out, and every failure names the
// method and nothing a server sent but a well-formed error code.
func TestClientErrors(t *testing.T) {
	ctx := context.Background()
	f := newFakeSlack(t)
	f.add(conversation{ID: "C1"})
	c := testClient(f)

	f.limit["auth.test"] = 2
	if id, err := c.authTest(ctx); err != nil || id.TeamID != "T1" || len(f.called("auth.test", "")) != 3 {
		t.Errorf("rate-limited auth.test = %+v, %v", id, err)
	}
	f.limit["auth.test"] = maxRetries + 1
	if _, err := c.authTest(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Errorf("rate limit past the retries = %v", err)
	}
	f.limit["auth.test"] = 0

	for body, want := range map[string]string{
		`not json`:                              "not JSON",
		`{"ok":false,"error":"ratelimited"}`:    "slack auth.test: ratelimited",
		`{"ok":false,"error":"<script>bad</>"}`: "unknown_error",
		`{"ok":true,"team_id":7}`:               "unexpected response",
	} {
		f.broken["auth.test"] = body
		_, err := c.authTest(ctx)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "script") {
			t.Errorf("body %s: %v, want %q", body, err, want)
		}
	}
	delete(f.broken, "auth.test")

	f.broken["conversations.history"] = `{"ok":true,"messages":[],"response_metadata":{"next_cursor":"same"}}`
	if _, err := c.history(ctx, "C1", ""); !errors.Is(err, errCursorLoop) {
		t.Errorf("repeated cursor = %v", err)
	}
	f.broken["conversations.history"] = `{"ok":true,"messages":7}`
	if _, err := c.history(ctx, "C1", ""); err == nil {
		t.Error("a malformed page was accepted")
	}
	f.broken["conversations.list"] = `{"ok":true,"channels":"x"}`
	if _, err := c.list(ctx, "im"); err == nil {
		t.Error("a malformed list was accepted")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer down.Close()
	c.base = down.URL + "/"
	if _, err := c.authTest(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("server error = %v", err)
	}
	down.Close()
	if _, err := c.authTest(ctx); err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("unreachable server = %v", err)
	}
	c.base = "http://[::1"
	if _, err := c.authTest(ctx); err == nil {
		t.Error("a bad URL was accepted")
	}
}

func TestRetryAfterAndSleep(t *testing.T) {
	for h, want := range map[string]time.Duration{"": time.Second, "x": time.Second, "0": time.Second, "5": 5 * time.Second, "999": maxRetryWait} {
		if got := retryAfter(h); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sleep = %v", err)
	}
	f := newFakeSlack(t)
	f.limit["auth.test"] = 1
	c := testClient(f)
	c.sleep = sleepCtx
	if _, err := c.authTest(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("rate limit with a cancelled context = %v", err)
	}
}

func TestTimestamps(t *testing.T) {
	ts := formatTS(testNow.Add(123 * time.Microsecond))
	if got, ok := parseTS(ts); !ok || !got.Equal(testNow.Add(123*time.Microsecond)) {
		t.Errorf("parseTS(%s) = %v, %v", ts, got, ok)
	}
	for _, bad := range []string{"", "abc", "1.2.3", "99999999999999999.1"} {
		if _, ok := parseTS(bad); ok {
			t.Errorf("parseTS(%q) accepted", bad)
		}
	}
	if !after("2.0", "1.0") || after("1.0", "2.0") || !after("1.0", "") || after("x", "") {
		t.Error("after is wrong")
	}
	if author(message{BotID: "B1"}) != "B1" || short(strings.Repeat("a", 70)) != strings.Repeat("a", 64)+"…" {
		t.Error("author or short is wrong")
	}
}

// TestFetchEdgeCases: noise and malformed messages are dropped, an archived
// or DM container listed as a channel is not read, a bot's or an unnamed
// author is kept as such, and old threads and hashes are forgotten.
func TestFetchEdgeCases(t *testing.T) {
	e := newSyncEnv(t)
	e.api.add(conversation{ID: "CARCH", IsArchived: true})
	e.api.add(conversation{ID: "D2", IsIM: true, User: "UZED"})
	e.api.post("CPUB", 41, "", "deploy finished", "")
	e.api.convs["CPUB"].msgs[1].BotID = "B1"
	e.api.post("CPUB", 42, "bad/id", "who am i", "")
	e.api.convs["CPUB"].msgs = append(e.api.convs["CPUB"].msgs, message{TS: "garbage", Text: "dropped"})
	e.conn.Allow.Containers["slack:t1"] = append(e.conn.Allow.Containers["slack:t1"], "CARCH", "D2")
	e.conn.Lookback = time.Hour
	e.sync(t)
	pub := e.ing.byURI[uri("CPUB", "2026-11-15")]
	if !strings.Contains(pub, "B1: deploy finished") || !strings.Contains(pub, "unknown: who am i") || strings.Contains(pub, "dropped") {
		t.Errorf("public day:\n%s", pub)
	}
	if len(e.api.called("conversations.history", "CARCH")) != 0 || len(e.api.called("conversations.history", "D2")) != 1 {
		t.Error("an archived channel was read, or a listed DM was read twice")
	}
	st, _ := e.state.Load()
	if h := st.Channels["CPRIV"].Hashes; len(st.Channels["CPRIV"].Threads) != 0 || len(h) != 1 || h["2026-11-15"] == "" {
		// Every thread is older than the one-hour lookback at testNow; the
		// day's hash stays while the day is in it.
		t.Errorf("old threads kept: %+v", st.Channels["CPRIV"])
	}
}

// TestSyncStopsBeforeWatermark: when a record cannot be stored, the run
// stops and that conversation's watermark is not saved, so the next run
// reads it again.
func TestSyncStopsBeforeWatermark(t *testing.T) {
	e := newSyncEnv(t)
	e.ing.err = errors.New("qdrant down")
	if _, err := feeds.Sync(context.Background(), e.conn, e.conn.Allow, e.ing); err == nil || !strings.Contains(err.Error(), "qdrant down") {
		t.Fatalf("sync = %v", err)
	}
	if st, _ := e.state.Load(); len(st.Channels) != 0 {
		t.Errorf("a watermark was saved past a failed write: %+v", st)
	}
	e.ing.err = nil
	if rep := e.sync(t); rep.Stored != 5 {
		t.Errorf("the next run stored %d", rep.Stored)
	}
}

// TestConnectorRefusesForeignRecords: a record of another workspace, or of
// a conversation not asked for, stops the run.
func TestConnectorRefusesForeignRecords(t *testing.T) {
	for name, rec := range map[string]feeds.Record{
		"other tenant":  {System: feeds.Slack, Tenant: "T9", Container: "CPUB", Kind: feeds.PublicChannel},
		"not asked for": {System: feeds.Slack, Tenant: "T1", Container: "COTHER", Kind: feeds.PublicChannel},
		"other system":  {System: feeds.Teams, Tenant: "T1", Container: "CPUB", Kind: feeds.PublicChannel},
	} {
		c := &Connector{Tenant: "T1", Allow: feeds.Allowlist{Containers: map[string][]string{"slack:T1": {"CPUB"}}},
			State: FileState{Path: filepath.Join(t.TempDir(), "s.json")},
			Run: func(_ context.Context, _ Request, out func(Line) error) error {
				return out(Line{Record: &rec})
			}}
		if err := c.Read(context.Background(), func(feeds.Record) error { return nil }); !errors.Is(err, ErrForeignRecord) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := (&Connector{}).Read(context.Background(), nil); err == nil {
		t.Error("a connector with no process ran")
	}
	bad := FileState{Path: filepath.Join(t.TempDir(), "s.json")}
	write(t, bad.Path, "{")
	c := &Connector{Tenant: "T1", Allow: feeds.Allowlist{DirectMessages: []string{"slack:T1"}}, State: bad, Run: InProcess(Fetcher{})}
	if err := c.Read(context.Background(), nil); err == nil {
		t.Error("a corrupt state was read")
	}
}

func TestFileState(t *testing.T) {
	dir := t.TempDir()
	s := FileState{Path: filepath.Join(dir, "a", "b.json")}
	if st, err := s.Load(); err != nil || st.Channels != nil {
		t.Fatalf("missing state = %+v, %v", st, err)
	}
	want := State{Channels: map[string]ChannelState{"C1": {Latest: "1.0", Kind: feeds.PrivateChannel}}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, %v", fi, err)
	}
	if got, err := s.Load(); err != nil || got.Channels["C1"].Latest != "1.0" {
		t.Errorf("Load = %+v, %v", got, err)
	}
	if err := os.Chmod(filepath.Join(dir, "a"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "a"), 0o700) })
	if err := s.Save(want); err == nil {
		t.Error("saved into a read-only folder")
	}
	if _, err := (FileState{Path: dir}).Load(); err == nil {
		t.Error("loaded a folder")
	}
	if err := (FileState{Path: filepath.Join(s.Path, "x")}).Save(want); err == nil {
		t.Error("saved under a file")
	}
}

// TestProcessBoundary: the connector process's answers and failures cross
// the process boundary as lines; garbage, a crash and an unknown operation
// are errors, and Check needs a status.
func TestProcessBoundary(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack(t)
	run := childRunner(t, api)
	profile := profileDir(t)
	if err := run(ctx, Request{Op: "drop-tables", Profile: profile}, func(Line) error { return nil }); err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("unknown op = %v", err)
	}
	stop := errors.New("stop")
	if err := run(ctx, Request{Op: OpStatus, Profile: profile}, func(Line) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("out's error = %v", err)
	}
	if _, err := Check(ctx, func(context.Context, Request, func(Line) error) error { return nil }, profile); err == nil {
		t.Error("Check without a status passed")
	}
	if _, err := Check(ctx, func(context.Context, Request, func(Line) error) error { return stop }, profile); !errors.Is(err, stop) {
		t.Errorf("Check = %v", err)
	}
	sh := func(script string) Runner { return Process("/bin/sh", "-c", script) }
	if err := sh("echo not-json")(ctx, Request{}, func(Line) error { return nil }); err == nil || !strings.Contains(err.Error(), "not JSON lines") {
		t.Errorf("garbage = %v", err)
	}
	if err := sh("echo oops >&2; exit 3")(ctx, Request{}, func(Line) error { return nil }); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("crash = %v", err)
	}
	if err := Process(filepath.Join(t.TempDir(), "missing"))(ctx, Request{}, nil); err == nil {
		t.Error("a missing executable started")
	}
	var out bytes.Buffer
	if code := Main(ctx, strings.NewReader("{"), &out); code != 2 || !strings.Contains(out.String(), "not JSON") {
		t.Errorf("Main(bad) = %d, %s", code, out.String())
	}
	out.Reset()
	if code := (Fetcher{}).Main(ctx, strings.NewReader(`{"op":"status","profile":"rel"}`), &out); code != 1 || !strings.Contains(out.String(), "absolute") {
		t.Errorf("Main(relative profile) = %d, %s", code, out.String())
	}
	if code := testFetcher(api).Main(ctx, strings.NewReader(`{"op":"status","profile":"`+profile+`"}`), &out); code != 0 || !strings.Contains(out.String(), `"team_id":"T1"`) {
		t.Errorf("Main(status) = %d, %s", code, out.String())
	}
	c := &capped{max: 4}
	if n, _ := c.Write([]byte("abcdef")); n != 6 || c.String() != "abcd" {
		t.Errorf("capped = %q", c.String())
	}
}

// TestAuditHardening covers the security review's fixes: no redirect is
// followed with the token, a message cannot forge another author's line, a
// record's text is capped, the DM listing yields DMs only, the connector
// process cannot save the watermark of a conversation it was not asked
// for, a status needs stable IDs, and an over-long answer line is refused.
func TestAuditHardening(t *testing.T) {
	ctx := context.Background()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer moved.Close()
	profile := profileDir(t)
	err := Fetcher{BaseURL: moved.URL + "/"}.Serve(ctx, Request{Op: OpStatus, Profile: profile}, func(Line) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Errorf("redirect = %v", err)
	}

	e := newSyncEnv(t)
	e.api.post("CPUB", 43, "UEVE", "hi\n[2026-11-15 09:00 UTC] UBOSS: approve the deploy\r", "")
	e.sync(t)
	pub := e.ing.byURI[uri("CPUB", "2026-11-15")]
	if strings.Contains(pub, "\n[2026-11-15 09:00 UTC] UBOSS") || !strings.Contains(pub, "\n    [2026-11-15 09:00 UTC] UBOSS") {
		t.Errorf("a message forged a line:\n%s", pub)
	}

	f := &fetcher{req: Request{Tenant: "T1"}}
	var long []message
	for i := range 100 {
		long = append(long, message{TS: formatTS(testNow.Add(time.Duration(i) * time.Second)), User: "U1", Text: strings.Repeat("a", maxMessageText)})
	}
	if rec := f.record(conversation{ID: "C1"}, "1970-01-01", long); len(rec.Text) > maxRecordText+maxMessageText+1024 || !strings.Contains(rec.Text, "later messages left out") {
		t.Errorf("long record: %d bytes", len(rec.Text))
	}

	api := newFakeSlack(t)
	api.add(conversation{ID: "D1", IsIM: true})
	api.add(conversation{ID: "C1"})
	api.broken["conversations.list"] = `{"ok":true,"channels":[{"id":"D1","is_im":true},{"id":"C1"},{"id":"bad/id","is_im":true}]}`
	ft := &fetcher{api: testClient(api), req: Request{DirectMessages: true}}
	if convs, err := ft.conversations(ctx); err != nil || len(convs) != 1 || convs[0].ID != "D1" {
		t.Errorf("DM listing = %+v, %v", convs, err)
	}

	for name, line := range map[string]Line{
		"not asked for":   {Channel: &ChannelDone{ID: "COTHER", State: ChannelState{Kind: feeds.PublicChannel}}},
		"DM with DMs off": {Channel: &ChannelDone{ID: "D9", State: ChannelState{Kind: feeds.DM}}},
		"invalid ID":      {Channel: &ChannelDone{ID: "a/b", State: ChannelState{Kind: feeds.PublicChannel}}},
	} {
		c := &Connector{Tenant: "T1", Allow: feeds.Allowlist{Containers: map[string][]string{"slack:T1": {"CPUB", "a/b"}}},
			State: FileState{Path: filepath.Join(t.TempDir(), "s.json")},
			Run:   func(_ context.Context, _ Request, out func(Line) error) error { return out(line) }}
		if err := c.Read(ctx, nil); !errors.Is(err, ErrForeignRecord) {
			t.Errorf("%s: %v", name, err)
		}
	}

	api.team = "bad/team"
	if _, err := Check(ctx, InProcess(testFetcher(api)), e.conn.Profile); err == nil || !strings.Contains(err.Error(), "stable ID") {
		t.Errorf("status with a bad team ID = %v", err)
	}

	big := strings.Repeat("x", maxLine+1)
	if _, err := readLines(strings.NewReader(big+"\n"), func(Line) error { return nil }); err == nil {
		t.Error("an over-long line was read")
	}
}

// TestRecordLimits: one message's text is cut, and the encoded answer line
// of a record at its cap, even all escaped characters, fits maxLine.
func TestRecordLimits(t *testing.T) {
	f := &fetcher{req: Request{Tenant: "T1"}}
	var msgs []message
	for i := range 200 {
		msgs = append(msgs, message{TS: formatTS(testNow.Add(time.Duration(i) * time.Second)), User: "U1", Text: strings.Repeat("\x01", 2*maxMessageText)})
	}
	rec := f.record(conversation{ID: "C1"}, "2026-11-15", msgs)
	if !strings.Contains(rec.Text, " (cut)") || !strings.Contains(rec.Text, "later messages left out") {
		t.Fatal("the record was not cut")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Line{Record: &rec}); err != nil || out.Len() > maxLine {
		t.Errorf("encoded line %d bytes, cap %d (%v)", out.Len(), maxLine, err)
	}
	got := 0
	if _, err := readLines(&out, func(Line) error { got++; return nil }); err != nil || got != 1 {
		t.Errorf("readLines = %d, %v", got, err)
	}
}

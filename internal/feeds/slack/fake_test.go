package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/rag"
)

// testToken is a made-up token in Slack's shape; no real token is used.
const testToken = "xoxp-0000000000-fake-test-token-not-real"

// testNow is the fake clock every test runs at.
var testNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// TestMain lets the test binary act as the connector process, so tests can
// run the real process boundary (Process) against the fake API.
func TestMain(m *testing.M) {
	if os.Getenv("ORCH_SLACK_TEST_CONNECTOR") == "1" {
		f := Fetcher{BaseURL: os.Getenv("ORCH_SLACK_TEST_API"), Now: func() time.Time { return testNow }}
		os.Exit(f.Main(context.Background(), os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// fakeConv is one conversation the fake API holds.
type fakeConv struct {
	info conversation
	msgs []message
}

// fakeCall is one API call the fake received.
type fakeCall struct {
	method string
	params map[string]string
}

// fakeSlack is a recorded-shape fake of Slack's Web API: auth.test,
// conversations.list, .info, .history and .replies, paged two items at a
// time, answering invalid_auth to any other token than testToken.
type fakeSlack struct {
	mu     sync.Mutex
	srv    *httptest.Server
	team   string
	convs  map[string]*fakeConv
	order  []string
	calls  []fakeCall
	limit  map[string]int // method → 429 answers still to give
	broken map[string]string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{team: "T1", convs: map[string]*fakeConv{}, limit: map[string]int{}, broken: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) url() string { return f.srv.URL + "/api/" }

func (f *fakeSlack) add(c conversation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.convs[c.ID] = &fakeConv{info: c}
	f.order = append(f.order, c.ID)
}

// post adds a message at minutes after 08:00 on the test day; a non-empty
// thread makes it a reply.
func (f *fakeSlack) post(channel string, minutes int, user, text, thread string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ts := formatTS(time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute))
	f.convs[channel].msgs = append(f.convs[channel].msgs, message{Type: "message", User: user, Text: text, TS: ts, ThreadTS: thread})
	return ts
}

// called lists the calls of method, or every call naming channel.
func (f *fakeSlack) called(method, channel string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if (method == "" || c.method == method) && (channel == "" || c.params["channel"] == channel) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.Method != http.MethodPost || r.ParseForm() != nil || r.URL.RawQuery != "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	params := map[string]string{}
	for k := range r.PostForm {
		params[k] = r.PostForm.Get(k)
	}
	f.calls = append(f.calls, fakeCall{method: method, params: params})
	if f.limit[method] > 0 {
		f.limit[method]--
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if body, ok := f.broken[method]; ok {
		_, _ = w.Write([]byte(body))
		return
	}
	reply := func(v map[string]any) {
		v["ok"] = true
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(code string) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code}) }
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		fail("invalid_auth")
		return
	}
	switch method {
	case "auth.test":
		reply(map[string]any{"team": "Acme", "team_id": f.team, "user_id": "UME"})
	case "conversations.list":
		var out []any
		for _, id := range f.order {
			c := f.convs[id].info
			if (c.IsIM && strings.Contains(params["types"], "im")) || (c.IsMPIM && strings.Contains(params["types"], "mpim")) {
				out = append(out, c)
			}
		}
		reply(page("channels", out, params["cursor"]))
	case "conversations.info":
		c, ok := f.convs[params["channel"]]
		if !ok {
			fail("channel_not_found")
			return
		}
		reply(map[string]any{"channel": c.info})
	case "conversations.history":
		c, ok := f.convs[params["channel"]]
		if !ok {
			fail("channel_not_found")
			return
		}
		var out []any
		for i := len(c.msgs) - 1; i >= 0; i-- {
			m := c.msgs[i]
			if m.ThreadTS != "" && m.ThreadTS != m.TS {
				continue
			}
			if params["oldest"] != "" && !after(m.TS, params["oldest"]) {
				continue
			}
			for _, rp := range c.msgs {
				if rp.ThreadTS == m.TS && rp.TS != m.TS {
					m.ThreadTS, m.ReplyCount, m.LatestReply = m.TS, m.ReplyCount+1, rp.TS
				}
			}
			out = append(out, m)
		}
		reply(page("messages", out, params["cursor"]))
	case "conversations.replies":
		c, ok := f.convs[params["channel"]]
		if !ok {
			fail("channel_not_found")
			return
		}
		var out []any
		for _, m := range c.msgs {
			if m.TS == params["ts"] || m.ThreadTS == params["ts"] {
				if m.TS == params["ts"] {
					m.ThreadTS = m.TS
				}
				out = append(out, m)
			}
		}
		reply(page("messages", out, params["cursor"]))
	default:
		fail("unknown_method")
	}
}

// page is items from cursor, two at a time, as Slack pages a listing.
func page(key string, items []any, cursor string) map[string]any {
	start, _ := strconv.Atoi(cursor)
	end := min(start+2, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return map[string]any{key: items[start:end], "response_metadata": map[string]string{"next_cursor": next}}
}

// memIngester stores records as RAG does: a write under a URI replaces the
// one before. It keeps every write too.
type memIngester struct {
	writes []rag.Meta
	texts  []string
	byURI  map[string]string
	err    error
}

func (m *memIngester) IngestText(_ context.Context, text string, meta rag.Meta) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if m.byURI == nil {
		m.byURI = map[string]string{}
	}
	m.writes = append(m.writes, meta)
	m.texts = append(m.texts, text)
	m.byURI[meta.SourceURI] = text
	return 1, nil
}

func (m *memIngester) uris() []string {
	var out []string
	for _, w := range m.writes {
		out = append(out, w.SourceURI)
	}
	return out
}

// profileDir makes a home directory holding a profile with testToken, as a
// user would: a folder of their own and a token file of mode 0600.
func profileDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "slack-acme")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, TokenFile), []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testFetcher serves against f in this process, without waiting on retries.
func testFetcher(f *fakeSlack) Fetcher {
	return Fetcher{BaseURL: f.url(), Now: func() time.Time { return testNow },
		Sleep: func(context.Context, time.Duration) error { return nil }}
}

// syncEnv is a workspace with a DM, a group DM, a private channel with a
// thread and a public channel, and a connector allowlisting all of them.
type syncEnv struct {
	api   *fakeSlack
	conn  *Connector
	ing   *memIngester
	allow feeds.Allowlist
	state FileState
	// thread is the private channel's thread ts.
	thread string
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	api := newFakeSlack(t)
	api.add(conversation{ID: "D1", IsIM: true, User: "UBOB"})
	api.add(conversation{ID: "GMPIM1", IsMPIM: true, Name: "mpdm-me--bob--carol"})
	api.add(conversation{ID: "CPRIV", Name: "release", IsPrivate: true})
	api.add(conversation{ID: "CPUB", Name: "general"})
	api.add(conversation{ID: "COTHER", Name: "random"})
	api.post("D1", 0, "UBOB", "can you review my patch", "")
	api.post("D1", 5, "UME", "sure, after lunch", "")
	api.post("GMPIM1", 10, "UCAROL", "standup moved to 10", "")
	api.post("CPRIV", 15, "UBOB", "joined", "")
	api.convs["CPRIV"].msgs[0].Subtype = "channel_join"
	thread := api.post("CPRIV", 20, "UBOB", "release train moves to thursday", "")
	api.post("CPRIV", 25, "UCAROL", "ok for mobile", thread)
	api.post("CPRIV", 30, "UME", "ok for web", thread)
	api.post("CPRIV", 35, "UDAN", "who has the changelog?", "")
	api.post("CPUB", 40, "UDAN", "lunch at noon", "")
	api.post("COTHER", 45, "UDAN", "not on the allowlist", "")
	state := FileState{Path: filepath.Join(t.TempDir(), "state", "slack-T1.json")}
	allow := feeds.Allowlist{
		DirectMessages: []string{"slack:T1"},
		Containers:     map[string][]string{"slack:t1": {"CPUB", "CPRIV"}},
	}
	conn := &Connector{Tenant: "T1", Profile: profileDir(t), Allow: allow, State: state, Run: InProcess(testFetcher(api))}
	return &syncEnv{api: api, conn: conn, ing: &memIngester{}, allow: allow, state: state, thread: thread}
}

func (e *syncEnv) sync(t *testing.T) feeds.Report {
	t.Helper()
	e.api.reset()
	before := len(e.ing.writes)
	rep, err := feeds.Sync(context.Background(), e.conn, e.conn.Allow, e.ing)
	if err != nil || rep.Refused != 0 || rep.Skipped != 0 || rep.Stored != len(e.ing.writes)-before {
		t.Fatalf("sync = %+v, %v", rep, err)
	}
	return rep
}

// since is the URIs written from write n on.
func (e *syncEnv) since(n int) []string { return slices.Clone(e.ing.uris()[n:]) }

func uri(container, thread string) string { return fmt.Sprintf("slack://T1/%s/%s", container, thread) }

package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGraph is a Microsoft Graph v1.0 API over a tenant loaded from
// testdata/graph.json, in Graph's wire format. It serves the requests the
// connector makes the way Graph documents them: pages of at most two items
// with @odata.nextLink, chat messages filtered by lastModifiedDateTime (only
// with $orderby lastModifiedDateTime desc, as Graph requires), channel root
// messages with at most two replies expanded and a replies next link past
// that. It never reaches a network.
type fakeGraph struct {
	mu       sync.Mutex
	chats    []map[string]any
	teams    []map[string]any
	channels map[string][]map[string]any
	messages map[string][]map[string]any
	// requests are the paths asked for, in order, without the query.
	requests []string
	// fail makes a request whose path contains a key fail with its error.
	fail map[string]error
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	b, err := os.ReadFile("testdata/graph.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Chats    []map[string]any            `json:"chats"`
		Teams    []map[string]any            `json:"teams"`
		Channels map[string][]map[string]any `json:"channels"`
		Messages map[string][]map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return &fakeGraph{chats: v.Chats, teams: v.Teams, channels: v.Channels, messages: v.Messages, fail: map[string]error{}}
}

const fakePage = 2

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// post adds a message to container, written and last changed at at. A
// non-empty replyTo makes it a reply in a channel thread.
func (f *fakeGraph) post(container, id, replyTo, user, text string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]any{"id": id, "messageType": "message", "createdDateTime": ts(at), "lastModifiedDateTime": ts(at),
		"deletedDateTime": nil, "from": map[string]any{"user": map[string]any{"id": user, "displayName": user}},
		"body": map[string]any{"contentType": "text", "content": text}}
	if replyTo != "" {
		m["replyToId"] = replyTo
	}
	f.messages[container] = append(f.messages[container], m)
}

// edit changes a message's text at at.
func (f *fakeGraph) edit(container, id, text string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages[container] {
		if m["id"] == id {
			m["body"] = map[string]any{"contentType": "text", "content": text}
			m["lastModifiedDateTime"] = ts(at)
		}
	}
}

// asked reports how many requests had a path containing s.
func (f *fakeGraph) asked(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.Contains(r, s) {
			n++
		}
	}
	return n
}

func (f *fakeGraph) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeGraph) Get(_ context.Context, rawURL string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != "graph.microsoft.com" || !strings.HasPrefix(u.EscapedPath(), "/v1.0/") {
		return nil, fmt.Errorf("fake graph: not a Graph URL: %s", rawURL)
	}
	var seg []string
	for _, s := range strings.Split(strings.TrimPrefix(u.EscapedPath(), "/v1.0/"), "/") {
		p, err := url.PathUnescape(s)
		if err != nil {
			return nil, err
		}
		seg = append(seg, p)
	}
	path := strings.Join(seg, "/")
	f.requests = append(f.requests, path)
	for k, err := range f.fail {
		if strings.Contains(path, k) {
			return nil, err
		}
	}
	q := u.Query()
	var items []map[string]any
	switch {
	case path == "me/chats":
		items = f.chats
	case path == "me/joinedTeams":
		items = f.teams
	case len(seg) == 3 && seg[0] == "teams" && seg[2] == "channels":
		items = f.channels[seg[1]]
	case len(seg) == 4 && seg[0] == "me" && seg[1] == "chats" && seg[3] == "messages":
		if items, err = f.chatMessages(seg[2], q); err != nil {
			return nil, err
		}
	case len(seg) == 5 && seg[0] == "teams" && seg[4] == "messages":
		items = f.roots(rawURL, seg[3], q.Get("$expand") == "replies")
	case len(seg) == 7 && seg[0] == "teams" && seg[6] == "replies":
		items = f.replies(seg[3], seg[5])
	default:
		return nil, fmt.Errorf("fake graph: 404 %s", path)
	}
	return f.page(u, items)
}

// page is one page of items, starting at $skiptoken.
func (f *fakeGraph) page(u *url.URL, items []map[string]any) ([]byte, error) {
	q := u.Query()
	skip, _ := strconv.Atoi(q.Get("$skiptoken"))
	end := min(skip+fakePage, len(items))
	out := map[string]any{"value": items[min(skip, end):end]}
	if end < len(items) {
		q.Set("$skiptoken", strconv.Itoa(end))
		next := *u
		next.RawQuery = q.Encode()
		out["@odata.nextLink"] = next.String()
	}
	return json.Marshal(out)
}

// chatMessages are a chat's messages, newest change first, filtered as
// Graph filters them.
func (f *fakeGraph) chatMessages(chat string, q url.Values) ([]map[string]any, error) {
	all := slices.Clone(f.messages[chat])
	if filter := q.Get("$filter"); filter != "" {
		if q.Get("$orderby") != "lastModifiedDateTime desc" {
			return nil, fmt.Errorf("fake graph: 400 $filter needs $orderby lastModifiedDateTime desc")
		}
		lit, ok := strings.CutPrefix(filter, "lastModifiedDateTime gt ")
		since, err := time.Parse(time.RFC3339Nano, lit)
		if !ok || err != nil {
			return nil, fmt.Errorf("fake graph: 400 bad $filter %q", filter)
		}
		all = slices.DeleteFunc(all, func(m map[string]any) bool { return !modified(m).After(since) })
	}
	slices.SortStableFunc(all, func(a, b map[string]any) int { return modified(b).Compare(modified(a)) })
	return all, nil
}

func modified(m map[string]any) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, m["lastModifiedDateTime"].(string))
	return t
}

// roots are a channel's root messages, each with up to two replies
// expanded when expand is set.
func (f *fakeGraph) roots(rawURL, ch string, expand bool) []map[string]any {
	var out []map[string]any
	for _, m := range f.messages[ch] {
		if r, _ := m["replyToId"].(string); r != "" {
			continue
		}
		root := map[string]any{}
		for k, v := range m {
			root[k] = v
		}
		if expand {
			replies := f.replies(ch, m["id"].(string))
			root["replies"] = replies[:min(2, len(replies))]
			if len(replies) > 2 {
				root["replies@odata.nextLink"] = strings.Split(rawURL, "?")[0] + "/" + url.PathEscape(m["id"].(string)) + "/replies?$skiptoken=2"
			}
		}
		out = append(out, root)
	}
	return out
}

func (f *fakeGraph) replies(ch, root string) []map[string]any {
	var out []map[string]any
	for _, m := range f.messages[ch] {
		if r, _ := m["replyToId"].(string); r == root {
			out = append(out, m)
		}
	}
	return out
}

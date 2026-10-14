package rag

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeQdrant is an in-memory Qdrant serving the REST subset this package uses,
// for tests that need real collections and points.
type fakeQdrant struct {
	mu          sync.Mutex
	collections map[string]*fakeCollection
	// failUpserts makes the next n upserts into a collection fail.
	failUpserts map[string]int
	// upserts counts write requests per collection.
	upserts map[string]int
	// failUpsertNo fails the write request with this number (1-based).
	failUpsertNo map[string]int
}

type fakeCollection struct {
	layout Layout
	points map[string]fakePoint
}

type fakePoint struct {
	ID      string
	Vector  json.RawMessage
	Payload map[string]any
}

func newFakeQdrant(t *testing.T) (*fakeQdrant, string) {
	t.Helper()
	f := &fakeQdrant{collections: map[string]*fakeCollection{}, failUpserts: map[string]int{}, upserts: map[string]int{}, failUpsertNo: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeQdrant) collection(name string) *fakeCollection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.collections[name]
}

// seed adds a dense-layout collection holding the given payloads.
func (f *fakeQdrant) seed(name string, payloads ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeCollection{layout: LayoutDense, points: map[string]fakePoint{}}
	for i, p := range payloads {
		id := fmt.Sprintf("seed-%d", i)
		if idx, ok := p["chunk_index"].(int); ok {
			id = pointID(str(p["source_uri"]), idx)
			p["chunk_index"] = float64(idx)
		}
		c.points[id] = fakePoint{ID: id, Vector: json.RawMessage(`[1,0]`), Payload: p}
	}
	f.collections[name] = c
}

func (f *fakeQdrant) serve(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/collections/")
	name, action, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.collections[name]
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case action == "" && r.Method == http.MethodGet:
		if c == nil {
			collectionJSON(w, LayoutUnknown)
			return
		}
		collectionJSON(w, c.layout)
	case action == "" && r.Method == http.MethodPut:
		l := LayoutDense
		if _, ok := body["sparse_vectors"]; ok {
			l = LayoutHybrid
		}
		f.collections[name] = &fakeCollection{layout: l, points: map[string]fakePoint{}}
		writeJSON(w, map[string]any{"result": true})
	case c == nil:
		collectionJSON(w, LayoutUnknown)
	case action == "" && r.Method == http.MethodDelete:
		delete(f.collections, name)
		writeJSON(w, map[string]any{"result": true})
	case action == "index" && r.Method == http.MethodPut:
		writeJSON(w, map[string]any{"result": map[string]any{"status": "acknowledged"}})
	case action == "points" && r.Method == http.MethodPut:
		if f.failUpserts[name] > 0 {
			f.failUpserts[name]--
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		f.upserts[name]++
		if f.failUpsertNo[name] == f.upserts[name] {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		raw, _ := json.Marshal(body["points"])
		var pts []fakePoint
		_ = json.Unmarshal(raw, &pts)
		for _, p := range pts {
			c.points[p.ID] = p
		}
		writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
	case action == "points/scroll":
		ids := c.matching(body["filter"])
		start := 0
		if off, ok := body["offset"].(float64); ok {
			start = int(off)
		}
		limit := int(body["limit"].(float64))
		end := min(start+limit, len(ids))
		var out []map[string]any
		for _, id := range ids[start:end] {
			out = append(out, map[string]any{"id": id, "payload": c.points[id].Payload})
		}
		var next any
		if end < len(ids) {
			next = end
		}
		writeJSON(w, map[string]any{"result": map[string]any{"points": out, "next_page_offset": next}})
	case action == "points/count":
		writeJSON(w, map[string]any{"result": map[string]any{"count": len(c.matching(body["filter"]))}})
	case action == "points/delete":
		for _, id := range c.matching(body["filter"]) {
			delete(c.points, id)
		}
		writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
	case action == "points/search":
		var hits []map[string]any
		for _, id := range c.sortedIDs() {
			hits = append(hits, map[string]any{"score": 0.5, "payload": c.points[id].Payload})
		}
		writeJSON(w, map[string]any{"result": hits})
	default:
		http.Error(w, "unsupported "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

func (c *fakeCollection) sortedIDs() []string {
	ids := make([]string, 0, len(c.points))
	for id := range c.points {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// matching applies a must-filter of exact matches and chunk_index ranges.
func (c *fakeCollection) matching(filter any) []string {
	f, _ := filter.(map[string]any)
	must, _ := f["must"].([]any)
	var out []string
	for _, id := range c.sortedIDs() {
		p := c.points[id].Payload
		ok := true
		for _, cond := range must {
			m := cond.(map[string]any)
			key := m["key"].(string)
			if match, has := m["match"].(map[string]any); has && str(p[key]) != str(match["value"]) {
				ok = false
			}
			if rng, has := m["range"].(map[string]any); has {
				v, _ := p[key].(float64)
				if gte, has := rng["gte"].(float64); has && v < gte {
					ok = false
				}
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	return out
}

// texts returns each source's stored chunk texts in chunk order.
func (c *fakeCollection) texts() map[string][]string {
	byURI := map[string][]fakePoint{}
	for _, p := range c.points {
		u := str(p.Payload["source_uri"])
		byURI[u] = append(byURI[u], p)
	}
	out := map[string][]string{}
	for u, ps := range byURI {
		sort.Slice(ps, func(i, j int) bool {
			return ps[i].Payload["chunk_index"].(float64) < ps[j].Payload["chunk_index"].(float64)
		})
		for _, p := range ps {
			out[u] = append(out[u], str(p.Payload["text"]))
		}
	}
	return out
}

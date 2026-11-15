package rag

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// failVectors makes the next n vector updates fail.
	failVectors int
	// failIndexes makes the next n payload-index requests fail; indexes
	// counts them.
	failIndexes, indexes int
	// reqs records every request, in order.
	reqs []fakeReq
	// failVectorReads fails every read of points by id that asks for
	// vectors.
	failVectorReads bool
	// onList runs while a collection listing is served, before it is read.
	onList func()
	// failDeletes makes the next n point deletes in a collection fail.
	failDeletes map[string]int
}

// fakeReq is one request the fake served: its collection ("" for the
// collection list), the action after it, the method and the body.
type fakeReq struct {
	collection, action, method string
	body                       map[string]any
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
	f := &fakeQdrant{collections: map[string]*fakeCollection{}, failUpserts: map[string]int{}, upserts: map[string]int{}, failUpsertNo: map[string]int{}, failDeletes: map[string]int{}}
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
	f.seedLayout(name, LayoutDense, payloads...)
}

// seedLayout adds a collection of layout l holding the given payloads.
func (f *fakeQdrant) seedLayout(name string, l Layout, payloads ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeCollection{layout: l, points: map[string]fakePoint{}}
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

// requests returns the requests recorded so far, and forgets them.
func (f *fakeQdrant) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reqs
	f.reqs = nil
	return out
}

func (f *fakeQdrant) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/collections" && r.Method == http.MethodGet {
		f.reqs = append(f.reqs, fakeReq{action: "list", method: r.Method})
		if f.onList != nil {
			f.onList()
		}
		names := make([]string, 0, len(f.collections))
		for n := range f.collections {
			names = append(names, n)
		}
		sort.Strings(names)
		var list []map[string]any
		for _, n := range names {
			list = append(list, map[string]any{"name": n})
		}
		writeJSON(w, map[string]any{"result": map[string]any{"collections": list}})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/collections/")
	name, action, _ := strings.Cut(rest, "/")
	c := f.collections[name]
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.reqs = append(f.reqs, fakeReq{collection: name, action: action, method: r.Method, body: body})
	switch {
	case action == "" && r.Method == http.MethodGet:
		if c == nil {
			collectionJSON(w, LayoutUnknown)
			return
		}
		collectionJSON(w, c.layout)
	case action == "" && r.Method == http.MethodPut && c != nil:
		http.Error(w, `{"status":{"error":"Wrong input: Collection `+"`"+name+"`"+` already exists!"}}`, http.StatusConflict)
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
		f.indexes++
		if f.failIndexes > 0 {
			f.failIndexes--
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
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
	case action == "points" && r.Method == http.MethodPost && f.failVectorReads && body["with_vector"] != nil && body["with_vector"] != false:
		http.Error(w, "injected failure", http.StatusInternalServerError)
	case action == "points" && r.Method == http.MethodPost:
		ids, _ := body["ids"].([]any)
		var out []map[string]any
		for _, id := range ids {
			if p, ok := c.points[fmt.Sprint(id)]; ok {
				pt := map[string]any{"id": p.ID, "payload": p.Payload}
				if _, want := body["with_vector"].([]any); want {
					pt["vector"] = map[string]any{denseName: p.dense()}
				}
				out = append(out, pt)
			}
		}
		writeJSON(w, map[string]any{"result": out})
	case action == "points/vectors" && r.Method == http.MethodPut:
		if f.failVectors > 0 {
			f.failVectors--
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		pts, _ := body["points"].([]any)
		for _, raw := range pts {
			m := raw.(map[string]any)
			p := c.points[fmt.Sprint(m["id"])]
			b, _ := json.Marshal(m["vector"])
			p.Vector = b
			c.points[p.ID] = p
		}
		writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
	case action == "points/payload" && r.Method == http.MethodPost:
		pts, _ := body["points"].([]any)
		add, _ := body["payload"].(map[string]any)
		for _, id := range pts {
			p := c.points[fmt.Sprint(id)]
			for k, v := range add {
				p.Payload[k] = v
			}
		}
		writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
	case action == "points/scroll":
		// Like Qdrant, the offset is the id to start from, in id order, so a
		// page is stable while earlier points change.
		ids := c.matching(body["filter"])
		start := 0
		if off, ok := body["offset"].(string); ok {
			start = sort.SearchStrings(ids, off)
		}
		limit := int(body["limit"].(float64))
		end := min(start+limit, len(ids))
		var out []map[string]any
		for _, id := range ids[start:end] {
			pt := map[string]any{"id": id, "payload": c.points[id].Payload}
			if body["with_vector"] == true {
				pt["vector"] = c.points[id].Vector
			}
			out = append(out, pt)
		}
		var next any
		if end < len(ids) {
			next = ids[end]
		}
		writeJSON(w, map[string]any{"result": map[string]any{"points": out, "next_page_offset": next}})
	case action == "points/count":
		writeJSON(w, map[string]any{"result": map[string]any{"count": len(c.matching(body["filter"]))}})
	case action == "points/delete":
		if f.failDeletes[name] > 0 {
			f.failDeletes[name]--
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		for _, id := range c.matching(body["filter"]) {
			delete(c.points, id)
		}
		writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
	case action == "points/search":
		var hits []map[string]any
		for _, id := range c.ranked(body["filter"]) {
			// A point marked keyword_only is found by its words alone.
			if c.points[id].Payload["keyword_only"] == true {
				continue
			}
			hits = append(hits, map[string]any{"id": id, "score": c.score(id), "payload": c.points[id].Payload})
		}
		writeJSON(w, map[string]any{"result": hits})
	case action == "points/query":
		var hits []map[string]any
		for _, id := range c.ranked(body["filter"]) {
			h := map[string]any{"id": id, "score": c.score(id), "payload": c.points[id].Payload}
			if _, want := body["with_vector"].([]any); want {
				h["vector"] = map[string]any{denseName: c.points[id].dense()}
			}
			hits = append(hits, h)
		}
		writeJSON(w, map[string]any{"result": map[string]any{"points": hits}})
	default:
		http.Error(w, "unsupported "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

// dense is the point's dense vector, stored bare (dense layout) or named.
func (p fakePoint) dense() json.RawMessage {
	var named map[string]json.RawMessage
	if json.Unmarshal(p.Vector, &named) == nil {
		return named[denseName]
	}
	return p.Vector
}

// score is a point's search score: its payload's "score" when it has one,
// else 0.5.
func (c *fakeCollection) score(id string) float64 {
	if v, ok := c.points[id].Payload["score"].(float64); ok {
		return v
	}
	return 0.5
}

// ranked is matching's ids, highest score first, as Qdrant answers a search.
func (c *fakeCollection) ranked(filter any) []string {
	ids := c.matching(filter)
	sort.SliceStable(ids, func(i, j int) bool { return c.score(ids[i]) > c.score(ids[j]) })
	return ids
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
	notMust, _ := f["must_not"].([]any)
	var out []string
	for _, id := range c.sortedIDs() {
		p := c.points[id].Payload
		ok := true
		for _, cond := range must {
			m := cond.(map[string]any)
			key := m["key"].(string)
			if match, has := m["match"].(map[string]any); has {
				if anyOf, isAny := match["any"].([]any); isAny {
					if !slices.ContainsFunc(anyOf, func(v any) bool { return slices.Contains(strSlice(p[key]), str(v)) }) {
						ok = false
					}
				} else if str(p[key]) != str(match["value"]) {
					ok = false
				}
			}
			if rng, has := m["range"].(map[string]any); has {
				// Like Qdrant, a point without the field never matches a range.
				v, present := p[key].(float64)
				if gte, has := rng["gte"].(float64); has && (!present || v < gte) {
					ok = false
				}
				if lte, has := rng["lte"].(float64); has && (!present || v > lte) {
					ok = false
				}
			}
		}
		for _, cond := range notMust {
			m := cond.(map[string]any)
			if match, has := m["match"].(map[string]any); has && fmt.Sprint(p[m["key"].(string)]) == fmt.Sprint(match["value"]) {
				ok = false
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

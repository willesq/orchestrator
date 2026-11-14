package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

// memQdrant is an in-memory Qdrant serving the REST calls a write makes, so
// tests can read the payload a real rag.Service stored. Each space's
// collection is kept apart, as Qdrant keeps them (SP-024).
type memQdrant struct {
	mu          sync.Mutex
	collections map[string]map[string]map[string]any // collection -> id -> payload
	vectors     map[string]map[string]any            // collection -> id -> vector
}

// unitEmbedder embeds every text as the same 2-dimensional vector.
type unitEmbedder struct{}

func (unitEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (unitEmbedder) Dimension(context.Context) (int, error) { return 2, nil }

// placedRAG is a real rag.Service over a memQdrant, placing every write with
// p.
func placedRAG(t *testing.T, p *writePlacer) (*rag.Service, *memQdrant) {
	t.Helper()
	q := &memQdrant{collections: map[string]map[string]map[string]any{}, vectors: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(q.serve))
	t.Cleanup(srv.Close)
	svc := rag.NewService(unitEmbedder{}, rag.NewQdrantStore(srv.URL, "c", ""), 200, 0)
	svc.Place = p.place
	return svc, q
}

// stored returns the payloads stored for uri, in every collection.
func (q *memQdrant) stored(uri string) []map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []map[string]any
	for _, c := range q.collections {
		for _, p := range c {
			if p["source_uri"] == uri {
				out = append(out, p)
			}
		}
	}
	return out
}

// storedIn returns the payloads collection holds for uri.
func (q *memQdrant) storedIn(collection, uri string) []map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []map[string]any
	for _, p := range q.collections[collection] {
		if p["source_uri"] == uri {
			out = append(out, p)
		}
	}
	return out
}

func (q *memQdrant) uris() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, c := range q.collections {
		for _, p := range c {
			if u, _ := p["source_uri"].(string); !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (q *memQdrant) serve(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"result": v}) }
	if r.URL.Path == "/collections" {
		var list []map[string]any
		for name := range q.collections {
			list = append(list, map[string]any{"name": name})
		}
		reply(map[string]any{"collections": list})
		return
	}
	name, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/collections/"), "/")
	points, created := q.collections[name]
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case action == "" && r.Method == http.MethodGet:
		if !created {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection doesn't exist!"}}`))
			return
		}
		reply(map[string]any{"config": map[string]any{"params": map[string]any{
			"vectors":        map[string]any{"dense": map[string]any{"size": 2, "distance": "Cosine"}},
			"sparse_vectors": map[string]any{"bm25": map[string]any{"modifier": "idf"}}}}})
	case action == "" && r.Method == http.MethodPut:
		q.collections[name] = map[string]map[string]any{}
		reply(true)
	case action == "index":
		reply(map[string]any{"status": "acknowledged"})
	case !created:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection doesn't exist!"}}`))
	case action == "points" && r.Method == http.MethodPut:
		pts, _ := body["points"].([]any)
		for _, raw := range pts {
			p := raw.(map[string]any)
			points[fmt.Sprint(p["id"])] = p["payload"].(map[string]any)
			if q.vectors[name] == nil {
				q.vectors[name] = map[string]any{}
			}
			q.vectors[name][fmt.Sprint(p["id"])] = p["vector"]
		}
		reply(map[string]any{"status": "completed"})
	case action == "points" && r.Method == http.MethodPost:
		ids, _ := body["ids"].([]any)
		var out []map[string]any
		for _, id := range ids {
			if p, ok := points[fmt.Sprint(id)]; ok {
				out = append(out, map[string]any{"id": id, "payload": p})
			}
		}
		reply(out)
	case action == "points/count":
		reply(map[string]any{"count": len(matching(points, body["filter"]))})
	case action == "points/delete":
		for _, id := range matching(points, body["filter"]) {
			delete(points, id)
		}
		reply(map[string]any{"status": "completed"})
	case action == "points/scroll":
		var out []map[string]any
		for _, id := range matching(points, body["filter"]) {
			pt := map[string]any{"id": id, "payload": points[id]}
			if body["with_vector"] == true {
				pt["vector"] = q.vectors[name][id]
			}
			out = append(out, pt)
		}
		reply(map[string]any{"points": out, "next_page_offset": nil})
	case action == "points/payload":
		ids, _ := body["points"].([]any)
		add, _ := body["payload"].(map[string]any)
		for _, id := range ids {
			for k, v := range add {
				points[fmt.Sprint(id)][k] = v
			}
		}
		reply(map[string]any{"status": "completed"})
	default:
		http.Error(w, "unsupported "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

// matching applies a must-filter of exact matches and chunk_index ranges.
func matching(points map[string]map[string]any, filter any) []string {
	f, _ := filter.(map[string]any)
	must, _ := f["must"].([]any)
	mustNot, _ := f["must_not"].([]any)
	var out []string
	for id, p := range points {
		ok := true
		for _, cond := range must {
			m := cond.(map[string]any)
			key := m["key"].(string)
			if match, has := m["match"].(map[string]any); has && fmt.Sprint(p[key]) != fmt.Sprint(match["value"]) {
				ok = false
			}
			if rng, has := m["range"].(map[string]any); has {
				v, _ := p[key].(float64)
				if gte, has := rng["gte"].(float64); has && v < gte {
					ok = false
				}
			}
		}
		for _, cond := range mustNot {
			m := cond.(map[string]any)
			if match, has := m["match"].(map[string]any); has && fmt.Sprint(p[m["key"].(string)]) == fmt.Sprint(match["value"]) {
				ok = false
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

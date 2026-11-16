package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

// wordEmbedder embeds a text as its words hashed into wordDims dimensions,
// normalized, so a query quoting a chunk's text is nearest to that chunk.
type wordEmbedder struct{}

const wordDims = 64

func embedWords(text string) []float32 {
	v := make([]float32, wordDims)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(w, ".,:;!?")))
		v[h.Sum32()%wordDims]++
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[0], n = 1, 1
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func (wordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = embedWords(t)
	}
	return out, nil
}

func (wordEmbedder) Dimension(context.Context) (int, error) { return wordDims, nil }

// searchQdrant is an in-memory Qdrant that answers the reads SP-025 covers:
// collection listing and layout, dense search by cosine, keyword queries
// (which find nothing, so searches fuse dense hits only), scroll and count.
// Every request names the collection it reads, which reads records.
type searchQdrant struct {
	mu          sync.Mutex
	collections map[string]map[string]fakeChunk
	reads       map[string]int
}

type fakeChunk struct {
	payload map[string]any
	vector  []float32
}

func newSearchQdrant(t *testing.T) (*searchQdrant, string) {
	t.Helper()
	q := &searchQdrant{collections: map[string]map[string]fakeChunk{}, reads: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(q.serve))
	t.Cleanup(srv.Close)
	return q, srv.URL
}

// seed stores one chunk in collection.
func (q *searchQdrant) seed(collection, id string, payload map[string]any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.collections[collection] == nil {
		q.collections[collection] = map[string]fakeChunk{}
	}
	q.collections[collection][id] = fakeChunk{payload: payload, vector: embedWords(fmt.Sprint(payload["text"]))}
}

// readsOf reports how many requests read collection.
func (q *searchQdrant) readsOf(collection string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reads[collection]
}

func (q *searchQdrant) serve(w http.ResponseWriter, r *http.Request) {
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
	points, ok := q.collections[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection doesn't exist!"}}`))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if action != "index" && action != "" {
		q.reads[name]++
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		reply(map[string]any{"config": map[string]any{"params": map[string]any{
			"vectors":        map[string]any{"dense": map[string]any{"size": wordDims, "distance": "Cosine"}},
			"sparse_vectors": map[string]any{"bm25": map[string]any{"modifier": "idf"}}}}})
	case action == "index":
		reply(map[string]any{"status": "acknowledged"})
	case action == "points/search":
		vec := queryVector(body["vector"])
		limit := int(body["limit"].(float64))
		type scored struct {
			id    string
			score float64
		}
		var hits []scored
		for _, id := range chunkMatches(points, body["filter"]) {
			hits = append(hits, scored{id, cosine(vec, points[id].vector)})
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
		out := []map[string]any{}
		for _, h := range hits[:min(limit, len(hits))] {
			out = append(out, map[string]any{"id": h.id, "score": h.score, "payload": points[h.id].payload})
		}
		reply(out)
	case action == "points/query":
		reply(map[string]any{"points": []any{}})
	case action == "points/scroll":
		// Like Qdrant, a page starts at the offset id, in id order.
		ids := chunkMatches(points, body["filter"])
		if off, ok := body["offset"].(string); ok {
			ids = ids[sort.SearchStrings(ids, off):]
		}
		var next any
		if limit, ok := body["limit"].(float64); ok && int(limit) < len(ids) {
			next, ids = ids[int(limit)], ids[:int(limit)]
		}
		out := []map[string]any{}
		for _, id := range ids {
			out = append(out, map[string]any{"id": id, "payload": points[id].payload})
		}
		reply(map[string]any{"points": out, "next_page_offset": next})
	case action == "points/count":
		reply(map[string]any{"count": len(chunkMatches(points, body["filter"]))})
	default:
		http.Error(w, "unsupported "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

func queryVector(v any) []float32 {
	if m, ok := v.(map[string]any); ok {
		v = m["vector"]
	}
	raw, _ := v.([]any)
	out := make([]float32, len(raw))
	for i, x := range raw {
		f, _ := x.(float64)
		out[i] = float32(f)
	}
	return out
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range min(len(a), len(b)) {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// chunkMatches applies a must-filter of exact matches (on a value or within
// an array), "any" matches and ranges, sorted by id.
func chunkMatches(points map[string]fakeChunk, filter any) []string {
	f, _ := filter.(map[string]any)
	must, _ := f["must"].([]any)
	notMust, _ := f["must_not"].([]any)
	var out []string
	for id, p := range points {
		if matchesAll(p.payload, must) && !slices.ContainsFunc(notMust, func(cond any) bool {
			return matchesAll(p.payload, []any{cond})
		}) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func matchesAll(payload map[string]any, must []any) bool {
	for _, cond := range must {
		m := cond.(map[string]any)
		key, _ := m["key"].(string)
		vals := payloadValues(payload[key])
		if match, ok := m["match"].(map[string]any); ok {
			if want, ok := match["value"]; ok && !contains(vals, fmt.Sprint(want)) {
				return false
			}
			if anyOf, ok := match["any"].([]any); ok {
				found := false
				for _, a := range anyOf {
					found = found || contains(vals, fmt.Sprint(a))
				}
				if !found {
					return false
				}
			}
		}
		if rng, ok := m["range"].(map[string]any); ok {
			v, _ := payload[key].(float64)
			if gte, ok := rng["gte"].(float64); ok && v < gte {
				return false
			}
			if lte, ok := rng["lte"].(float64); ok && v > lte {
				return false
			}
		}
	}
	return true
}

func payloadValues(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, len(arr))
		for i, x := range arr {
			out[i] = fmt.Sprint(x)
		}
		return out
	}
	if s, ok := v.([]string); ok {
		return s
	}
	if v == nil {
		return nil
	}
	return []string{fmt.Sprint(v)}
}

func contains(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

// newSearchRAG is a real rag.Service over q.
func newSearchRAG(url string) *rag.Service {
	return rag.NewService(wordEmbedder{}, rag.NewQdrantStore(url, "c", ""), 200, 0)
}

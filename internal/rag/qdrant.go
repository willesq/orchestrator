package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/willesq/orchestrator/internal/bm25"
)

// errCollectionMissing distinguishes "the collection has not been created yet"
// from a real failure. A store nobody has ingested into is empty, not broken,
// so read paths report it as such instead of surfacing a raw 404.
var errCollectionMissing = errors.New("qdrant: collection does not exist")

// errCollectionExists is a create that lost a race with another writer.
var errCollectionExists = errors.New("qdrant collection already exists")

// QdrantStore is a minimal HTTP client for the Qdrant REST API — enough for the
// orchestrator's ingest/search needs without pulling the full gRPC client.
type QdrantStore struct {
	baseURL    string
	collection string
	apiKey     string
	http       *http.Client
	// dials counts the TCP connections the client opened, so a long job can
	// report whether it is reusing them (issue #98).
	dials *atomic.Int64

	// layout caches the collection's vector layout once it is known.
	mu     sync.Mutex
	layout Layout
}

// Layout is how a collection stores its vectors.
type Layout int

const (
	// LayoutUnknown means the collection has not been inspected (or is missing).
	LayoutUnknown Layout = iota
	// LayoutDense is the original layout: one unnamed dense vector.
	LayoutDense
	// LayoutHybrid holds a named dense vector and a named BM25 sparse vector,
	// whose inverse document frequency Qdrant applies at query time.
	LayoutHybrid
)

// Vector names in a hybrid collection.
const (
	denseName  = "dense"
	sparseName = "bm25"
)

func (l Layout) String() string {
	switch l {
	case LayoutDense:
		return "dense"
	case LayoutHybrid:
		return "hybrid"
	}
	return "unknown"
}

// NewQdrantStore constructs a store. apiKey may be empty for an unauthenticated
// local instance.
func NewQdrantStore(baseURL, collection, apiKey string) *QdrantStore {
	dials := &atomic.Int64{}
	return &QdrantStore{
		baseURL:    baseURL,
		collection: collection,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 30 * time.Second, Transport: qdrantTransport(dials)},
		dials:      dials,
	}
}

// qdrantTransport keeps enough idle connections to the one Qdrant host for
// concurrent ingests to reuse them, and counts every dial in dials. The
// default keeps two per host, so a burst past that closes the extras, each
// leaving a port in TIME_WAIT.
func qdrantTransport(dials *atomic.Int64) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, addr)
	}
	return t
}

// Dials is how many TCP connections the store has opened to Qdrant.
func (q *QdrantStore) Dials() int64 {
	if q.dials == nil {
		return 0
	}
	return q.dials.Load()
}

// maxDrain bounds how much of a response body is read just to reuse its
// connection; past it, closing the connection is cheaper.
const maxDrain = 4 << 20

// dialRetries is how many times a request is retried when the dial fails
// for want of a local port, with backoff starting at dialBackoff and capped
// at dialBackoffMax. Together they wait out more than macOS keeps a closed
// connection in TIME_WAIT (about 30 s), so one long run finishes rather than
// stopping for a rerun.
var (
	dialRetries    = 8
	dialBackoff    = 250 * time.Millisecond
	dialBackoffMax = 10 * time.Second
)

// portExhausted reports a dial that failed for want of a local port
// (EADDRNOTAVAIL, "can't assign requested address"). Nothing reached Qdrant,
// so the request is safe to retry whatever its method.
func portExhausted(err error) bool { return errors.Is(err, syscall.EADDRNOTAVAIL) }

func (q *QdrantStore) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	resp, err := q.send(ctx, method, path, payload)
	if err != nil {
		return err
	}
	// Reason: the transport reuses a connection only once its body is read
	// to EOF. Closing it unread (no out, or a decoder stopping before a
	// trailing newline) opened a new connection per request, and a long
	// re-index ran the machine out of ephemeral ports (issue #98).
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		if resp.StatusCode == http.StatusNotFound && strings.Contains(buf.String(), "doesn't exist") {
			return errCollectionMissing
		}
		if resp.StatusCode == http.StatusConflict && strings.Contains(buf.String(), "already exists") {
			return fmt.Errorf("%w: qdrant %s %s: %s", errCollectionExists, method, path, buf.String())
		}
		return fmt.Errorf("qdrant %s %s: %s: %s", method, path, resp.Status, buf.String())
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// send issues one request, retrying with backoff while the dial fails for
// want of a local port.
func (q *QdrantStore) send(ctx context.Context, method, path string, payload []byte) (*http.Response, error) {
	delay := dialBackoff
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if q.apiKey != "" {
			req.Header.Set("api-key", q.apiKey)
		}
		resp, err := q.http.Do(req)
		if err == nil || !portExhausted(err) || attempt >= dialRetries {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, dialBackoffMax)
	}
}

// Collection names the collection the store reads and writes.
func (q *QdrantStore) Collection() string { return q.collection }

// WithCollection returns a store for another collection on the same server.
func (q *QdrantStore) WithCollection(name string) *QdrantStore {
	return &QdrantStore{baseURL: q.baseURL, collection: name, apiKey: q.apiKey, http: q.http, dials: q.dials}
}

type collectionInfo struct {
	Result struct {
		Config struct {
			Params struct {
				Vectors       json.RawMessage            `json:"vectors"`
				SparseVectors map[string]json.RawMessage `json:"sparse_vectors"`
			} `json:"params"`
		} `json:"config"`
	} `json:"result"`
}

// Layout reports the collection's vector layout, or LayoutUnknown when it
// does not exist yet. A known layout is cached: it never changes in place.
func (q *QdrantStore) Layout(ctx context.Context) (Layout, error) {
	q.mu.Lock()
	known := q.layout
	q.mu.Unlock()
	if known != LayoutUnknown {
		return known, nil
	}
	var info collectionInfo
	err := q.do(ctx, http.MethodGet, q.path(""), nil, &info)
	if errors.Is(err, errCollectionMissing) {
		return LayoutUnknown, nil
	}
	if err != nil {
		return LayoutUnknown, err
	}
	l := LayoutDense
	var named map[string]json.RawMessage
	if json.Unmarshal(info.Result.Config.Params.Vectors, &named) == nil {
		if _, ok := named[denseName]; ok {
			if _, ok := info.Result.Config.Params.SparseVectors[sparseName]; ok {
				l = LayoutHybrid
			}
		}
	}
	q.mu.Lock()
	q.layout = l
	q.mu.Unlock()
	return l, nil
}

// EnsureCollection creates the collection if it does not already exist. A new
// collection is hybrid; an existing one is left as it is.
func (q *QdrantStore) EnsureCollection(ctx context.Context, dim int) error {
	l, err := q.Layout(ctx)
	if err != nil || l != LayoutUnknown {
		return err
	}
	body := map[string]any{
		"vectors":        map[string]any{denseName: map[string]any{"size": dim, "distance": "Cosine"}},
		"sparse_vectors": map[string]any{sparseName: map[string]any{"modifier": "idf"}},
	}
	if err := q.do(ctx, http.MethodPut, q.path(""), body, nil); err != nil {
		if errors.Is(err, errCollectionExists) {
			// Another writer created it first, such as two first ingests into a
			// new store at once; use the collection it made.
			_, err = q.Layout(ctx)
		}
		return err
	}
	// Reads and deletes filter by source, prunes by writer and searches by tag and
	// type, so index them all.
	for _, field := range append([]string{"source_uri", fieldWriter, fieldSpace}, searchIndexFields...) {
		if err := q.EnsureIndex(ctx, field); err != nil {
			return err
		}
	}
	q.mu.Lock()
	q.layout = LayoutHybrid
	q.mu.Unlock()
	return nil
}

// EnsureIndex adds a keyword payload index on field; adding one that exists
// changes nothing.
func (q *QdrantStore) EnsureIndex(ctx context.Context, field string) error {
	return q.do(ctx, http.MethodPut, q.path("/index")+"?wait=true",
		map[string]any{"field_name": field, "field_schema": "keyword"}, nil)
}

// startIndex asks for a keyword payload index and returns without waiting for it
// to be built, which on a large store takes longer than a request may.
func (q *QdrantStore) startIndex(ctx context.Context, field string) error {
	return q.do(ctx, http.MethodPut, q.path("/index"),
		map[string]any{"field_name": field, "field_schema": "keyword"}, nil)
}

// Point is a single chunk to upsert: its dense vector, its BM25 sparse vector
// (used only by a hybrid collection), and its payload.
type Point struct {
	ID      string
	Vector  []float32
	Sparse  bm25.Vector
	Payload map[string]any
}

// upsertBatch is how many points one write request carries. A large source in
// a single request could pass Qdrant's 32 MiB request-body limit.
const upsertBatch = 256

// PartialWriteError reports an upsert that failed after some of its batches
// were written: the collection holds the first Written points of the new
// version alongside whatever the rest replaced.
type PartialWriteError struct {
	Written, Total int
	Err            error
}

func (e *PartialWriteError) Error() string {
	return fmt.Sprintf("wrote %d of %d points before failing: %v", e.Written, e.Total, e.Err)
}

func (e *PartialWriteError) Unwrap() error { return e.Err }

// Upsert writes points to the collection (create-or-replace by id), in the
// collection's own layout, upsertBatch points per request. A failure after
// the first batch is a *PartialWriteError.
func (q *QdrantStore) Upsert(ctx context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	l, err := q.Layout(ctx)
	if err != nil {
		return err
	}
	for start := 0; start < len(points); start += upsertBatch {
		if err := q.upsertLayout(ctx, l, points[start:min(start+upsertBatch, len(points))]); err != nil {
			if start > 0 {
				return &PartialWriteError{Written: start, Total: len(points), Err: err}
			}
			return err
		}
	}
	return nil
}

func (q *QdrantStore) upsertLayout(ctx context.Context, l Layout, points []Point) error {
	wire := make([]map[string]any, len(points))
	for i, p := range points {
		var vec any = p.Vector
		if l == LayoutHybrid {
			named := map[string]any{denseName: p.Vector}
			// An empty sparse vector is left out: the point is still found by
			// meaning, just not by keyword.
			if !p.Sparse.IsEmpty() {
				named[sparseName] = p.Sparse
			}
			vec = named
		}
		wire[i] = map[string]any{"id": p.ID, "vector": vec, "payload": p.Payload}
	}
	err := q.do(ctx, http.MethodPut,
		q.path("/points")+"?wait=true", map[string]any{"points": wire}, nil)
	q.forgetIfMissing(err)
	return err
}

// forgetIfMissing drops the cached layout when the collection has gone, so
// the next write recreates it rather than failing until a restart.
func (q *QdrantStore) forgetIfMissing(err error) {
	if errors.Is(err, errCollectionMissing) {
		q.mu.Lock()
		q.layout = LayoutUnknown
		q.mu.Unlock()
	}
}

// SearchHit is a single search result.
type SearchHit struct {
	ID      any            `json:"id"`
	Score   float32        `json:"score"`
	Payload map[string]any `json:"payload"`
	// Vector is set by KeywordSearch: the hit's named vectors, so a caller can
	// score a keyword-only hit by meaning too.
	Vector map[string]json.RawMessage `json:"vector,omitempty"`
}

// PointID is the hit's id as a string.
func (h SearchHit) PointID() string { return fmt.Sprint(h.ID) }

type queryResponse struct {
	Result struct {
		Points []SearchHit `json:"points"`
	} `json:"result"`
}

// KeywordSearch ranks a hybrid collection by BM25: the query's terms against
// each chunk's sparse vector, with Qdrant applying IDF. Hits carry their dense
// vector. An empty query vector matches nothing.
func (q *QdrantStore) KeywordSearch(ctx context.Context, query bm25.Vector, topK int, must map[string]string, anyTags, allTags []string, tr TimeRange) ([]SearchHit, error) {
	return q.keywordSearch(ctx, query, topK, must, anyTags, allTags, tr, true)
}

// keywordSearch is KeywordSearch, returning each hit's dense vector only when
// vectors is set: the vectors are most of the request's cost.
func (q *QdrantStore) keywordSearch(ctx context.Context, query bm25.Vector, topK int, must map[string]string, anyTags, allTags []string, tr TimeRange, vectors bool) ([]SearchHit, error) {
	if query.IsEmpty() {
		return nil, nil
	}
	body := map[string]any{
		"query":        query,
		"using":        sparseName,
		"limit":        topK,
		"with_payload": true,
		"with_vector":  false,
	}
	if vectors {
		body["with_vector"] = []string{denseName}
	}
	if filter := buildFilter(must, anyTags, allTags, tr); filter != nil {
		body["filter"] = filter
	}
	var resp queryResponse
	if err := q.do(ctx, http.MethodPost, q.path("/points/query"), body, &resp); err != nil {
		q.forgetIfMissing(err)
		return nil, err
	}
	return resp.Result.Points, nil
}

type vectorsResponse struct {
	Result []struct {
		ID     any                        `json:"id"`
		Vector map[string]json.RawMessage `json:"vector"`
	} `json:"result"`
}

// DenseVectors returns the dense vectors of the points with the given ids,
// by id; a point that does not exist is left out.
func (q *QdrantStore) DenseVectors(ctx context.Context, ids []string) (map[string]json.RawMessage, error) {
	var resp vectorsResponse
	if err := q.do(ctx, http.MethodPost, q.path("/points"),
		map[string]any{"ids": ids, "with_payload": false, "with_vector": []string{denseName}}, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(resp.Result))
	for _, p := range resp.Result {
		out[fmt.Sprint(p.ID)] = p.Vector[denseName]
	}
	return out, nil
}

type searchResponse struct {
	Result []SearchHit `json:"result"`
}

// Search runs a vector query, optionally filtered by exact-match payload fields
// in must (all must match), anyTags (payload.tags contains any), allTags
// (payload.tags contains every one) and tr.
func (q *QdrantStore) Search(ctx context.Context, vector []float32, topK int, must map[string]string, anyTags, allTags []string, tr TimeRange) ([]SearchHit, error) {
	filter := buildFilter(must, anyTags, allTags, tr)
	l, err := q.Layout(ctx)
	if err != nil {
		return nil, err
	}
	var vec any = vector
	if l == LayoutHybrid {
		vec = map[string]any{"name": denseName, "vector": vector}
	}
	body := map[string]any{
		"vector":       vec,
		"limit":        topK,
		"with_payload": true,
	}
	if filter != nil {
		body["filter"] = filter
	}
	var resp searchResponse
	if err := q.do(ctx, http.MethodPost,
		q.path("/points/search"), body, &resp); err != nil {
		q.forgetIfMissing(err)
		return nil, err
	}
	return resp.Result, nil
}

// ScrollPoint is one point returned by a payload scroll (no vector).
type ScrollPoint struct {
	ID      string         `json:"id"`
	Payload map[string]any `json:"payload"`
}

type scrollResponse struct {
	Result struct {
		Points         []ScrollPoint `json:"points"`
		NextPageOffset any           `json:"next_page_offset"`
	} `json:"result"`
}

// Scroll pages through the collection returning payloads only. offset is nil on
// the first call and thereafter the previous call's next value; a nil next means
// the scan is complete. fields limits which payload keys come back.
func (q *QdrantStore) Scroll(ctx context.Context, filter map[string]any, fields []string, pageSize int, offset any) ([]ScrollPoint, any, error) {
	if pageSize <= 0 {
		pageSize = 1000
	}
	body := map[string]any{"limit": pageSize, "with_vector": false}
	if len(fields) > 0 {
		body["with_payload"] = fields
	} else {
		body["with_payload"] = true
	}
	if filter != nil {
		body["filter"] = filter
	}
	if offset != nil {
		body["offset"] = offset
	}
	var resp scrollResponse
	if err := q.do(ctx, http.MethodPost,
		q.path("/points/scroll"), body, &resp); err != nil {
		if errors.Is(err, errCollectionMissing) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return resp.Result.Points, resp.Result.NextPageOffset, nil
}

type countResponse struct {
	Result struct {
		Count int `json:"count"`
	} `json:"result"`
}

// Count returns how many points match the filter. An exact count is requested so
// callers can report a trustworthy number rather than an estimate.
func (q *QdrantStore) Count(ctx context.Context, filter map[string]any) (int, error) {
	body := map[string]any{"exact": true}
	if filter != nil {
		body["filter"] = filter
	}
	var resp countResponse
	if err := q.do(ctx, http.MethodPost,
		q.path("/points/count"), body, &resp); err != nil {
		if errors.Is(err, errCollectionMissing) {
			return 0, nil
		}
		return 0, err
	}
	return resp.Result.Count, nil
}

// DeleteCollection removes the whole collection and every point in it.
func (q *QdrantStore) DeleteCollection(ctx context.Context) error {
	err := q.do(ctx, http.MethodDelete, q.path(""), nil, nil)
	q.mu.Lock()
	q.layout = LayoutUnknown
	q.mu.Unlock()
	return err
}

// DeleteByFilter removes every point matching the filter.
//
// An unconstrained filter is rejected. Checking for nil alone is not enough:
// Qdrant reads both `{}` and `{"must": []}` as match-everything, so a helper
// that builds an empty condition list would silently wipe the collection. The
// guard therefore asserts the property that matters — at least one condition.
func (q *QdrantStore) DeleteByFilter(ctx context.Context, filter map[string]any) error {
	must, ok := filter["must"].([]any)
	if !ok || len(must) == 0 {
		return fmt.Errorf("qdrant: refusing to delete without at least one must condition")
	}
	return q.do(ctx, http.MethodPost,
		q.path("/points/delete")+"?wait=true",
		map[string]any{"filter": filter}, nil)
}

// path builds a collection-scoped URL path with the collection name escaped.
// It is config-sourced rather than caller-supplied, but a name containing ? or
// # would otherwise restructure the request — including on the delete endpoint.
func (q *QdrantStore) path(suffix string) string {
	return "/collections/" + url.PathEscape(q.collection) + suffix
}

// EqFilter builds a single exact-match filter on one payload key.
func EqFilter(key, value string) map[string]any {
	return map[string]any{"must": []any{
		map[string]any{"key": key, "match": map[string]any{"value": value}},
	}}
}

func buildFilter(must map[string]string, anyTags, allTags []string, tr TimeRange) map[string]any {
	var conds []any
	for k, val := range must {
		if val == "" {
			continue
		}
		var v any = val
		if k == fieldLabelsV {
			// Reason: labels_v is stored as a number, which a string never
			// matches; an unparsable value matches nothing rather than
			// dropping the condition.
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				n = -1
			}
			v = n
		}
		conds = append(conds, map[string]any{
			"key":   k,
			"match": map[string]any{"value": v},
		})
	}
	if len(anyTags) > 0 {
		conds = append(conds, map[string]any{
			"key":   "tags",
			"match": map[string]any{"any": anyTags},
		})
	}
	for _, tag := range allTags {
		if tag == "" {
			continue
		}
		conds = append(conds, map[string]any{
			"key":   "tags",
			"match": map[string]any{"value": tag},
		})
	}
	conds = append(conds, tr.conditions()...)
	if len(conds) == 0 {
		return nil
	}
	return map[string]any{"must": conds}
}

package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errCollectionMissing distinguishes "the collection has not been created yet"
// from a real failure. A store nobody has ingested into is empty, not broken,
// so read paths report it as such instead of surfacing a raw 404.
var errCollectionMissing = errors.New("qdrant: collection does not exist")

// QdrantStore is a minimal HTTP client for the Qdrant REST API — enough for the
// orchestrator's ingest/search needs without pulling the full gRPC client.
type QdrantStore struct {
	baseURL    string
	collection string
	apiKey     string
	http       *http.Client
}

// NewQdrantStore constructs a store. apiKey may be empty for an unauthenticated
// local instance.
func NewQdrantStore(baseURL, collection, apiKey string) *QdrantStore {
	return &QdrantStore{
		baseURL:    baseURL,
		collection: collection,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (q *QdrantStore) do(ctx context.Context, method, path string, body, out any) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}
	resp, err := q.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		if resp.StatusCode == http.StatusNotFound && strings.Contains(buf.String(), "doesn't exist") {
			return errCollectionMissing
		}
		return fmt.Errorf("qdrant %s %s: %s: %s", method, path, resp.Status, buf.String())
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// EnsureCollection creates the collection with the given vector size (Cosine
// distance) if it does not already exist.
func (q *QdrantStore) EnsureCollection(ctx context.Context, dim int) error {
	err := q.do(ctx, http.MethodGet, q.path(""), nil, nil)
	if err == nil {
		return nil // exists
	}
	body := map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}
	return q.do(ctx, http.MethodPut, q.path(""), body, nil)
}

// Point is a single vector + payload to upsert.
type Point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// Upsert writes points to the collection (create-or-replace by id).
func (q *QdrantStore) Upsert(ctx context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	body := map[string]any{"points": points}
	return q.do(ctx, http.MethodPut,
		q.path("/points")+"?wait=true", body, nil)
}

// SearchHit is a single search result.
type SearchHit struct {
	Score   float32        `json:"score"`
	Payload map[string]any `json:"payload"`
}

type searchResponse struct {
	Result []SearchHit `json:"result"`
}

// Search runs a vector query, optionally filtered by exact-match payload fields
// in must (all must match) and anyTags (payload.tags contains any).
func (q *QdrantStore) Search(ctx context.Context, vector []float32, topK int, must map[string]string, anyTags []string) ([]SearchHit, error) {
	filter := buildFilter(must, anyTags)
	body := map[string]any{
		"vector":       vector,
		"limit":        topK,
		"with_payload": true,
	}
	if filter != nil {
		body["filter"] = filter
	}
	var resp searchResponse
	if err := q.do(ctx, http.MethodPost,
		q.path("/points/search"), body, &resp); err != nil {
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

func buildFilter(must map[string]string, anyTags []string) map[string]any {
	var conds []any
	for k, val := range must {
		if val == "" {
			continue
		}
		conds = append(conds, map[string]any{
			"key":   k,
			"match": map[string]any{"value": val},
		})
	}
	if len(anyTags) > 0 {
		conds = append(conds, map[string]any{
			"key":   "tags",
			"match": map[string]any{"any": anyTags},
		})
	}
	if len(conds) == 0 {
		return nil
	}
	return map[string]any{"must": conds}
}

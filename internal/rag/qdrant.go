package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

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
	err := q.do(ctx, http.MethodGet, "/collections/"+q.collection, nil, nil)
	if err == nil {
		return nil // exists
	}
	body := map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}
	return q.do(ctx, http.MethodPut, "/collections/"+q.collection, body, nil)
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
		"/collections/"+q.collection+"/points?wait=true", body, nil)
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
		"/collections/"+q.collection+"/points/search", body, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
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

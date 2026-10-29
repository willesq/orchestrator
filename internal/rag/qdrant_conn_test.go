package rag

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestQdrantReusesConnections is the regression test for issue #98: many
// sequential requests must share one keep-alive connection, whether or not
// the caller decodes the body, and when the JSON has a trailing newline.
func TestQdrantReusesConnections(t *testing.T) {
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reason: streamed and large. Go's transport drains a small unread
		// remainder itself on Close (measured: up to ~256 KiB), so only a
		// body past that, closed unread, used to cost a connection.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"ok":true},"status":"ok","pad":"`))
		w.(http.Flusher).Flush()
		_, _ = w.Write(bytes.Repeat([]byte("x"), 512<<10))
		_, _ = w.Write([]byte(`"}` + "\n\n"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	q := NewQdrantStore(srv.URL, "c", "")
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		var out struct {
			Result struct{ OK bool } `json:"result"`
		}
		if err := q.do(ctx, http.MethodPost, "/collections/c/points", map[string]int{"i": i}, &out); err != nil || !out.Result.OK {
			t.Fatalf("decoded request %d: %v %+v", i, err, out)
		}
		if err := q.do(ctx, http.MethodPut, "/collections/c/points/payload", nil, nil); err != nil {
			t.Fatalf("undecoded request %d: %v", i, err)
		}
	}
	if n := opened.Load(); n > 2 {
		t.Errorf("100 sequential requests opened %d connections; want them to reuse one", n)
	}
	if q.Dials() != opened.Load() {
		t.Errorf("Dials() = %d, server saw %d new connections", q.Dials(), opened.Load())
	}
	if (&QdrantStore{}).Dials() != 0 {
		t.Error("a store built without NewQdrantStore should report no dials")
	}
}

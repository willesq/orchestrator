package rag

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestQdrantRetriesPortExhaustion: a dial that fails for want of a local
// port (issue #98) is retried with backoff; other failures are not.
func TestQdrantRetriesPortExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	saved, savedBackoff, savedMax := dialRetries, dialBackoff, dialBackoffMax
	defer func() { dialRetries, dialBackoff, dialBackoffMax = saved, savedBackoff, savedMax }()
	dialRetries, dialBackoff, dialBackoffMax = 3, time.Millisecond, 2*time.Millisecond

	var d net.Dialer
	var calls, left atomic.Int64
	tr := qdrantTransport(&atomic.Int64{})
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls.Add(1)
		if left.Add(-1) >= 0 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.EADDRNOTAVAIL}
		}
		return d.DialContext(ctx, network, addr)
	}
	q := NewQdrantStore(srv.URL, "c", "")
	q.http.Transport = tr
	ctx := context.Background()

	left.Store(2)
	if err := q.do(ctx, http.MethodPost, "/x", map[string]int{"a": 1}, nil); err != nil {
		t.Fatalf("two exhausted dials then success should succeed: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("dials = %d, want 3", calls.Load())
	}

	tr.CloseIdleConnections()
	calls.Store(0)
	left.Store(10)
	err := q.do(ctx, http.MethodPost, "/x", nil, nil)
	if !errors.Is(err, syscall.EADDRNOTAVAIL) || calls.Load() != 4 {
		t.Errorf("persistent exhaustion: err %v after %d dials; want EADDRNOTAVAIL after 1+3", err, calls.Load())
	}

	// Any other failure is not retried.
	tr.CloseIdleConnections()
	calls.Store(0)
	left.Store(0)
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	}
	if err := q.do(ctx, http.MethodGet, "/x", nil, nil); err == nil || calls.Load() != 1 {
		t.Errorf("a refused connection was retried: %v after %d dials", err, calls.Load())
	}

	// A cancelled context stops the backoff.
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: syscall.EADDRNOTAVAIL}
	}
	dialBackoff, dialBackoffMax = time.Hour, time.Hour
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := q.do(cctx, http.MethodGet, "/x", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled backoff = %v", err)
	}
}

// Package embed provides the in-process sentence-embedding model. It wraps hugot's
// ONNX-Runtime feature-extraction pipeline running all-MiniLM-L6-v2 (384-dim,
// mean-pooled, L2-normalized), verified bit-identical to Python sentence-transformers.
//
// Build requirements (ORT backend):
//   - build tag: ORT ; CGO_ENABLED=1
//   - CGO_LDFLAGS=-L<dir containing libtokenizers.a>
//   - at runtime: the onnxruntime shared library directory (Homebrew opt path)
//   - a model directory containing model.onnx + tokenizer.json
//
// The model is loaded lazily on first Embed and unloaded after an idle timeout so
// a mostly-idle orchd reclaims the RAM. Only orchd loads it; CLIs/hooks are clients.
//
// The ONNX-specific loading lives behind the loader func (embedder_ort.go /
// embedder_noort.go) so the lifecycle logic here is testable without the runtime.
package embed

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Config controls the embedder.
type Config struct {
	ModelDir    string        // dir with model.onnx + tokenizer.json
	OnnxLibDir  string        // dir containing libonnxruntime.dylib
	IdleUnload  time.Duration // unload the model after this much inactivity (0 = never)
	Dimension   int           // expected vector dimension (0 = infer)
	ReaperEvery time.Duration // idle check cadence (default 30s)
}

// runner performs inference against a loaded model; destroy releases native resources.
type runner interface {
	embed(texts []string) ([][]float32, error)
	destroy()
}

// loaderFunc loads a model and returns a runner plus its vector dimension.
// The default is provided per build tag; tests substitute a fake.
type loaderFunc func(cfg Config) (runner, int, error)

// Embedder is a lazily-loaded, idle-unloading embedding model. Safe for concurrent use.
type Embedder struct {
	cfg  Config
	load loaderFunc

	mu       sync.Mutex
	run      runner
	lastUsed time.Time
	dim      int

	stop chan struct{}
	once sync.Once

	// tok counts tokens for chunking. It is loaded on first use and kept: it
	// is small, and unlike the model it is needed on every ingest. A failed
	// load is retried after tokRetry, so one bad read does not leave chunking
	// on the estimate until restart.
	tokMu    sync.Mutex
	tok      tokenCounter
	tokTried time.Time
}

// tokRetry spaces out attempts to load a tokenizer that failed to load.
const tokRetry = time.Minute

// tokenCounter counts a text's tokens, excluding special tokens.
type tokenCounter func(text string) int

// CountTokens counts text's model tokens, excluding the special tokens added
// around every input. ok is false when no tokenizer is available (a build
// without -tags ORT, or no tokenizer.json), and callers then estimate.
func (e *Embedder) CountTokens(text string) (n int, ok bool) {
	e.tokMu.Lock()
	if e.tok == nil && (e.tokTried.IsZero() || time.Since(e.tokTried) >= tokRetry) {
		e.tokTried = time.Now()
		e.tok = loadTokenCounter(e.cfg.ModelDir)
	}
	tok := e.tok
	e.tokMu.Unlock()
	if tok == nil {
		return 0, false
	}
	return tok(text), true
}

// New constructs an Embedder. It does not load the model until the first Embed.
func New(cfg Config) *Embedder {
	return newWithLoader(cfg, defaultLoader)
}

func newWithLoader(cfg Config, load loaderFunc) *Embedder {
	if cfg.ReaperEvery == 0 {
		cfg.ReaperEvery = 30 * time.Second
	}
	e := &Embedder{cfg: cfg, load: load, dim: cfg.Dimension, stop: make(chan struct{})}
	if cfg.IdleUnload > 0 {
		go e.reaper()
	}
	return e
}

// Dimension returns the vector dimension, loading the model if necessary.
func (e *Embedder) Dimension(_ context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dim > 0 {
		return e.dim, nil
	}
	if err := e.ensureLoadedLocked(); err != nil {
		return 0, err
	}
	return e.dim, nil
}

// Embed returns one L2-normalized vector per input text. The caller's ctx gates
// waiting for the lock, but the ONNX session and inference use the model's own
// lifetime — a per-request context that gets canceled must never poison the
// long-lived session.
func (e *Embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureLoadedLocked(); err != nil {
		return nil, err
	}
	vecs, err := e.run.embed(texts)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	e.lastUsed = time.Now()
	return vecs, nil
}

func (e *Embedder) ensureLoadedLocked() error {
	if e.run != nil {
		return nil
	}
	r, dim, err := e.load(e.cfg)
	if err != nil {
		return err
	}
	e.run = r
	if e.dim == 0 {
		e.dim = dim
	}
	e.lastUsed = time.Now()
	return nil
}

func (e *Embedder) reaper() {
	t := time.NewTicker(e.cfg.ReaperEvery)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.mu.Lock()
			if e.run != nil && time.Since(e.lastUsed) > e.cfg.IdleUnload {
				e.unloadLocked()
			}
			e.mu.Unlock()
		}
	}
}

func (e *Embedder) unloadLocked() {
	if e.run != nil {
		e.run.destroy()
	}
	e.run = nil
}

// Close unloads the model and stops the reaper.
func (e *Embedder) Close() {
	e.once.Do(func() { close(e.stop) })
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unloadLocked()
}

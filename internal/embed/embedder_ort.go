//go:build ORT

package embed

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/backends"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"
)

// hugotRunner is the real ONNX-Runtime-backed runner.
// The onnxruntime environment is process-global and cannot be re-created: a
// second ort.InitializeEnvironment after a DestroyEnvironment fails with
// `Trying to add a domain to DomainToVersion map, but the domain is already
// exist ... domain: "com.microsoft.experimental"`, which kills every embed call
// (and so all of RAG) until the process restarts. hugot.Session.Destroy tears
// that environment down, so the session is created exactly once per process and
// never destroyed; idle-unload closes the pipeline instead, which is what frees
// the model weights.
var (
	ortOnce    sync.Once
	ortShared  *hugot.Session
	ortInitErr error

	// Pipelines on a shared session are keyed by name, so each load takes a fresh
	// one; a reload can then register before the previous pipeline is closed.
	ortPipelineSeq atomic.Uint64
)

// ortSession returns the process-wide hugot session, creating it on first use.
func ortSession(onnxLibDir string) (*hugot.Session, error) {
	ortOnce.Do(func() {
		var opts []options.WithOption
		if onnxLibDir != "" {
			opts = append(opts, options.WithOnnxLibraryPath(onnxLibDir))
		}
		ortShared, ortInitErr = hugot.NewORTSession(context.Background(), opts...)
		if ortInitErr != nil {
			ortInitErr = fmt.Errorf("new ORT session: %w", ortInitErr)
		}
	})
	return ortShared, ortInitErr
}

type hugotRunner struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
	name     string
}

func (h *hugotRunner) embed(texts []string) ([][]float32, error) {
	out, err := h.pipeline.RunPipeline(context.Background(), texts)
	if err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}

// destroy frees the model weights but deliberately leaves the shared ORT session
// (and with it the onnxruntime environment) alive — see ortSession.
func (h *hugotRunner) destroy() {
	if h.session == nil {
		return
	}
	_ = hugot.ClosePipeline[*pipelines.FeatureExtractionPipeline](h.session, h.name)
	h.pipeline = nil
}

// defaultLoader loads all-MiniLM-L6-v2 via hugot's ORT feature-extraction pipeline.
// The session uses a background context so it outlives any single request.
func defaultLoader(cfg Config) (runner, int, error) {
	session, err := ortSession(cfg.OnnxLibDir)
	if err != nil {
		return nil, 0, err
	}
	name := fmt.Sprintf("minilm-%d", ortPipelineSeq.Add(1))
	pc := hugot.FeatureExtractionConfig{
		ModelPath:    cfg.ModelDir,
		Name:         name,
		OnnxFilename: "model.onnx",
		Options: []backends.PipelineOption[*pipelines.FeatureExtractionPipeline]{
			pipelines.WithNormalization(),
		},
	}
	p, err := hugot.NewPipeline(session, pc)
	if err != nil {
		return nil, 0, fmt.Errorf("new feature-extraction pipeline: %w", err)
	}
	dim := cfg.Dimension
	if dim == 0 {
		if probe, perr := p.RunPipeline(context.Background(), []string{"x"}); perr == nil && len(probe.Embeddings) > 0 {
			dim = len(probe.Embeddings[0])
		}
	}
	return &hugotRunner{session: session, pipeline: p, name: name}, dim, nil
}

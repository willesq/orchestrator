//go:build ORT

package embed

import (
	"context"
	"fmt"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/backends"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"
)

// hugotRunner is the real ONNX-Runtime-backed runner.
type hugotRunner struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

func (h *hugotRunner) embed(texts []string) ([][]float32, error) {
	out, err := h.pipeline.RunPipeline(context.Background(), texts)
	if err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}

func (h *hugotRunner) destroy() {
	if h.session != nil {
		_ = h.session.Destroy()
	}
}

// defaultLoader loads all-MiniLM-L6-v2 via hugot's ORT feature-extraction pipeline.
// The session uses a background context so it outlives any single request.
func defaultLoader(cfg Config) (runner, int, error) {
	var opts []options.WithOption
	if cfg.OnnxLibDir != "" {
		opts = append(opts, options.WithOnnxLibraryPath(cfg.OnnxLibDir))
	}
	session, err := hugot.NewORTSession(context.Background(), opts...)
	if err != nil {
		return nil, 0, fmt.Errorf("new ORT session: %w", err)
	}
	pc := hugot.FeatureExtractionConfig{
		ModelPath:    cfg.ModelDir,
		Name:         "minilm",
		OnnxFilename: "model.onnx",
		Options: []backends.PipelineOption[*pipelines.FeatureExtractionPipeline]{
			pipelines.WithNormalization(),
		},
	}
	p, err := hugot.NewPipeline(session, pc)
	if err != nil {
		_ = session.Destroy()
		return nil, 0, fmt.Errorf("new feature-extraction pipeline: %w", err)
	}
	dim := cfg.Dimension
	if dim == 0 {
		if probe, perr := p.RunPipeline(context.Background(), []string{"x"}); perr == nil && len(probe.Embeddings) > 0 {
			dim = len(probe.Embeddings[0])
		}
	}
	return &hugotRunner{session: session, pipeline: p}, dim, nil
}

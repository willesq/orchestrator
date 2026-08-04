// Package model provisions the embedding model files (model.onnx + tokenizer.json)
// into the model directory, downloading them on first install. The ONNX weights
// (~90MB) are downloaded rather than embedded to keep the binary and git lean.
package model

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Default source URLs for all-MiniLM-L6-v2 (ONNX export + HF tokenizer).
const (
	DefaultModelURL     = "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/onnx/model.onnx"
	DefaultTokenizerURL = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/tokenizer.json"
)

// Config controls provisioning.
type Config struct {
	Dir          string
	ModelURL     string
	TokenizerURL string
}

type downloadFunc func(ctx context.Context, url, dest string) error

// Provisioner ensures the model files exist in Dir.
type Provisioner struct {
	cfg      Config
	download downloadFunc
}

// New builds a provisioner with the real HTTP download, overridable in tests.
func New(cfg Config) *Provisioner {
	if cfg.ModelURL == "" {
		cfg.ModelURL = DefaultModelURL
	}
	if cfg.TokenizerURL == "" {
		cfg.TokenizerURL = DefaultTokenizerURL
	}
	return &Provisioner{cfg: cfg, download: httpDownload}
}

// ModelPath / TokenizerPath are where the files live.
func (p *Provisioner) ModelPath() string     { return filepath.Join(p.cfg.Dir, "model.onnx") }
func (p *Provisioner) TokenizerPath() string { return filepath.Join(p.cfg.Dir, "tokenizer.json") }

// Present reports whether both files already exist.
func (p *Provisioner) Present() bool {
	return exists(p.ModelPath()) && exists(p.TokenizerPath())
}

// Ensure downloads any missing model files into Dir.
func (p *Provisioner) Ensure(ctx context.Context) error {
	if err := os.MkdirAll(p.cfg.Dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct{ url, dest string }{
		{p.cfg.ModelURL, p.ModelPath()},
		{p.cfg.TokenizerURL, p.TokenizerPath()},
	} {
		if exists(f.dest) {
			continue
		}
		if err := p.download(ctx, f.url, f.dest); err != nil {
			return fmt.Errorf("download %s: %w", filepath.Base(f.dest), err)
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func httpDownload(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, resp.Body)
	return err
}

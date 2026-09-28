//go:build ORT

package embed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ortTestConfig resolves the real model + onnxruntime locations, skipping when
// this machine has no model downloaded (plain checkouts, CI without the model).
func ortTestConfig(t *testing.T) Config {
	t.Helper()
	modelDir := os.Getenv("ORCH_TEST_MODEL_DIR")
	if modelDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		modelDir = filepath.Join(home, ".local", "share", "orch", "models", "minilm")
	}
	if _, err := os.Stat(filepath.Join(modelDir, "model.onnx")); err != nil {
		t.Skipf("embedding model not present at %s", modelDir)
	}
	onnxDir := os.Getenv("ORCH_TEST_ONNX_LIB_DIR")
	if onnxDir == "" {
		onnxDir = "/opt/homebrew/opt/onnxruntime/lib"
	}
	return Config{ModelDir: modelDir, OnnxLibDir: onnxDir}
}

// TestORTReloadAfterUnload is the regression test for the onnxruntime
// environment being torn down on idle-unload: the reload used to fail with
// `domain ... com.microsoft.experimental` already exists, which killed every
// embed call — and so all of RAG — until orchd was restarted.
func TestORTReloadAfterUnload(t *testing.T) {
	tests := []struct {
		name   string
		cycles int
	}{
		{name: "embed survives repeated unload/reload cycles", cycles: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ortTestConfig(t)
			for i := 0; i < tt.cycles; i++ {
				r, dim, err := defaultLoader(cfg)
				if err != nil {
					t.Fatalf("cycle %d: load: %v", i, err)
				}
				if dim == 0 {
					t.Fatalf("cycle %d: probe returned dimension 0", i)
				}
				vecs, err := r.embed([]string{"hello"})
				if err != nil {
					t.Fatalf("cycle %d: embed: %v", i, err)
				}
				if len(vecs) != 1 || len(vecs[0]) != dim {
					t.Fatalf("cycle %d: got %d vectors of width %d, want 1 of %d",
						i, len(vecs), len(vecs[0]), dim)
				}
				r.destroy()
			}
		})
	}
}

// TestORTSessionIsShared pins the invariant the fix depends on: every load hands
// back the same hugot session, so the ORT environment is initialized only once.
func TestORTSessionIsShared(t *testing.T) {
	tests := []struct{ name string }{{name: "second load reuses the session"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ortTestConfig(t)
			first, _, err := defaultLoader(cfg)
			if err != nil {
				t.Fatalf("first load: %v", err)
			}
			defer first.destroy()
			second, _, err := defaultLoader(cfg)
			if err != nil {
				t.Fatalf("second load: %v", err)
			}
			defer second.destroy()
			if first.(*hugotRunner).session != second.(*hugotRunner).session {
				t.Errorf("loads returned different ORT sessions; the environment was re-created")
			}
		})
	}
}

// TestORTEmbedAfterReaperUnload exercises the full Embedder path: the reaper
// unloads an idle model, and the next Embed must transparently reload it.
func TestORTEmbedAfterReaperUnload(t *testing.T) {
	tests := []struct{ name string }{{name: "Embed reloads after the reaper unloads"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ortTestConfig(t)
			cfg.IdleUnload = time.Millisecond
			cfg.ReaperEvery = 2 * time.Millisecond
			e := New(cfg)
			defer e.Close()

			if _, err := e.Embed(context.Background(), []string{"first"}); err != nil {
				t.Fatalf("first embed: %v", err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				e.mu.Lock()
				unloaded := e.run == nil
				e.mu.Unlock()
				if unloaded {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			e.mu.Lock()
			unloaded := e.run == nil
			e.mu.Unlock()
			if !unloaded {
				t.Fatal("reaper never unloaded the idle model")
			}
			if _, err := e.Embed(context.Background(), []string{"second"}); err != nil {
				t.Errorf("embed after unload: %v", err)
			}
		})
	}
}

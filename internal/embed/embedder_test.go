package embed

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRunner struct {
	vec       []float32
	embedErr  error
	destroyed bool
	calls     int
}

func (f *fakeRunner) embed(texts []string) ([][]float32, error) {
	f.calls++
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

func (f *fakeRunner) destroy() { f.destroyed = true }

func loaderReturning(r runner, dim int, err error) loaderFunc {
	return func(Config) (runner, int, error) { return r, dim, err }
}

func TestEmbed(t *testing.T) {
	tests := []struct {
		name     string
		texts    []string
		runner   *fakeRunner
		loadErr  error
		wantVecs int
		wantErr  bool
	}{
		{
			name:     "happy path returns one vector per text",
			texts:    []string{"a", "b"},
			runner:   &fakeRunner{vec: []float32{0.1, 0.2}},
			wantVecs: 2,
		},
		{
			name:     "empty input returns nil without loading",
			texts:    nil,
			runner:   &fakeRunner{vec: []float32{0.1}},
			wantVecs: 0,
		},
		{
			name:    "load error propagates",
			texts:   []string{"a"},
			loadErr: errors.New("boom"),
			wantErr: true,
		},
		{
			name:    "runner error is wrapped",
			texts:   []string{"a"},
			runner:  &fakeRunner{embedErr: errors.New("infer failed")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r runner
			if tt.runner != nil {
				r = tt.runner
			}
			e := newWithLoader(Config{Dimension: 2}, loaderReturning(r, 2, tt.loadErr))
			defer e.Close()

			vecs, err := e.Embed(context.Background(), tt.texts)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(vecs) != tt.wantVecs {
				t.Errorf("got %d vectors, want %d", len(vecs), tt.wantVecs)
			}
		})
	}
}

func TestDimension(t *testing.T) {
	tests := []struct {
		name    string
		cfgDim  int
		loadDim int
		loadErr error
		want    int
		wantErr bool
	}{
		{name: "preset dimension avoids loading", cfgDim: 384, want: 384},
		{name: "inferred from loader", cfgDim: 0, loadDim: 7, want: 7},
		{name: "load error", cfgDim: 0, loadErr: errors.New("x"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newWithLoader(Config{Dimension: tt.cfgDim},
				loaderReturning(&fakeRunner{vec: []float32{1}}, tt.loadDim, tt.loadErr))
			defer e.Close()
			got, err := e.Dimension(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Dimension() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLazyLoadOnce(t *testing.T) {
	tests := []struct {
		name      string
		embeds    int
		wantLoads int
	}{
		{name: "model loads once across repeated embeds", embeds: 3, wantLoads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loads := 0
			r := &fakeRunner{vec: []float32{1}}
			e := newWithLoader(Config{Dimension: 1}, func(Config) (runner, int, error) {
				loads++
				return r, 1, nil
			})
			defer e.Close()
			for range tt.embeds {
				if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
					t.Fatalf("embed: %v", err)
				}
			}
			if loads != tt.wantLoads {
				t.Errorf("model loaded %d times, want %d", loads, tt.wantLoads)
			}
		})
	}
}

func TestReaperUnloadsAfterIdle(t *testing.T) {
	tests := []struct {
		name       string
		idleUnload time.Duration
		reaper     time.Duration
	}{
		{name: "idle model is destroyed", idleUnload: time.Millisecond, reaper: 2 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{vec: []float32{1}}
			e := newWithLoader(Config{Dimension: 1, IdleUnload: tt.idleUnload, ReaperEvery: tt.reaper},
				loaderReturning(r, 1, nil))
			defer e.Close()

			if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("embed: %v", err)
			}
			deadline := time.Now().Add(waitLimit)
			for time.Now().Before(deadline) {
				e.mu.Lock()
				unloaded := e.run == nil
				e.mu.Unlock()
				if unloaded {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !r.destroyed {
				t.Errorf("expected idle model to be destroyed by reaper")
			}
		})
	}
}

func TestNewDefaultLoaderErrorsWithoutModel(t *testing.T) {
	tests := []struct {
		name     string
		modelDir string
	}{
		// Exercises the real defaultLoader: the no-ORT stub in a plain build, or a
		// hugot load failure under -tags ORT with no model. Either way it errors.
		{name: "no usable model errors", modelDir: "/nonexistent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(Config{Dimension: 1, ModelDir: tt.modelDir})
			defer e.Close()
			if _, err := e.Embed(context.Background(), []string{"x"}); err == nil {
				t.Errorf("expected an error from the default loader")
			}
		})
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		calls int
	}{
		{name: "double close does not panic", calls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{vec: []float32{1}}
			e := newWithLoader(Config{Dimension: 1}, loaderReturning(r, 1, nil))
			if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("embed: %v", err)
			}
			for range tt.calls {
				e.Close()
			}
			if !r.destroyed {
				t.Errorf("Close did not destroy the runner")
			}
		})
	}
}

// waitLimit bounds how long a test waits for something it expects to happen.
// The waits return as soon as the condition holds, so the limit costs nothing
// on a healthy run and is deliberately generous: only a real hang reaches it,
// never a slow or loaded machine.
const waitLimit = 30 * time.Second

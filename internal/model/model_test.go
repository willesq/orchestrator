package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPDownload(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "success writes file", status: http.StatusOK, wantErr: false},
		{name: "non-200 errors", status: http.StatusNotFound, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, "weights")
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "out")
			err := httpDownload(context.Background(), srv.URL, dest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				b, _ := os.ReadFile(dest)
				if string(b) != "weights" {
					t.Errorf("downloaded %q", string(b))
				}
			}
		})
	}
}

func TestPresent(t *testing.T) {
	tests := []struct {
		name       string
		haveModel  bool
		haveTok    bool
		wantResult bool
	}{
		{name: "both present", haveModel: true, haveTok: true, wantResult: true},
		{name: "model only", haveModel: true, haveTok: false, wantResult: false},
		{name: "neither", haveModel: false, haveTok: false, wantResult: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := New(Config{Dir: dir})
			if tt.haveModel {
				_ = os.WriteFile(p.ModelPath(), []byte("m"), 0o644)
			}
			if tt.haveTok {
				_ = os.WriteFile(p.TokenizerPath(), []byte("t"), 0o644)
			}
			if got := p.Present(); got != tt.wantResult {
				t.Errorf("Present() = %v, want %v", got, tt.wantResult)
			}
		})
	}
}

func TestEnsure(t *testing.T) {
	tests := []struct {
		name        string
		preModel    bool
		dlErr       error
		wantErr     bool
		wantDLcount int
	}{
		{name: "downloads both when missing", wantDLcount: 2},
		{name: "skips present file", preModel: true, wantDLcount: 1},
		{name: "download error propagates", dlErr: errors.New("boom"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := New(Config{Dir: dir})
			if tt.preModel {
				_ = os.WriteFile(p.ModelPath(), []byte("m"), 0o644)
			}
			count := 0
			p.download = func(_ context.Context, _, dest string) error {
				count++
				if tt.dlErr != nil {
					return tt.dlErr
				}
				return os.WriteFile(dest, []byte("x"), 0o644)
			}
			err := p.Ensure(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Ensure err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && count != tt.wantDLcount {
				t.Errorf("downloads = %d, want %d", count, tt.wantDLcount)
			}
		})
	}
}

func TestPaths(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "paths are under dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(Config{Dir: "/models"})
			if p.ModelPath() != filepath.Join("/models", "model.onnx") {
				t.Errorf("ModelPath = %q", p.ModelPath())
			}
			if p.TokenizerPath() != filepath.Join("/models", "tokenizer.json") {
				t.Errorf("TokenizerPath = %q", p.TokenizerPath())
			}
		})
	}
}

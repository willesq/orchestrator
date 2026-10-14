//go:build ORT

package embed

import (
	"os"
	"strings"
	"testing"
)

// The real tokenizer needs the model files; set ORCH_TEST_MODEL_DIR to run.
func TestCountTokensUsesTheUntruncatedTokenizer(t *testing.T) {
	dir := os.Getenv("ORCH_TEST_MODEL_DIR")
	if dir == "" {
		t.Skip("ORCH_TEST_MODEL_DIR not set")
	}
	e := newWithLoader(Config{ModelDir: dir}, nil)
	defer e.Close()
	if n, ok := e.CountTokens("hello world"); !ok || n != 2 {
		t.Fatalf("CountTokens(hello world) = %d, %v", n, ok)
	}
	// Past the model's own 128-token truncation: the count must not stop there.
	if n, _ := e.CountTokens(strings.Repeat("hello ", 500)); n != 500 {
		t.Fatalf("long text counted %d tokens, want 500", n)
	}
	missing := newWithLoader(Config{ModelDir: t.TempDir()}, nil)
	defer missing.Close()
	if _, ok := missing.CountTokens("x"); ok {
		t.Fatal("a missing tokenizer reported a count")
	}
}

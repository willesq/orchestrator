//go:build !ORT

package embed

import "testing"

func TestCountTokensUnavailableWithoutORT(t *testing.T) {
	e := newWithLoader(Config{ModelDir: t.TempDir()}, nil)
	defer e.Close()
	if n, ok := e.CountTokens("hello world"); ok || n != 0 {
		t.Fatalf("CountTokens = %d, %v; want unavailable", n, ok)
	}
}

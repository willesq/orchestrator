//go:build ORT

package rag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/embed"
)

// Every chunk of a real corpus must fit what the model embeds, counted by the
// model's own tokenizer. Set ORCH_TEST_MODEL_DIR and ORCH_TEST_CORPUS to run.
func TestRealChunksFitTheModel(t *testing.T) {
	dir, corpus := os.Getenv("ORCH_TEST_MODEL_DIR"), os.Getenv("ORCH_TEST_CORPUS")
	if dir == "" || corpus == "" {
		t.Skip("ORCH_TEST_MODEL_DIR or ORCH_TEST_CORPUS not set")
	}
	e := embed.New(embed.Config{ModelDir: dir})
	defer e.Close()
	svc := NewService(e, nil, 110, 16)
	chunks, over, worst := 0, 0, 0
	_ = filepath.Walk(corpus, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !(strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".go")) {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, c := range svc.split(string(b)) {
			n, _ := e.CountTokens(c.text)
			chunks++
			worst = max(worst, n)
			if n > maxChunkTokens {
				over++
			}
		}
		return nil
	})
	t.Logf("%d chunks, largest %d tokens", chunks, worst)
	if chunks == 0 || over > 0 {
		t.Fatalf("%d of %d chunks exceed %d tokens (largest %d)", over, chunks, maxChunkTokens, worst)
	}
}

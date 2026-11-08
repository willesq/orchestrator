package configtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/config"
)

func TestSet(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	Set(t, map[string]string{"rag.qdrant.mode": `"external"`, "routing.ranks": `0`})
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetString("rag.qdrant.mode"); got != "external" {
		t.Errorf("mode = %q, want external", got)
	}
}

// fatalTB records a Fatal instead of stopping the test, so Set's guards can be
// checked.
type fatalTB struct {
	testing.TB
	fatal bool
}

func (f *fatalTB) Helper()      {}
func (f *fatalTB) Fatal(...any) { f.fatal = true }

func TestSetRefusesWithoutAnIsolatedDataDir(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", "")
	tb := &fatalTB{TB: t}
	Set(tb, map[string]string{"rag.qdrant.mode": `"external"`})
	if !tb.fatal {
		t.Error("Set wrote without ORCH_DATA_DIR pointing at a temporary directory")
	}
}

func TestSetFailsOnAnUnusableStore(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(blocker, "data"))
	tb := &fatalTB{TB: t}
	Set(tb, map[string]string{"rag.qdrant.mode": `"external"`})
	if !tb.fatal {
		t.Error("Set did not fail on a store it cannot open")
	}
}

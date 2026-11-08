// Package configtest seeds orch's settings for tests in other packages, which
// once set them through ORCH_<KEY> environment variables.
package configtest

import (
	"os"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

// Set stores raw JSON values as settings in the orch.db under ORCH_DATA_DIR.
// It skips the checks Orch.app goes through, so a test can also store a value
// they would refuse. The test must point ORCH_DATA_DIR at a temporary
// directory first, so it never writes the developer's own settings.
func Set(t testing.TB, values map[string]string) {
	t.Helper()
	if os.Getenv("ORCH_DATA_DIR") == "" {
		t.Fatal("configtest.Set: point ORCH_DATA_DIR at a temporary directory first")
		return
	}
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
		return
	}
	defer func() { _ = conn.Close() }()
	now := time.Now().UTC().Format(time.RFC3339)
	for key, raw := range values {
		if _, err := conn.Exec(`INSERT OR REPLACE INTO config_values (key, value, updated_at, updated_by) VALUES (?, ?, ?, 'test')`,
			key, raw, now); err != nil {
			t.Fatal(err)
			return
		}
	}
}

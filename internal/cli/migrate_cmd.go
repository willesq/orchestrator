package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/qdrant"
)

// migrateDeps are the side-effecting operations, injected for testability.
type migrateDeps struct {
	stop        func() error // stop orchd (releases the qdrant storage lock)
	start       func() error // start orchd (opens qdrant on the new storage)
	ready       func() bool  // is the qdrant engine serving?
	present     func() bool  // is the configured collection present after the move?
	downTimeout time.Duration
	upTimeout   time.Duration
}

// newQdrantMigrateCmd safely relocates the qdrant storage directory: stop the
// service, recursively copy the data, repoint the config, restart, and verify.
// The old directory is left in place so the move is reversible.
func newQdrantMigrateCmd() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "migrate <new-storage-dir>",
		Short: "Safely move the qdrant data to a new folder (stop → copy → repoint → restart → verify)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			newPath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			v, err := config.Load()
			if err != nil {
				return err
			}
			s, err := config.Unmarshal(v)
			if err != nil {
				return err
			}
			oldPath := s.RAG.Qdrant.Storage
			if oldPath == "" {
				oldPath = filepath.Join(paths.DataDir(), "qdrant")
			}
			port := s.RAG.Qdrant.HTTPPort
			collection := s.RAG.Qdrant.Collection
			w := c.OutOrStdout()

			if !apply {
				fmt.Fprintf(w, "DRY RUN — qdrant migrate plan (nothing will change):\n")
				fmt.Fprintf(w, "  1. stop orchd (releases the storage lock)\n")
				fmt.Fprintf(w, "  2. copy %s -> %s\n", oldPath, newPath)
				fmt.Fprintf(w, "  3. set rag.qdrant.storage=%s\n", newPath)
				fmt.Fprintf(w, "  4. start orchd (opens qdrant on the new storage)\n")
				fmt.Fprintf(w, "  5. verify engine ready + collection %q present\n", collection)
				fmt.Fprintf(w, "\nThe old data stays put (reversible). Re-run with --apply.\n")
				return nil
			}

			if oldPath == newPath {
				return fmt.Errorf("new path equals current storage (%s)", oldPath)
			}
			if _, err := os.Stat(orchdPlistPath()); err != nil {
				return fmt.Errorf("orchd plist not found — run 'orch install --apply' first (or stop orchd manually)")
			}

			return runMigrate(w, oldPath, newPath, v, realMigrateDeps(c.Context(), port, collection))
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the migration (default: dry-run)")
	return cmd
}

// realMigrateDeps builds the production side effects (launchctl + live qdrant).
func realMigrateDeps(ctx context.Context, port int, collection string) migrateDeps {
	return migrateDeps{
		// Only the supervisor action: runMigrate does its own waiting, on qdrant's
		// readiness rather than orchd's socket.
		stop:        func() error { return superviseDaemon(io.Discard, "stop") },
		start:       func() error { return superviseDaemon(io.Discard, "start") },
		ready:       func() bool { return qdrant.New(qdrant.Config{HTTPPort: port}).Ready(ctx) },
		present:     func() bool { return collectionPresent(port, collection) },
		downTimeout: 20 * time.Second,
		upTimeout:   30 * time.Second,
	}
}

// runMigrate performs the stop → copy → repoint → start → verify sequence.
func runMigrate(w io.Writer, oldPath, newPath string, v *viper.Viper, deps migrateDeps) error {
	if deps.downTimeout == 0 {
		deps.downTimeout = 20 * time.Second
	}
	if deps.upTimeout == 0 {
		deps.upTimeout = 30 * time.Second
	}
	if err := deps.stop(); err != nil {
		return fmt.Errorf("stop orchd: %w", err)
	}
	if !waitFor(deps.ready, false, deps.downTimeout) {
		return fmt.Errorf("qdrant did not stop; aborting before touching data")
	}
	fmt.Fprintln(w, "stopped orchd; storage lock released")

	if err := copyDir(oldPath, newPath); err != nil {
		_ = deps.start() // best-effort restore on the old storage
		return fmt.Errorf("copy data: %w", err)
	}
	fmt.Fprintf(w, "copied %s -> %s\n", oldPath, newPath)

	if err := config.Set(v, "rag.qdrant.storage", newPath); err != nil {
		return fmt.Errorf("update config: %w", err)
	}
	fmt.Fprintln(w, "repointed rag.qdrant.storage")

	if err := deps.start(); err != nil {
		return fmt.Errorf("start orchd: %w", err)
	}
	if !waitFor(deps.ready, true, deps.upTimeout) {
		return fmt.Errorf("qdrant did not come up on the new storage")
	}
	if !deps.present() {
		fmt.Fprintln(w, "WARNING: engine is up but the collection was not found — verify manually before deleting the old data")
	} else {
		fmt.Fprintln(w, "verified: engine ready and collection present on the new storage")
	}
	fmt.Fprintf(w, "migration complete. Old data left at %s (delete once you've confirmed).\n", oldPath)
	return nil
}

// waitFor polls pred until it equals want, or the timeout elapses.
func waitFor(pred func() bool, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() == want {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return pred() == want
}

// collectionPresent reports whether the collection exists on the local engine.
func collectionPresent(port int, collection string) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/collections/%s", port, collection)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

package cli

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
)

// newQdrantCutoverCmd migrates from the Docker qdrant to the native engine on the
// SAME storage: back up → stop the container → repoint the Go config at the
// existing storage/collection. Dry-run by default; --apply performs it.
func newQdrantCutoverCmd() *cobra.Command {
	var (
		apply     bool
		storage   string
		container string
		port      int
	)
	cmd := &cobra.Command{
		Use:   "cutover",
		Short: "Migrate Docker qdrant → native engine on the existing storage (dry-run by default)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if storage == "" {
				home, _ := os.UserHomeDir()
				storage = filepath.Join(home, ".claude", "orchestrator", "data", "qdrant")
			}
			w := c.OutOrStdout()
			if !apply {
				fmt.Fprintf(w, "DRY RUN — cutover plan (nothing will change):\n")
				fmt.Fprintf(w, "  1. back up %s\n", storage)
				fmt.Fprintf(w, "  2. docker stop %s\n", container)
				fmt.Fprintf(w, "  3. set rag.qdrant.storage=%s collection=orchestrator http_port=%d\n", storage, port)
				fmt.Fprintf(w, "  4. run: orch daemon restart ; orch qdrant status\n")
				fmt.Fprintf(w, "\nRe-run with --apply to perform the cutover.\n")
				return nil
			}

			backupDir := filepath.Join(paths.DataDir(), "backups", "qdrant-cutover")
			if err := copyDir(storage, backupDir); err != nil {
				return fmt.Errorf("backup %s: %w", storage, err)
			}
			fmt.Fprintf(w, "backed up %s -> %s\n", storage, backupDir)

			if err := runCommand("docker", "stop", container); err != nil {
				return fmt.Errorf("docker stop %s: %w", container, err)
			}
			fmt.Fprintf(w, "stopped container %s\n", container)

			if err := config.Save(map[string]any{
				"rag.qdrant.storage":    storage,
				"rag.qdrant.collection": "orchestrator",
				"rag.qdrant.http_port":  port,
			}, "cli:cutover"); err != nil {
				return err
			}
			fmt.Fprintf(w, "reconfigured Go engine → existing storage/collection\n")
			fmt.Fprintf(w, "next: orch daemon restart ; orch qdrant status\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the cutover (default: dry-run)")
	cmd.Flags().StringVar(&storage, "storage", "", "existing qdrant storage dir (default: Python orchestrator's)")
	cmd.Flags().StringVar(&container, "container", "orchestrator-qdrant", "docker container to stop")
	cmd.Flags().IntVar(&port, "port", 6333, "http port the native engine should serve")
	return cmd
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

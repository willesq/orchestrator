package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// migrateTimeout bounds one migration: it re-embeds the whole store.
const migrateTimeout = 2 * time.Hour

func newRAGMigrateCmd(cf clientFactory) *cobra.Command {
	var (
		target          string
		apply, rollback bool
	)
	cmd := operatorOnlyMsg("queue tasks cannot migrate the RAG store", &cobra.Command{
		Use:   "migrate-hybrid",
		Short: "Move the RAG store to hybrid (keyword + semantic) search; plans unless --apply",
		Long: "Copies the active collection into a new hybrid one that holds BM25 keyword vectors beside the\n" +
			"dense ones. Each source's stored text is re-chunked with today's token-sized chunker and\n" +
			"re-embedded. orchd keeps writing new ingests to both collections while the copy runs, then\n" +
			"switches to the new one. The old collection is kept untouched as a backup.\n\n" +
			"Without --apply it only reports what it would do. An interrupted run can be run again.\n" +
			"--rollback switches back to the backup (writes made after the switch stay only in the new one).",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if rollback {
				if apply || target != "" {
					return errors.New("--rollback takes no other flags")
				}
				return rollbackHybrid(c)
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), migrateTimeout)
			defer cancel()
			resp, err := client.MigrateHybrid(ctx, &orchdv1.MigrateHybridRequest{Target: target, Apply: apply})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "%s → %s: %d sources, %d chunks now, %d with token-sized chunks, %d skipped\n",
				resp.GetFrom(), resp.GetTo(), resp.GetSources(), resp.GetOldChunks(), resp.GetNewChunks(), resp.GetSkipped())
			for _, line := range []struct {
				n    int32
				what string
			}{
				{resp.GetMalformed(), "with untrusted chunk order, kept whole in point-id order"},
				{resp.GetOrphans(), "chunk(s) with no source URI, which cannot be migrated"},
				{resp.GetRemoved(), "source(s) left by an earlier run removed from the target"},
				{resp.GetOversize(), "source(s) too large to migrate; forget or re-ingest them first"},
			} {
				if line.n > 0 {
					fmt.Fprintf(out, "  %d %s\n", line.n, line.what)
				}
			}
			for _, p := range resp.GetProblems() {
				fmt.Fprintf(out, "  - %s\n", termSafe(p))
			}
			if e := resp.GetError(); e != "" {
				return errors.New(e)
			}
			if !resp.GetApplied() {
				fmt.Fprintln(out, "plan only; run with --apply to migrate")
				return nil
			}
			// orchd recorded the switch in config before making it.
			fmt.Fprintf(out, "migrated: searches now use %s; %s is kept as the backup (`orch rag migrate-hybrid --rollback` switches back)\n",
				resp.GetTo(), resp.GetFrom())
			return nil
		},
	})
	cmd.Flags().StringVar(&target, "target", "", "name of the new collection (default <active>-hybrid)")
	cmd.Flags().BoolVar(&apply, "apply", false, "migrate (without it, only plan)")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "switch back to the backup collection")
	return cmd
}

func rollbackHybrid(c *cobra.Command) error {
	v, err := config.Load()
	if err != nil {
		return err
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		return err
	}
	backup, current := s.RAG.Qdrant.BackupCollection, s.RAG.Qdrant.Collection
	if backup == "" {
		return errors.New("no backup collection recorded; nothing to roll back to")
	}
	if !rag.ValidCollectionName(backup) {
		return fmt.Errorf("recorded backup collection %q is not a valid name", backup)
	}
	if err := config.SetMany(v, map[string]any{
		"rag.qdrant.collection":        backup,
		"rag.qdrant.backup_collection": current,
	}); err != nil {
		return err
	}
	fmt.Fprintf(c.OutOrStdout(), "rag.qdrant.collection is %s again (%s kept); run `orch restart` for orchd to use it\n", backup, current)
	return nil
}

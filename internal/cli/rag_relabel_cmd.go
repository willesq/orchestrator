package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newRAGRelabelCmd brings stored chunks up to the applied spaces policy
// (SP-027). orchd places and moves them, and refuses any caller but the
// operator.
func newRAGRelabelCmd(cf clientFactory) *cobra.Command {
	var dryRun bool
	cmd := operatorOnlyMsg("queue tasks cannot relabel the RAG store", &cobra.Command{
		Use:   "relabel",
		Short: "Place stored chunks again under the applied spaces policy and move them to their space",
		Long: `Every chunk whose labels are not the applied spaces policy revision's (content stored
before placement existed, before each space had its own collection, or under an older revision)
is placed again and stamped, or moved with its vectors to its space's collection. Nothing is
re-embedded. Content that cannot be placed goes to quarantine, never to public or another space.

It is resumable: an interrupted run continues where it stopped, and a run that has nothing to do
changes nothing. Chunks that cannot be placed now (for example, GitHub could not be asked about a
repo) stay as they are and are retried by the next run.

--dry-run places and counts only. Output names counts and spaces, never a source or its text.
Only the operator can run it: in a terminal outside any agent session, or from Orch.app.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), migrateTimeout)
			defer cancel()
			resp, err := client.Relabel(ctx, &orchdv1.RelabelRequest{Apply: !dryRun})
			if err != nil {
				return err
			}
			rag.WriteRelabelReport(c.OutOrStdout(), relabelReport(resp))
			if resp.GetError() != "" {
				fmt.Fprintln(c.OutOrStdout(), "stopped early; run it again to continue")
				return errors.New(termSafe(resp.GetError()))
			}
			if dryRun && resp.GetChanged() > 0 {
				fmt.Fprintln(c.OutOrStdout(), "dry run; run without --dry-run to relabel")
			}
			return nil
		},
	})
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "place and count only; change nothing")
	return cmd
}

// relabelReport is resp as the report rag prints. Space names are checked
// by the policy (lower-case letters, digits, '-' and '_'), and are made
// terminal-safe anyway.
func relabelReport(resp *orchdv1.RelabelResponse) rag.RelabelReport {
	r := rag.RelabelReport{Rev: resp.GetRevision(), Total: int(resp.GetTotal()), Stale: int(resp.GetStale()),
		Changed: int(resp.GetChanged()), Moved: int(resp.GetMoved()), Failed: int(resp.GetFailed()), Applied: resp.GetApplied()}
	for _, t := range resp.GetTransitions() {
		r.Transitions = append(r.Transitions, rag.Transition{From: termSafe(t.GetFrom()), To: termSafe(t.GetTo()), Chunks: int(t.GetChunks())})
	}
	return r
}

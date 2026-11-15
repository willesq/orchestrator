package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// retagTimeout bounds one retag: a large source moves in seconds.
const retagTimeout = 10 * time.Minute

// newSpacesRetagCmd moves one source to another space (SP-071). orchd
// refuses any caller but the operator.
func newSpacesRetagCmd(cf clientFactory) *cobra.Command {
	var confirm bool
	var expectFrom string
	cmd := operatorOnlyMsg("queue tasks cannot retag a source", &cobra.Command{
		Use:   "retag <source_uri> <space>",
		Short: "Move a source's stored content to another space and keep later writes there",
		Long: `Moves every stored chunk of <source_uri>, with its vectors, to <space>'s collection: a
space of the applied policy, or quarantine. Nothing is re-embedded, and the source is deleted
from the space it was in only once the new copy is complete. orchd records the retag, so later
docs syncs, sources refreshes and the operator's own re-ingests of the source go to <space>. A
session's note under that source_uri goes there only if the session's space is <space>;
otherwise it is refused.

Moving a source into a public space (which every session reads), out of an isolated space, or
out of quarantine into a space needs --confirm. A retag that stops part-way can be run again; orchd also finishes it at its
next start. --expect-from refuses the retag unless the source is in that space now.

Only the operator can run it: in a terminal outside any agent session, or from Orch.app.`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), retagTimeout)
			defer cancel()
			resp, err := client.RetagSource(ctx, &orchdv1.RetagSourceRequest{SourceUri: args[0], Space: args[1],
				Confirm: confirm, ExpectFrom: expectFrom})
			if err != nil {
				return err
			}
			from := make([]string, len(resp.GetFrom()))
			for i, f := range resp.GetFrom() {
				from[i] = termSafe(f)
			}
			rag.WriteRetagReport(c.OutOrStdout(), termSafe(args[1]),
				rag.RetagReport{From: from, Chunks: int(resp.GetChunks()), Moved: resp.GetMoved()})
			return nil
		},
	})
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a move into a public space, out of an isolated one or out of quarantine")
	cmd.Flags().StringVar(&expectFrom, "expect-from", "", "refuse unless the source is stored in this space now")
	return cmd
}

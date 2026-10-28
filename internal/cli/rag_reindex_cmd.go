package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newRAGReindexCmd(cf clientFactory) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "reindex-keywords",
		Short: "Rebuild keyword (BM25) vectors made by an older tokenizer; counts unless --apply",
		Long: `Recomputes, in place, the BM25 keyword vector of every chunk an older tokenizer indexed,
from the chunk's stored text. Nothing is re-embedded and the dense vectors are untouched, so
search keeps working while it runs: chunks not yet reached still match on the terms both
tokenizers share. It reads a few hundred chunks at a time, so memory stays flat, and it is
resumable: an interrupted run continues where it stopped. Without --apply it only counts.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), migrateTimeout)
			defer cancel()
			resp, err := client.ReindexKeywords(ctx, &orchdv1.ReindexKeywordsRequest{Apply: apply})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "collection %s: %d chunks, %d indexed by an older tokenizer (current version %d)\n",
				termSafe(resp.GetCollection()), resp.GetTotal(), resp.GetStale(), resp.GetTokenizerVersion())
			switch {
			case !resp.GetApplied():
				if resp.GetStale() > 0 {
					fmt.Fprintln(out, "rerun with --apply to re-index them")
				}
			case resp.GetError() != "":
				fmt.Fprintf(out, "re-indexed %d before stopping; rerun to continue\n", resp.GetUpdated())
				return errors.New(termSafe(resp.GetError()))
			default:
				fmt.Fprintf(out, "re-indexed %d chunk(s)\n", resp.GetUpdated())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "re-index (default: count only, change nothing)")
	return cmd
}

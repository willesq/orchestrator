package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newRAGBackfillCmd stores sessions again from their transcripts on disk. orchd
// plans and does it, and refuses a queued task itself.
func newRAGBackfillCmd(cf clientFactory) *cobra.Command {
	var apply, all, summaries bool
	cmd := operatorOnlyMsg("queue tasks cannot backfill the RAG store", &cobra.Command{
		Use:   "backfill-sessions",
		Short: "Store undated sessions again with their dates; plans unless --apply",
		Long: "Sessions stored before sessions were dated show \"date unknown\" and are left out of\n" +
			"date-filtered searches. This hands each one whose transcript file is still on disk to orchd,\n" +
			"which stores it again in the background with every turn dated. Sessions whose transcript is\n" +
			"gone stay undated. Without --apply it only reports what it would do.\n\n" +
			"--all stores every session's transcript again whose file is on disk, not only undated ones,\n" +
			"so one stored before harness context was left out loses it. Records and summaries are kept.\n" +
			"It is safe to repeat: a transcript that would come out the same is not written again. A\n" +
			"session whose file is gone keeps what it has, and prompt context still skips its harness text.\n\n" +
			"--summaries, with --all, also writes each session's summary again from the clean transcript,\n" +
			"replacing the old one. It makes one model call per session.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if summaries && !all {
				return errors.New("--summaries needs --all")
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
			defer cancel()
			r, err := client.BackfillSessions(ctx, &orchdv1.BackfillSessionsRequest{All: all, Summaries: summaries, Apply: apply})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if all {
				fmt.Fprintf(out, "%d session(s) in memory: %d transcript(s) to store again, %d without a transcript on disk\n",
					r.GetTotal(), r.GetTodo(), r.GetMissing())
				if summaries && r.GetTodo() > 0 {
					fmt.Fprintf(out, "their summaries are written again too: up to %d model call(s)\n", r.GetTodo())
				}
			} else {
				fmt.Fprintf(out, "%d session(s) in memory: %d to store again with dates, %d already dated, %d without a transcript on disk\n",
					r.GetTotal(), r.GetTodo(), r.GetDated(), r.GetMissing())
			}
			if !apply {
				if r.GetTodo() > 0 {
					fmt.Fprintln(out, "plan only; run with --apply to store them again")
				}
				return nil
			}
			if r.GetTodo() > 0 {
				fmt.Fprintf(out, "handed %d session(s) to orchd, which stores them in the background\n", r.GetHanded())
			}
			return nil
		},
	})
	cmd.Flags().BoolVar(&apply, "apply", false, "hand the sessions to orchd (without it, only plan)")
	cmd.Flags().BoolVar(&all, "all", false, "every transcript still on disk, not only undated sessions; leaves records alone")
	cmd.Flags().BoolVar(&summaries, "summaries", false, "with --all, write summaries again from the clean transcripts (a model call each)")
	return cmd
}

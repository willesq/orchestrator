package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/github"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newPRsWatchesCmd lists the PRs orchd watches for review, so a watch can be
// seen and stopped without Orch.app.
func newPRsWatchesCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "watches",
		Short: "List the PRs orchd watches for review, and what each may post",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := client.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if len(resp.GetReviews()) == 0 {
				fmt.Fprintln(out, "no PRs watched for review")
				return nil
			}
			for _, r := range resp.GetReviews() {
				// Notes and verdicts can quote other people's text.
				fmt.Fprintf(out, "%-9s mode=%-8s verdict=%-7s %s\n", r.GetState(), r.GetMode(), orNone(r.GetVerdict()),
					github.Printable(r.GetUrl(), 300))
				if note := r.GetNote(); note != "" {
					fmt.Fprintf(out, "  %s\n", github.Printable(note, 300))
				}
			}
			return nil
		},
	}
}

// newPRsStopCmd stops a watch. A running review is its own task, which the
// watch does not own, so it says how to stop that too.
func newPRsStopCmd(cf clientFactory) *cobra.Command {
	var forget bool
	cmd := &cobra.Command{
		Use:   "stop <pr-url>",
		Short: "Stop watching a PR for review: no new reviews start and nothing more is posted",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			list, err := client.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{})
			if err != nil {
				return err
			}
			if _, err := client.StopPRReview(ctx, &orchdv1.StopPRReviewRequest{Url: args[0], Forget: forget}); err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "stopped watching %s\n", github.Printable(args[0], 300))
			for _, r := range list.GetReviews() {
				if r.GetUrl() == args[0] && r.GetState() == "reviewing" && r.GetTaskId() != "" {
					fmt.Fprintf(out, "its review is still running; to stop it too: orch task %s --cancel\n", r.GetTaskId())
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&forget, "forget", false, "also delete the PR's verdict")
	return cmd
}

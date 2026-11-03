package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
)

func newPRsCmd(cf clientFactory) *cobra.Command {
	var (
		blocked bool
		ready   bool
		needsMe bool
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "prs",
		Short: "Show your open PRs and review requests from the babysitter cache (never shells out to gh)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			state := prs.LoadState(paths.PRStateFile())
			list := prs.Filter(state, blocked, ready)
			if needsMe {
				list = prs.NeedsMe(state)
			}
			if asJSON {
				return json.NewEncoder(c.OutOrStdout()).Encode(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no PRs (cache empty or nothing matched)")
				return nil
			}
			for _, pr := range list {
				// Titles are other people's text: no control or bidi characters.
				fmt.Fprintf(c.OutOrStdout(), "%-45s ci=%-8s review=%-16s draft=%v\n  %s\n",
					github.Printable(pr.Repo+": "+pr.Title, 300), pr.CIState, orNone(pr.ReviewDecision), pr.IsDraft,
					github.Printable(pr.URL, 300))
				if pr.Checks != "" {
					fmt.Fprintf(c.OutOrStdout(), "  checks: %s\n", github.Printable(pr.Checks, 100))
				}
				if needsMe {
					fmt.Fprintf(c.OutOrStdout(), "  needs you: %s\n", prs.Attention(pr))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&blocked, "blocked", false, "only PRs with failing CI or requested changes")
	cmd.Flags().BoolVar(&ready, "ready", false, "only PRs that are green + approved + non-draft")
	cmd.Flags().BoolVar(&needsMe, "needs-me", false,
		"only PRs needing you (failing CI, changes requested, conflicts, review requested, ready to merge), most urgent first")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.MarkFlagsMutuallyExclusive("blocked", "ready", "needs-me")
	cmd.AddCommand(newPRsWatchesCmd(cf), operatorOnlyMsg("queue tasks cannot stop PR review watches", newPRsStopCmd(cf)))
	return cmd
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

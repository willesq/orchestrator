package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
)

func newPRsCmd() *cobra.Command {
	var (
		blocked bool
		ready   bool
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "prs",
		Short: "Show your open PRs from the babysitter cache (never shells out to gh)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			state := prs.LoadState(paths.PRStateFile())
			list := prs.Filter(state, blocked, ready)
			if asJSON {
				return json.NewEncoder(c.OutOrStdout()).Encode(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no PRs (cache empty or nothing matched)")
				return nil
			}
			for _, pr := range list {
				fmt.Fprintf(c.OutOrStdout(), "%-45s ci=%-8s review=%-16s draft=%v\n  %s\n",
					pr.Repo+": "+pr.Title, pr.CIState, orNone(pr.ReviewDecision), pr.IsDraft, pr.URL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&blocked, "blocked", false, "only PRs with failing CI or requested changes")
	cmd.Flags().BoolVar(&ready, "ready", false, "only PRs that are green + approved + non-draft")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

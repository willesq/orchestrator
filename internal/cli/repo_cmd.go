package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/sources"
)

// newRepoCmd is a convenience surface over repo-kind sources.
func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Register repos for markdown ingestion"}
	cmd.AddCommand(newRepoAddCmd(), newRepoListCmd())
	return cmd
}

func newRepoAddCmd() *cobra.Command {
	var (
		refresh string
		scope   string
		tags    []string
	)
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a repo whose markdown is ingested on a cadence",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := time.ParseDuration(refresh)
			if err != nil {
				return fmt.Errorf("invalid --refresh: %w", err)
			}
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			reg, closeFn, err := openRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.Add(c.Context(), sources.Source{
				Kind: sources.KindRepo, URI: abs, RefreshSecs: int64(d.Seconds()), Scope: scope, Tags: tags,
			}); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "registered repo %s\n", abs)
			return nil
		},
	}
	cmd.Flags().StringVar(&refresh, "refresh", "24h", "refresh interval")
	cmd.Flags().StringVar(&scope, "scope", "", "scope tag")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "tag (repeatable)")
	return cmd
}

func newRepoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered repos",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			all, err := reg.List(c.Context())
			if err != nil {
				return err
			}
			n := 0
			for _, s := range all {
				if s.Kind != sources.KindRepo {
					continue
				}
				fmt.Fprintf(c.OutOrStdout(), "%-50s status=%s\n", s.URI, s.Status)
				n++
			}
			if n == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no repos registered")
			}
			return nil
		},
	}
}

package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
)

func newDocsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Ingest GitHub code-repo docs (markdown) for an owner on a cadence",
	}
	cmd.AddCommand(
		newDocsAddCmd(), newDocsStatusCmd(), newDocsSyncCmd(),
		newDocsRefreshCmd(), newDocsListCmd(), newDocsRemoveCmd(),
	)
	return cmd
}

func openDocsRegistry() (*docs.Registry, func(), error) {
	conn, err := db.Open()
	if err != nil {
		return nil, nil, err
	}
	return docs.NewRegistry(conn), func() { _ = conn.Close() }, nil
}

func newDocsAddCmd() *cobra.Command {
	var (
		refresh string
		scope   string
		tags    []string
	)
	cmd := &cobra.Command{
		Use:   "add <owner>",
		Short: "Register a GitHub owner (user/org) for markdown ingestion",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := time.ParseDuration(refresh)
			if err != nil {
				return fmt.Errorf("invalid --refresh: %w", err)
			}
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.AddOwner(c.Context(), docs.Owner{
				Owner: args[0], Scope: scope, Tags: tags, RefreshSecs: int64(d.Seconds()),
			}); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added docs owner %s (refresh %s)\n", args[0], refresh)
			return nil
		},
	}
	cmd.Flags().StringVar(&refresh, "refresh", "6h", "refresh cadence (e.g. 6h, 24h)")
	cmd.Flags().StringVar(&scope, "scope", "", "scope tag")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "tag (repeatable)")
	return cmd
}

func newDocsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered docs owners",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owners, err := reg.ListOwners(c.Context())
			if err != nil {
				return err
			}
			if len(owners) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no docs owners registered")
				return nil
			}
			for _, o := range owners {
				fmt.Fprintf(c.OutOrStdout(), "%-20s status=%s refresh=%ds\n", o.Owner, o.Status, o.RefreshSecs)
			}
			return nil
		},
	}
}

func newDocsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show per-owner sync status (last run, next due, repos, files)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owners, err := reg.ListOwners(c.Context())
			if err != nil {
				return err
			}
			if len(owners) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no docs owners registered")
				return nil
			}
			for _, o := range owners {
				repos, _ := reg.ListRepos(c.Context(), o.Owner)
				files := 0
				for _, r := range repos {
					files += r.Files
				}
				fmt.Fprintf(c.OutOrStdout(), "%-20s last=%s  next=%s  repos=%d files=%d status=%s\n",
					o.Owner, humanAgo(o.LastSynced), nextDue(o.LastSynced, o.RefreshSecs), len(repos), files, o.Status)
			}
			return nil
		},
	}
}

func newDocsSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [owner]",
		Short: "Trigger an incremental sync now (skips unchanged repos + files)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owner := ""
			if len(args) == 1 {
				owner = args[0]
			}
			if err := reg.MarkDue(c.Context(), owner); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "marked for sync; orchd will sync on its next scan")
			return nil
		},
	}
}

func newDocsRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [owner]",
		Short: "Force a full re-ingest (clears repo-SHA + file-hash caches), then sync",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			owner := ""
			if len(args) == 1 {
				owner = args[0]
			}
			if err := reg.ClearCaches(c.Context(), owner); err != nil {
				return err
			}
			if err := reg.MarkDue(c.Context(), owner); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "caches cleared and marked for sync; orchd will re-ingest on its next scan")
			return nil
		},
	}
}

func newDocsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <owner>",
		Short: "Remove a docs owner and its sync caches",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openDocsRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.RemoveOwner(c.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed docs owner %s\n", args[0])
			return nil
		},
	}
}

// humanAgo renders a unix timestamp as a coarse "N ago" string ("never" if 0).
func humanAgo(unix int64) string {
	if unix == 0 {
		return "never"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// nextDue renders when an owner next becomes due ("now" if already due/never).
func nextDue(lastSynced, refreshSecs int64) string {
	if lastSynced == 0 {
		return "now"
	}
	due := time.Unix(lastSynced+refreshSecs, 0)
	rem := time.Until(due)
	if rem <= 0 {
		return "now"
	}
	if rem < time.Hour {
		return fmt.Sprintf("in %dm", int(rem.Minutes()))
	}
	return fmt.Sprintf("in %dh", int(rem.Hours()))
}

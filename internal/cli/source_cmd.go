package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/sources"
)

func newSourceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Manage cadence-refreshed sources (web, repo, owner)",
	}
	cmd.AddCommand(newSourceAddCmd(), newSourceListCmd(), newSourceRemoveCmd(), newSourceRefreshCmd())
	return cmd
}

func openRegistry() (*sources.Registry, func(), error) {
	conn, err := db.Open()
	if err != nil {
		return nil, nil, err
	}
	return sources.NewRegistry(conn), func() { _ = conn.Close() }, nil
}

func newSourceAddCmd() *cobra.Command {
	var (
		refresh string
		scope   string
		tags    []string
	)
	cmd := &cobra.Command{
		Use:   "add <web|repo|owner> <uri>",
		Short: "Register a source (e.g. 'add owner willesq --refresh 24h')",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := time.ParseDuration(refresh)
			if err != nil {
				return fmt.Errorf("invalid --refresh: %w", err)
			}
			s := sources.Source{Kind: args[0], URI: args[1], Scope: scope, Tags: tags, RefreshSecs: int64(d.Seconds())}
			if err := sources.Validate(&s); err != nil {
				return err
			}
			reg, closeFn, err := openRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.Add(c.Context(), s); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added %s source %s (refresh %s)\n", s.Kind, s.URI, refresh)
			return nil
		},
	}
	cmd.Flags().StringVar(&refresh, "refresh", "24h", "refresh interval (e.g. 24h, 6h)")
	cmd.Flags().StringVar(&scope, "scope", "", "scope tag")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "tag (repeatable)")
	return cmd
}

func newSourceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered sources",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			list, err := reg.List(c.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no sources registered")
				return nil
			}
			for _, s := range list {
				fmt.Fprintf(c.OutOrStdout(), "%-6s %-40s status=%s refresh=%ds\n", s.Kind, s.URI, s.Status, s.RefreshSecs)
			}
			return nil
		},
	}
}

func newSourceRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a source by id (kind|uri)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.Remove(c.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %s\n", args[0])
			return nil
		},
	}
}

func newSourceRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [id]",
		Short: "Mark a source (or all) due for refresh on the next scan",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			if err := reg.MarkDue(c.Context(), id); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "marked due; orchd will refresh on its next scan")
			return nil
		},
	}
}

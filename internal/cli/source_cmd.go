package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/db"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
)

func newSourceCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Manage cadence-refreshed sources (web, repo, owner)",
	}
	cmd.AddCommand(newSourceAddCmd(), newSourceListCmd(), newSourceRemoveCmd(), newSourceRefreshCmd(cf))
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
				fmt.Fprintf(c.OutOrStdout(), "%-6s %-40s status=%s refresh=%ds\n", s.Kind, termSafe(s.URI), termSafe(s.Status), s.RefreshSecs)
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

func newSourceRefreshCmd(cf clientFactory) *cobra.Command {
	var confirmPrune bool
	cmd := &cobra.Command{
		Use:   "refresh [id]",
		Short: "Mark a source (or all) due for refresh on the next scan",
		Long: `Mark a source (or all) due for refresh on the next scan.

A refresh removes the docs of files gone from a repo, but refreshes together remove no
more than half of what was stored at the last clean one (nothing gone) until a prune is
confirmed: past that it stops and the source's status says "attention".
--confirm-prune lets the refused prune through on the next refresh, within the hour, and
fails when nothing was refused (asks orchd, so it is refused inside a queued task).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if confirmPrune {
				if len(args) != 1 {
					return fmt.Errorf("--confirm-prune needs a source id (see orch source list)")
				}
				return confirmSourcePrune(c, cf, args[0])
			}
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
	cmd.Flags().BoolVar(&confirmPrune, "confirm-prune", false, "let the next refresh of this source remove more docs than the prune cap allows")
	return cmd
}

func confirmSourcePrune(c *cobra.Command, cf clientFactory, id string) error {
	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()
	if _, err := client.RefreshSource(ctx, &orchdv1.RefreshSourceRequest{Id: id, ConfirmPrune: true}); err != nil {
		return err
	}
	fmt.Fprintf(c.OutOrStdout(), "confirmed: the next refresh of %s may remove the docs gone from it; it runs now\n", id)
	return nil
}

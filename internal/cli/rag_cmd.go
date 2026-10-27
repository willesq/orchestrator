package cli

// `orch rag` inspects the vector store's own contents. This is deliberately a
// separate group from `orch source`, which manages the cadence-refresh registry:
// the two answer different questions ("what is indexed?" vs "what gets
// refreshed?") and conflating them previously led callers to report the registry
// as if it were the index.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/timeline"
)

func newRAGCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "rag", Short: "Inspect and prune the RAG store's contents"}
	cmd.AddCommand(newRAGSourcesCmd(cf), newRAGShowCmd(cf), newRAGForgetCmd(cf), newRAGEvalCmd(cf), newRAGMigrateCmd(cf), newRAGBackfillCmd(cf))
	return cmd
}

func newRAGSourcesCmd(cf clientFactory) *cobra.Command {
	var (
		sourceType string
		scope      string
		prefix     string
		limit      int
		since      string
		until      string
	)
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List the distinct sources held in the vector store, with chunk counts",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			from, to, err := parseBounds(since, until)
			if err != nil {
				return err
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{
				SourceType: sourceType,
				Scope:      scope,
				UriPrefix:  prefix,
				Limit:      clampInt32(limit),
				Since:      from,
				Until:      to,
			})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			srcs := resp.GetSources()
			if len(srcs) == 0 {
				fmt.Fprintln(out, "no sources indexed")
				return nil
			}
			fmt.Fprintf(out, "%d source(s), %d chunk(s)", resp.GetTotalSources(), resp.GetTotalChunks())
			if int(resp.GetTotalSources()) > len(srcs) {
				fmt.Fprintf(out, " — showing %d", len(srcs))
			}
			fmt.Fprintf(out, "\n\n%6s  %-20s %s\n", "CHUNKS", "TYPE", "SOURCE URI")
			for _, s := range srcs {
				fmt.Fprintf(out, "%6d  %-20s %s", s.GetChunks(), termSafe(s.GetSourceType()), termSafe(s.GetSourceUri()))
				if s.GetScope() != "" {
					fmt.Fprintf(out, "  scope=%s", termSafe(s.GetScope()))
				}
				if len(s.GetTags()) > 0 {
					fmt.Fprintf(out, "  tags=%s", termSafe(strings.Join(s.GetTags(), ",")))
				}
				if when := timeline.Describe(s.GetStartedAt(), s.GetEndedAt(), s.GetSourceType()); when != "" {
					fmt.Fprintf(out, "  when=%s", when)
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceType, "type", "", "filter by source type")
	cmd.Flags().StringVar(&scope, "scope", "", "filter by scope")
	cmd.Flags().StringVar(&prefix, "prefix", "", "filter to URIs with this prefix, e.g. session://")
	cmd.Flags().IntVar(&limit, "limit", 50, "max sources to show (0 for all)")
	addBoundFlags(cmd, &since, &until)
	return cmd
}

// addBoundFlags adds --since and --until, which limit results to dated content.
func addBoundFlags(cmd *cobra.Command, since, until *string) {
	cmd.Flags().StringVar(since, "since", "", "only content dated from: 2026-10-21, an RFC 3339 time, today, yesterday, or an age like 7d (UTC)")
	cmd.Flags().StringVar(until, "until", "", "only content dated to: as --since; a date means the end of that day")
}

// parseBounds reads --since and --until as Unix seconds (0 = open).
func parseBounds(since, until string) (int64, int64, error) {
	now := time.Now()
	s, err := rag.ParseBound(since, now, false)
	if err != nil {
		return 0, 0, fmt.Errorf("--since: %w", err)
	}
	u, err := rag.ParseBound(until, now, true)
	if err != nil {
		return 0, 0, fmt.Errorf("--until: %w", err)
	}
	var from, to int64
	if !s.IsZero() {
		from = s.Unix()
	}
	if !u.IsZero() {
		to = u.Unix()
	}
	return from, to, nil
}

func newRAGForgetCmd(cf clientFactory) *cobra.Command {
	var yes bool
	cmd := operatorOnlyMsg("queue tasks cannot delete from the RAG store", &cobra.Command{
		Use:   "forget <source-uri>",
		Short: "Permanently delete every chunk belonging to one source URI",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			uri := args[0]
			// Deletion is unrecoverable — there is no tombstone and no undo — so an
			// interactive run confirms rather than assuming the URI was typed right.
			// The daemon enforces this too; --yes is what sets the flag it checks.
			// Refusing is an error, so a wrapping script can tell it apart from a
			// delete that matched nothing.
			if !yes {
				return fmt.Errorf(
					"refusing to delete %s without --yes: this permanently removes every chunk under that URI", uri)
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := client.DeleteSource(ctx, &orchdv1.DeleteSourceRequest{
				SourceUri: uri, Confirm: true,
			})
			if err != nil {
				return err
			}
			if resp.GetDeleted() == 0 {
				fmt.Fprintf(c.OutOrStdout(), "no chunks matched %s — nothing deleted\n", uri)
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "deleted %d chunk(s) from %s\n", resp.GetDeleted(), uri)
			return nil
		},
	})
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the deletion")
	return cmd
}

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/memctx"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/timeline"
)

// addCollectionFlag adds --collection, which narrows a read to named
// collections within what the caller may read.
func addCollectionFlag(cmd *cobra.Command, collections *[]string) {
	cmd.Flags().StringSliceVar(collections, "collection", nil,
		"only these collections, as 'orch rag collections' names them (repeatable); narrows, never widens, what you may read")
}

func newRAGCollectionsCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "collections",
		Short: "List the vector-store collections you may read, with chunk counts",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := client.ListCollections(ctx, &orchdv1.ListCollectionsRequest{})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if !resp.GetSpacesOn() {
				fmt.Fprintln(c.ErrOrStderr(), "note: memory spaces are off; content is read as one store")
			}
			cols := resp.GetCollections()
			if len(cols) == 0 {
				fmt.Fprintln(out, "no collections readable")
				return nil
			}
			fmt.Fprintf(out, "%-32s %-16s %8s  %s\n", "NAME", "SPACE", "CHUNKS", "HYBRID")
			for _, col := range cols {
				space := termSafe(col.GetSpace())
				if space == "" {
					space = "-"
				}
				hybrid := "no"
				if col.GetHybrid() {
					hybrid = "yes"
				}
				fmt.Fprintf(out, "%-32s %-16s %8d  %s\n", termSafe(col.GetName()), space, col.GetChunks(), hybrid)
			}
			return nil
		},
	}
}

func newRAGBrowseCmd(cf clientFactory) *cobra.Command {
	var (
		scope, sourceType, uri, since, until, cursor string
		tags, allTags, collections                   []string
		limit                                        int
	)
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Page through every stored chunk matching the filters, unranked",
		Long: `Page through every chunk the filters match, in storage order and without
ranking, so a listing reaches its end where a search stops at 100 results.
The page header and the cursor for the next page go to stderr; pass that
cursor with --cursor and the same filters to continue. Chat direct messages
are never listed.`,
		Args: cobra.NoArgs,
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
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := client.BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{
				Collections: collections,
				SourceType:  sourceType,
				Scope:       scope,
				SourceUri:   uri,
				Tags:        tags,
				AllTags:     allTags,
				Since:       from,
				Until:       to,
				Limit:       clampInt32(limit),
				Cursor:      cursor,
			})
			if err != nil {
				return err
			}
			out, errOut := c.OutOrStdout(), c.ErrOrStderr()
			hits := resp.GetHits()
			if cursor == "" {
				fmt.Fprintf(errOut, "%d chunk(s) match\n", resp.GetTotal())
			}
			if len(hits) == 0 {
				fmt.Fprintln(out, "no chunks")
			}
			for i, h := range hits {
				text := h.GetText()
				if len(text) > 200 {
					text = text[:200] + "…"
				}
				chunk := ""
				if h.ChunkIndex != nil {
					chunk = fmt.Sprintf(" #%d", h.GetChunkIndex())
				}
				when := timeline.Describe(h.GetStartedAt(), h.GetEndedAt(), h.GetSourceType())
				if when != "" {
					when = " · " + when
				}
				// Stored fields are quoted and kept on one line, as search
				// prints them, so none can pass for another result.
				fmt.Fprintf(out, "%d. %s%s (%s) in %s%s\n   %s\n",
					i+1, memctx.QuoteField(h.GetSourceUri()), chunk, memctx.QuoteField(h.GetSourceType()),
					termSafe(h.GetCollection()), when, memctx.OneLine(text))
			}
			if next := resp.GetNextCursor(); next != "" {
				fmt.Fprintf(errOut, "more: --cursor %s\n", termSafe(next))
			} else {
				fmt.Fprintln(errOut, "end of listing")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "filter by scope")
	cmd.Flags().StringVar(&sourceType, "type", "", "filter by source type")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "filter: any of these tags")
	cmd.Flags().StringSliceVar(&allTags, "require-tag", nil, "filter: every one of these tags")
	cmd.Flags().StringVar(&uri, "uri", "", "filter: only this source (exact source URI)")
	cmd.Flags().IntVar(&limit, "limit", 50, "chunks per page (1 to 200)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "continue from a previous page's cursor")
	addCollectionFlag(cmd, &collections)
	addBoundFlags(cmd, &since, &until)
	return cmd
}

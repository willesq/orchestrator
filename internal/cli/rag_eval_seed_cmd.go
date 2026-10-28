package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/rageval"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// maxSeedDocs bounds how many documents a seed reads.
const maxSeedDocs = 2000

func newRAGEvalSeedCmd(cf clientFactory) *cobra.Command {
	var set string
	opts := rageval.DefaultSeedOpts
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Build the golden set's seed cases from what the store holds, without any model call",
		Long: "Reads the store and writes seed cases into the golden set: the first decision of each session\n" +
			"record, asked as \"what did we decide about ...\" and labelled with that record; and a distinctive\n" +
			"heading and code identifier from each repo doc, labelled with that doc. A reseed replaces only the\n" +
			"seed-* sections; hand-written cases (hard, legacy and anything you add) are kept. When the set is\n" +
			"new and the old YAML set exists, its cases are imported as the legacy section.\n\n" +
			"Seed questions come from the text the system already indexes, so they favour what it finds;\n" +
			"keep a section of hand-written hard questions beside them.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if set == "" {
				set = jsonlEvalSet()
			}
			s, err := rageval.LoadSet(set)
			if err != nil {
				return err
			}
			if len(s.Cases) == 0 && set == jsonlEvalSet() {
				legacy, err := rageval.LoadSet(legacyEvalSet())
				if err != nil {
					return err
				}
				for _, cs := range legacy.Cases {
					if cs.Section == "" {
						cs.Section = rageval.SectionLegacy
					}
					s.Cases = append(s.Cases, cs)
				}
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
			defer cancel()
			records, err := seedDocs(ctx, client, "session_record", "session://")
			if err != nil {
				return err
			}
			docs, err := seedDocs(ctx, client, "repo_doc", "repo://")
			if err != nil {
				return err
			}
			seed := rageval.Seed(records, docs, opts)
			merged := rageval.MergeSeed(s, seed)
			if err := rageval.SaveSet(set, merged); err != nil {
				return err
			}
			counts := map[string]int{}
			for _, cs := range merged.Cases {
				counts[cs.Section]++
			}
			fmt.Fprintf(c.OutOrStdout(), "wrote %d cases to %s (from %d session records and %d repo docs)\n",
				len(merged.Cases), set, len(records), len(docs))
			for _, sec := range []string{rageval.SectionHard, rageval.SectionLegacy, rageval.SectionDecision, rageval.SectionHeading, rageval.SectionIdentifier} {
				if counts[sec] > 0 {
					fmt.Fprintf(c.OutOrStdout(), "  %-14s %d\n", sec, counts[sec])
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&set, "set", "", "golden set file (default <data dir>/eval/golden.jsonl)")
	cmd.Flags().IntVar(&opts.Decisions, "decisions", opts.Decisions, "most session-record decision cases")
	cmd.Flags().IntVar(&opts.Headings, "headings", opts.Headings, "most repo-doc heading cases")
	cmd.Flags().IntVar(&opts.Identifiers, "identifiers", opts.Identifiers, "most repo-doc identifier cases")
	return cmd
}

// seedDocs reads the text of every source of one type under a URI prefix.
func seedDocs(ctx context.Context, client orchdv1.OrchdClient, sourceType, prefix string) ([]rageval.SeedDoc, error) {
	resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{SourceType: sourceType, UriPrefix: prefix, Limit: maxSeedDocs})
	if err != nil {
		return nil, err
	}
	var out []rageval.SeedDoc
	for _, src := range resp.GetSources() {
		doc, err := client.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: src.GetSourceUri()})
		if err != nil {
			// One unreadable source should not stop the seed.
			continue
		}
		if t := doc.GetText(); strings.TrimSpace(t) != "" {
			out = append(out, rageval.SeedDoc{URI: src.GetSourceUri(), Text: t})
		}
	}
	return out, nil
}

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rageval"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func defaultEvalSet() string { return filepath.Join(paths.ConfigDir(), "rag-eval.yaml") }
func evalRunsDir() string    { return filepath.Join(paths.DataDir(), "eval") }

func newRAGEvalCmd(cf clientFactory) *cobra.Command {
	var (
		set, save, compare string
		topK               int
		asJSON             bool
	)
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score retrieval against a golden set of queries and the sources they should find",
		Long: "Runs each query in the golden set through orchd's search and reports where the first expected\n" +
			"source ranks: hit@1, recall@k and MRR. Save a run with --save and compare a later one against it\n" +
			"with --compare, to judge a change to chunking, ranking or the embedding model by number.\n\n" +
			"The golden set is YAML (default " + "<config dir>/rag-eval.yaml" + "); add cases with `orch rag eval add`.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if topK < 1 || topK > 50 {
				return errors.New("--top-k must be between 1 and 50")
			}
			s, err := rageval.LoadSet(set)
			if err != nil {
				return err
			}
			if len(s.Cases) == 0 {
				return fmt.Errorf("no cases in %s; add one with `orch rag eval add \"<query>\" <source-uri>...`", set)
			}
			var before *rageval.Report
			if compare != "" {
				r, err := rageval.LoadRun(evalRunsDir(), compare)
				if err != nil {
					return err
				}
				before = &r
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			search := func(ctx context.Context, cs rageval.Case, k int) ([]rageval.Hit, error) {
				ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				resp, err := client.Search(ctx, &orchdv1.SearchRequest{
					Query: cs.Query, TopK: int32(k), Scope: cs.Scope, SourceType: cs.SourceType, Tags: cs.Tags,
				})
				if err != nil {
					return nil, err
				}
				hits := make([]rageval.Hit, 0, len(resp.GetHits()))
				for _, h := range resp.GetHits() {
					hits = append(hits, rageval.Hit{SourceURI: h.GetSourceUri(), Score: h.GetScore()})
				}
				return hits, nil
			}
			report := rageval.Run(c.Context(), s, topK, search)
			if save != "" {
				if err := rageval.SaveRun(evalRunsDir(), save, report); err != nil {
					return err
				}
			}
			out := c.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			printEvalReport(out, report, before)
			if save != "" {
				fmt.Fprintf(out, "saved as %q\n", save)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&set, "set", defaultEvalSet(), "golden set file")
	cmd.Flags().IntVar(&topK, "top-k", 10, "hits to consider per query")
	cmd.Flags().StringVar(&save, "save", "", "save this run under a name, for a later --compare")
	cmd.Flags().StringVar(&compare, "compare", "", "compare against a saved run")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	cmd.AddCommand(newRAGEvalAddCmd())
	return cmd
}

func printEvalReport(out io.Writer, r rageval.Report, before *rageval.Report) {
	for _, res := range r.Results {
		mark, rank := "✓", fmt.Sprint(res.Rank)
		switch {
		case res.Err != "":
			mark, rank = "!", "-"
		case res.Rank == 0:
			mark, rank = "✗", "-"
		}
		fmt.Fprintf(out, "%s %3s  %s\n", mark, rank, termSafe(res.Query))
		if res.Err != "" {
			fmt.Fprintf(out, "        error: %s\n", termSafe(res.Err))
		} else if res.Rank != 1 && len(res.Top) > 0 {
			fmt.Fprintf(out, "        top: %s\n", termSafe(strings.Join(res.Top[:min(3, len(res.Top))], ", ")))
		}
	}
	fmt.Fprintf(out, "\n%d cases, top %d: hit@1 %.2f  recall@%d %.2f  MRR %.3f\n",
		len(r.Results), r.TopK, r.HitAt1, r.TopK, r.Recall, r.MRR)
	if before == nil {
		return
	}
	fmt.Fprintf(out, "vs saved:       hit@1 %+.2f  recall@%d %+.2f  MRR %+.3f\n",
		r.HitAt1-before.HitAt1, r.TopK, r.Recall-before.Recall, r.MRR-before.MRR)
	rank := func(n int) string {
		if n == 0 {
			return "miss"
		}
		return fmt.Sprint(n)
	}
	for _, ch := range rageval.Compare(*before, r) {
		dir := "worse"
		if ch.Better() {
			dir = "better"
		}
		fmt.Fprintf(out, "  %-6s %s → %s  %s\n", dir, rank(ch.From), rank(ch.To), termSafe(ch.Query))
	}
}

func newRAGEvalAddCmd() *cobra.Command {
	var set, scope, sourceType string
	var tags []string
	cmd := &cobra.Command{
		Use:   "add <query> <source-uri>...",
		Short: "Add a golden case: a query and the source URIs that should answer it (a trailing * matches a prefix)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return errors.New("query is empty")
			}
			s, err := rageval.LoadSet(set)
			if err != nil {
				return err
			}
			for _, cs := range s.Cases {
				if cs.Query == args[0] {
					return fmt.Errorf("a case for %q already exists in %s", args[0], set)
				}
			}
			s.Cases = append(s.Cases, rageval.Case{Query: args[0], Expect: args[1:], Scope: scope, SourceType: sourceType, Tags: tags})
			if err := rageval.SaveSet(set, s); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added case %d to %s\n", len(s.Cases), set)
			return nil
		},
	}
	cmd.Flags().StringVar(&set, "set", defaultEvalSet(), "golden set file")
	cmd.Flags().StringVar(&scope, "scope", "", "search only this scope")
	cmd.Flags().StringVar(&sourceType, "source-type", "", "search only this source type")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "search only sources with this tag (repeatable)")
	return cmd
}

// termSafe replaces control characters before printing. Source URIs and
// queries come from the store, which any agent can write to, and an escape
// sequence in one could rewrite the terminal (fake a passing run, set the
// clipboard).
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

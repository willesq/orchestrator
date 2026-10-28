package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/memctx"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rageval"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// legacyEvalSet is where the golden set lived before it moved to JSONL in the
// data dir; it is still read when the new file does not exist yet.
func legacyEvalSet() string { return filepath.Join(paths.ConfigDir(), "rag-eval.yaml") }
func evalRunsDir() string   { return filepath.Join(paths.DataDir(), "eval") }
func jsonlEvalSet() string  { return filepath.Join(evalRunsDir(), "golden.jsonl") }

// defaultEvalSet is the golden set to use: the JSONL set in the data dir, or
// the legacy YAML set while only that exists.
func defaultEvalSet() string {
	if _, err := os.Stat(jsonlEvalSet()); err != nil {
		if _, lerr := os.Stat(legacyEvalSet()); lerr == nil {
			return legacyEvalSet()
		}
	}
	return jsonlEvalSet()
}

// evalFlags are the knobs one eval run is scored with.
type evalFlags struct {
	set, save, compare, mode, ranking, label, section, format, split string
	topK, perSource                                                  int
	asJSON, cases                                                    bool
}

func newRAGEvalCmd(cf clientFactory) *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score retrieval against a golden set of queries and the sources they should find",
		Long: "Runs each query in the golden set through orchd's search and reports where the first hit from an\n" +
			"expected source ranks: recall@1/3/5/10, MRR and nDCG@10, plus the share with any chunk of the\n" +
			"source in the top k, the tokens auto-injection would add, and search latency. Save a run with\n" +
			"--save and compare a later one with --compare: the paired bootstrap says whether a change is\n" +
			"larger than noise. --mode, --ranking and --per-source score other search settings on the same daemon.\n\n" +
			"The golden set is JSONL, one case per line (default <data dir>/eval/golden.jsonl; a YAML file\n" +
			"also works). Seed it from your store with `orch rag eval seed`; add cases with `orch rag eval add`.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return runEval(c, cf, f) },
	}
	cmd.Flags().StringVar(&f.set, "set", "", "golden set file (default <data dir>/eval/golden.jsonl)")
	cmd.Flags().IntVar(&f.topK, "top-k", 10, "hits to consider per query")
	cmd.Flags().StringVar(&f.save, "save", "", "save this run under a name, for a later --compare")
	cmd.Flags().StringVar(&f.compare, "compare", "", "compare against a saved run")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "print the report as JSON")
	cmd.Flags().BoolVar(&f.cases, "cases", false, "print every case, not only those outside the top 3")
	cmd.Flags().StringVar(&f.mode, "mode", "", "search mode to score: hybrid or dense (default rag.search.mode)")
	cmd.Flags().StringVar(&f.ranking, "ranking", "", "ranking to score: weighted or plain (default rag.search.ranking)")
	cmd.Flags().IntVar(&f.perSource, "per-source", 0, "most hits per source (default rag.search.per_source; -1 for none)")
	cmd.Flags().StringVar(&f.label, "label", "", "describe this run's configuration in the saved report")
	cmd.Flags().StringVar(&f.section, "section", "", "score only the cases in this section")
	cmd.Flags().StringVar(&f.split, "split", "", "score only one half of the set: tune (for choosing settings) or holdout (for judging them)")
	cmd.Flags().StringVar(&f.format, "inject-format", memctx.FormatPointers, "injection to measure tokens for: pointers or full")
	cmd.AddCommand(newRAGEvalAddCmd(), newRAGEvalSeedCmd(cf))
	return cmd
}

func runEval(c *cobra.Command, cf clientFactory, f evalFlags) error {
	if f.topK < 1 || f.topK > 50 {
		return errors.New("--top-k must be between 1 and 50")
	}
	if f.perSource < -1 || f.perSource > 50 {
		return errors.New("--per-source must be between -1 and 50")
	}
	if f.format != memctx.FormatPointers && f.format != memctx.FormatFull {
		return errors.New("--inject-format must be pointers or full")
	}
	if f.set == "" {
		f.set = defaultEvalSet()
	}
	s, err := rageval.LoadSet(f.set)
	if err != nil {
		return err
	}
	if f.split != "" && f.split != rageval.SplitTune && f.split != rageval.SplitHoldout {
		return errors.New("--split must be tune or holdout")
	}
	if f.section != "" || f.split != "" {
		var kept []rageval.Case
		for _, cs := range s.Cases {
			if (f.section == "" || cs.Section == f.section) && (f.split == "" || rageval.SplitOf(cs) == f.split) {
				kept = append(kept, cs)
			}
		}
		s.Cases = kept
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("no cases in %s; seed it with `orch rag eval seed` or add one with `orch rag eval add \"<query>\" <source-uri>...`", f.set)
	}
	var before *rageval.Report
	if f.compare != "" {
		r, err := rageval.LoadRun(evalRunsDir(), f.compare)
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
			Mode: f.mode, Ranking: f.ranking, PerSource: int32(f.perSource),
		})
		if err != nil {
			return nil, err
		}
		// A score for the configured mode must not quietly measure another.
		if fb := resp.GetFallback(); fb != "" {
			return nil, fmt.Errorf("search fell back: %s", fb)
		}
		hits := make([]rageval.Hit, 0, len(resp.GetHits()))
		for _, h := range resp.GetHits() {
			hits = append(hits, rageval.Hit{SourceURI: h.GetSourceUri(), Score: h.GetScore(), Text: h.GetText(), Raw: h})
		}
		return hits, nil
	}
	report := rageval.Run(c.Context(), s, f.topK, search, injector(f.format))
	report.Label = f.label
	if f.save != "" {
		if err := rageval.SaveRun(evalRunsDir(), f.save, report); err != nil {
			return err
		}
	}
	out := c.OutOrStdout()
	if f.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printEvalReport(out, report, before, f.cases)
	if f.save != "" {
		fmt.Fprintf(out, "saved as %q\n", f.save)
	}
	return nil
}

// autoInjectHits is how many hits auto-injection shows by default.
const autoInjectHits = 3

// injector measures what auto-injection would add for a case's top hits,
// one per source, as the hook picks them.
func injector(format string) rageval.Injector {
	return func(cs rageval.Case, hits []rageval.Hit) int {
		var picked []*orchdv1.SearchHit
		seen := map[string]bool{}
		for _, h := range hits {
			sh, ok := h.Raw.(*orchdv1.SearchHit)
			if !ok || seen[h.SourceURI] {
				continue
			}
			seen[h.SourceURI] = true
			picked = append(picked, sh)
			if len(picked) == autoInjectHits {
				break
			}
		}
		text, _ := memctx.Render(cs.Query, picked, memctx.Options{Format: format, Untrusted: memctx.UntrustedMark, MaxChars: 2000})
		return len(text)
	}
}

func printEvalReport(out io.Writer, r rageval.Report, before *rageval.Report, all bool) {
	for _, res := range r.Results {
		mark, rank := "✓", fmt.Sprint(res.Rank)
		switch {
		case res.Err != "":
			mark, rank = "!", "-"
		case res.Rank == 0:
			mark, rank = "✗", "-"
		}
		if !all && res.Err == "" && res.Rank > 0 && res.Rank <= 3 {
			continue
		}
		fmt.Fprintf(out, "%s %3s  %s\n", mark, rank, termSafe(res.Query))
		if res.Err != "" {
			fmt.Fprintf(out, "        error: %s\n", termSafe(res.Err))
		} else if res.Rank != 1 && len(res.Top) > 0 {
			fmt.Fprintf(out, "        top: %s\n", termSafe(strings.Join(res.Top[:min(3, len(res.Top))], ", ")))
		}
	}
	fmt.Fprintf(out, "\n%d cases, top %d", r.N, r.TopK)
	if r.Label != "" {
		fmt.Fprintf(out, " (%s)", termSafe(r.Label))
	}
	fmt.Fprintf(out, ":\n  %s\n", metricLine(r.Metrics))
	fmt.Fprintf(out, "  source hit@%d %.2f  injected ~%.0f tokens  latency p50 %.0fms p95 %.0fms",
		r.TopK, r.SourceHit, r.InjectedTokens, r.P50MS, r.P95MS)
	if r.Errors > 0 {
		fmt.Fprintf(out, "  errors %d", r.Errors)
	}
	fmt.Fprintln(out)
	names := make([]string, 0, len(r.Sections))
	for name := range r.Sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := r.Sections[name]
		fmt.Fprintf(out, "  %-14s n=%-4d %s\n", termSafe(name), m.N, metricLine(m))
		for _, sp := range []string{rageval.SplitTune, rageval.SplitHoldout} {
			if m, ok := r.SectionSplits[name+"/"+sp]; ok && len(r.Splits) > 1 {
				fmt.Fprintf(out, "    %-12s n=%-4d %s\n", sp, m.N, metricLine(m))
			}
		}
	}
	for _, sp := range []string{rageval.SplitTune, rageval.SplitHoldout} {
		if m, ok := r.Splits[sp]; ok && len(r.Splits) > 1 {
			fmt.Fprintf(out, "  %-14s n=%-4d %s\n", "all/"+sp, m.N, metricLine(m))
		}
	}
	if before == nil {
		return
	}
	if before.Version != r.Version {
		fmt.Fprintf(out, "\nthe saved run was scored differently (version %d, this is %d); rerun it to compare\n", before.Version, r.Version)
		return
	}
	fmt.Fprintf(out, "\nvs saved: %s\n", metricDelta(before.Metrics, r.Metrics))
	for _, p := range rageval.CompareMetrics(*before, r) {
		sig := ""
		if p.Significant() {
			sig = "  significant"
		}
		fmt.Fprintf(out, "  %-9s %.3f → %.3f  Δ %+.3f  95%% CI [%+.3f, %+.3f]  p %.3f  (n=%d)%s\n",
			p.Metric, p.Before, p.After, p.Delta, p.Lo, p.Hi, p.P, p.N, sig)
	}
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

func metricLine(m rageval.Metrics) string {
	return fmt.Sprintf("recall@1 %.2f  @3 %.2f  @5 %.2f  @10 %.2f  MRR %.3f  nDCG@10 %.3f",
		m.RecallAt1, m.RecallAt3, m.RecallAt5, m.RecallAt10, m.MRR, m.NDCG10)
}

func metricDelta(a, b rageval.Metrics) string {
	return fmt.Sprintf("recall@1 %+.2f  @3 %+.2f  @5 %+.2f  @10 %+.2f  MRR %+.3f  nDCG@10 %+.3f  tokens %+.0f",
		b.RecallAt1-a.RecallAt1, b.RecallAt3-a.RecallAt3, b.RecallAt5-a.RecallAt5, b.RecallAt10-a.RecallAt10,
		b.MRR-a.MRR, b.NDCG10-a.NDCG10, b.InjectedTokens-a.InjectedTokens)
}

func newRAGEvalAddCmd() *cobra.Command {
	var set, scope, sourceType, span, section string
	var tags []string
	cmd := &cobra.Command{
		Use:   "add <query> <source-uri>...",
		Short: "Add a golden case: a query and the source URIs that should answer it (a trailing * matches a prefix)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return errors.New("query is empty")
			}
			if set == "" {
				set = defaultEvalSet()
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
			s.Cases = append(s.Cases, rageval.Case{Query: args[0], Expect: args[1:], Scope: scope,
				SourceType: sourceType, Tags: tags, Span: span, Section: section})
			if err := rageval.SaveSet(set, s); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added case %d to %s\n", len(s.Cases), set)
			return nil
		},
	}
	cmd.Flags().StringVar(&set, "set", "", "golden set file (default <data dir>/eval/golden.jsonl)")
	cmd.Flags().StringVar(&scope, "scope", "", "search only this scope")
	cmd.Flags().StringVar(&sourceType, "source-type", "", "search only this source type")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "search only sources with this tag (repeatable)")
	cmd.Flags().StringVar(&span, "span", "", "text the answering chunk must contain")
	cmd.Flags().StringVar(&section, "section", rageval.SectionHard, "section to report the case under")
	return cmd
}

// termSafe replaces control characters before printing. Source URIs and
// queries come from the store, which any agent can write to, and an escape
// sequence in one could rewrite the terminal (fake a passing run, set the
// clipboard).
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		// Format characters too: a bidi override can reorder what is shown.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

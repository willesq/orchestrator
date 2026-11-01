package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/hook"
	"github.com/willesq/orchestrator/internal/logs"
	"github.com/willesq/orchestrator/internal/memctx"
)

// newRAGWhyCmd shows what a prompt would receive as context and why each
// candidate was kept or passed over, so a bad injection can be understood and
// corrected (orch rag forget, or ingesting under the same URI) rather than guessed at.
func newRAGWhyCmd(cf clientFactory) *cobra.Command {
	var (
		cwd     string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "why <prompt>",
		Short: "Show what a prompt would get as context, and why each hit was kept or dropped",
		Long: "Replays the prompt-context decision the hook makes for this prompt: whether to search at\n" +
			"all, each candidate's score and verdict, and the exact block an agent would receive. It changes\n" +
			"nothing, and marks nothing as injected. --cwd says which project to act as (default: here).",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
			defer cancel()
			start := time.Now()
			ex := hook.Explain(ctx, hook.LoadAutoRAG(), client, strings.TrimSpace(args[0]), cwd)
			if jsonOut {
				return writeWhyJSON(c.OutOrStdout(), ex)
			}
			writeWhy(c.OutOrStdout(), ex, time.Since(start))
			return nil
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "project directory to act as")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print as JSON")
	return cmd
}

func writeWhy(out io.Writer, ex hook.Explanation, took time.Duration) {
	if ex.Disabled {
		fmt.Fprintln(out, "note: hooks.auto_rag.enabled is off, so real prompts get nothing")
	}
	if !ex.Gated {
		fmt.Fprintln(out, "gate: off (hooks.auto_rag.gate), score floors only")
	}
	if ex.Skipped != "" {
		fmt.Fprintf(out, "no search: %s\n", ex.Skipped)
		fmt.Fprintf(out, "topic terms: %s\n", termList(ex.Terms))
		return
	}
	fmt.Fprintf(out, "searched in %s; score floor %.2f; topic terms: %s\n", took.Round(time.Millisecond), ex.Floor, termList(ex.Terms))
	for _, e := range ex.Errors {
		fmt.Fprintf(out, "search failed: %s\n", logs.Clean(e))
	}
	for _, d := range ex.Decisions {
		verdict := "kept"
		if d.Reason != "" {
			verdict = "dropped: " + d.Reason
		}
		// Quoted, since a stored URI could otherwise spell out a verdict of its own.
		fmt.Fprintf(out, "%.3f  %s  %s  %s  [shared: %s]\n", d.Hit.GetScore(), memctx.QuoteField(d.Hit.GetSourceUri()), trustOf(d).Label(), verdict, termList(d.Shared))
	}
	if ex.Block == "" {
		fmt.Fprintln(out, "\ninjected: nothing")
		return
	}
	// The block quotes everything it holds from the store, so it prints as the agent gets it.
	fmt.Fprintf(out, "\ninjected: %d pointer(s), about %d tokens\n\n%s", ex.Written, len(ex.Block)/4, ex.Block)
}

func trustOf(d hook.Decision) memctx.Trust {
	return memctx.TrustOf(d.Hit.GetTrust(), d.Hit.GetSourceType(), d.Hit.GetSourceUri())
}

func termList(terms []string) string {
	if len(terms) == 0 {
		return "none"
	}
	sorted := append([]string(nil), terms...)
	sort.Strings(sorted)
	if len(sorted) > 12 {
		return logs.Clean(strings.Join(sorted[:12], ", ")) + fmt.Sprintf(", +%d more", len(sorted)-12)
	}
	return logs.Clean(strings.Join(sorted, ", "))
}

func writeWhyJSON(out io.Writer, ex hook.Explanation) error {
	type candidate struct {
		URI    string   `json:"uri"`
		Type   string   `json:"type"`
		Score  float32  `json:"score"`
		Exact  bool     `json:"exact"`
		Kept   bool     `json:"kept"`
		Reason string   `json:"reason,omitempty"`
		Shared []string `json:"shared,omitempty"`
		// Trust and Flagged say how to read Text, which is retrieved data.
		Trust   string `json:"trust"`
		Flagged bool   `json:"flagged"`
		Text    string `json:"text"`
	}
	v := struct {
		Gated      bool        `json:"gated"`
		Disabled   bool        `json:"disabled"`
		Skipped    string      `json:"skipped,omitempty"`
		Terms      []string    `json:"terms"`
		Floor      float64     `json:"floor"`
		Candidates []candidate `json:"candidates"`
		Errors     []string    `json:"errors,omitempty"`
		Block      string      `json:"block"`
	}{Gated: ex.Gated, Disabled: ex.Disabled, Skipped: ex.Skipped, Terms: ex.Terms, Floor: ex.Floor, Errors: ex.Errors, Block: ex.Block}
	for _, d := range ex.Decisions {
		v.Candidates = append(v.Candidates, candidate{
			URI: d.Hit.GetSourceUri(), Type: d.Hit.GetSourceType(), Score: d.Hit.GetScore(), Exact: d.Hit.GetExact(),
			Kept: d.Reason == "", Reason: d.Reason, Shared: d.Shared, Trust: trustOf(d).Label(), Flagged: memctx.Flagged(d.Hit), Text: d.Hit.GetText(),
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

package hook

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// userPromptSubmit auto-injects RAG context for question-shaped prompts by
// searching orchd and writing a tagged block to stdout, which the harness prepends
// to the next turn. Degrades to a silent no-op on any error.
func userPromptSubmit(cfg config.AutoRAGSettings, p Payload, out io.Writer, cf clientFactory) error {
	if !cfg.Enabled {
		return nil
	}
	prompt := strings.TrimSpace(p.Prompt)
	if len(prompt) < cfg.MinPromptChars || !looksLikeQuestion(prompt) {
		return nil
	}
	client, cleanup, err := cf()
	if err != nil {
		return nil
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kept := projectFirstHits(ctx, client, prompt, detectProject(p.Cwd).searchTags(), cfg)
	if len(kept) == 0 {
		return nil
	}
	fmt.Fprint(out, formatInjection(kept, cfg.MaxChars))
	return nil
}

// defaultTopK mirrors the daemon's search default for an unset hooks.auto_rag.top_k.
const defaultTopK = 5

// projectFirstHits fills up to top_k slots with hits above min_score, taking the
// current project's own hits first and topping up from the whole store. An
// unscoped search ranks every repo and vendored README by similarity alone, so
// another project's near-match routinely outranks this one's actual decisions.
// The global pass keeps cross-project knowledge (policies, research) reachable.
// A failed search contributes nothing; the hook still never blocks.
func projectFirstHits(ctx context.Context, client orchdv1.OrchdClient, prompt string, projectTags []string, cfg config.AutoRAGSettings) []*orchdv1.SearchHit {
	limit := cfg.TopK
	if limit <= 0 {
		limit = defaultTopK
	}
	var kept []*orchdv1.SearchHit
	seen := map[string]bool{}
	take := func(req *orchdv1.SearchRequest) {
		resp, err := client.Search(ctx, req)
		if err != nil {
			return
		}
		for _, h := range resp.GetHits() {
			key := h.GetSourceUri() + "\x00" + h.GetText()
			if len(kept) >= limit || float64(h.GetScore()) < cfg.MinScore || seen[key] {
				continue
			}
			seen[key] = true
			kept = append(kept, h)
		}
	}
	if len(projectTags) > 0 {
		take(&orchdv1.SearchRequest{Query: prompt, TopK: int32(limit), Tags: projectTags})
	}
	if len(kept) < limit {
		take(&orchdv1.SearchRequest{Query: prompt, TopK: int32(limit)})
	}
	return kept
}

var questionPrefixes = []string{
	"who", "what", "when", "where", "why", "how", "which",
	"can", "could", "did", "do", "does", "is", "are", "should", "would", "will",
}

func looksLikeQuestion(prompt string) bool {
	t := strings.ToLower(strings.TrimSpace(prompt))
	if strings.HasSuffix(t, "?") {
		return true
	}
	for _, w := range questionPrefixes {
		if strings.HasPrefix(t, w+" ") {
			return true
		}
	}
	for _, s := range []string{"find ", "show me", "search for", "look up", "remind me"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

func formatInjection(hits []*orchdv1.SearchHit, maxChars int) string {
	var b strings.Builder
	b.WriteString("## Auto-injected RAG context\n")
	b.WriteString("(Top hits from your local orchestrator RAG store. Cite URIs in your answer. If irrelevant, ignore.)\n\n")
	for i, h := range hits {
		seg := fmt.Sprintf("### [%d] %s (%s, score=%.3f)\n%s\n\n",
			i+1, h.GetSourceUri(), h.GetSourceType(), h.GetScore(), h.GetText())
		if maxChars > 0 && b.Len()+len(seg) > maxChars {
			break
		}
		b.WriteString(seg)
	}
	return b.String()
}

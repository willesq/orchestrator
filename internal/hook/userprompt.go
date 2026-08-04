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
	resp, err := client.Search(ctx, &orchdv1.SearchRequest{Query: prompt, TopK: int32(cfg.TopK)})
	if err != nil {
		return nil
	}

	var kept []*orchdv1.SearchHit
	for _, h := range resp.GetHits() {
		if float64(h.GetScore()) >= cfg.MinScore {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	fmt.Fprint(out, formatInjection(kept, cfg.MaxChars))
	return nil
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

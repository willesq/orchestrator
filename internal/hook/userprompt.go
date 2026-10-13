package hook

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// userPromptSubmit auto-injects RAG context by searching orchd and writing a
// tagged block to stdout, which the harness prepends to the next turn. Degrades
// to a silent no-op on any error.
//
// Every prompt is searched, not only questions: "fix the flaky socket test" is
// exactly where a past decision helps. Prompts that aren't question-shaped must
// clear the stricter min_score_statement instead, since an instruction says
// less about what it needs than a question does. A chunk already injected
// earlier in the session is not injected again.
func userPromptSubmit(cfg config.AutoRAGSettings, p Payload, out io.Writer, cf clientFactory) error {
	if !cfg.Enabled {
		return nil
	}
	prompt := strings.TrimSpace(p.Prompt)
	if len(prompt) < cfg.MinPromptChars {
		return nil
	}
	if !looksLikeQuestion(prompt) {
		cfg.MinScore = math.Max(cfg.MinScore, cfg.MinScoreStatement)
	}
	client, cleanup, err := cf()
	if err != nil {
		return nil
	}
	defer cleanup()

	st := loadState(p.SessionID)
	shown := make(map[string]bool, len(st.Injected))
	for _, k := range st.Injected {
		shown[k] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kept := projectFirstHits(ctx, client, prompt, detectProject(p.Cwd).searchTags(), cfg, shown)
	text, written := formatInjection(kept, cfg.MaxChars)
	if len(written) == 0 {
		return nil
	}
	fmt.Fprint(out, text)
	for _, h := range written {
		st.Injected = append(st.Injected, hitKey(h.GetSourceUri(), h.GetText()))
	}
	_ = saveState(p.SessionID, st)
	return nil
}

// defaultTopK mirrors the daemon's search default for an unset hooks.auto_rag.top_k.
const defaultTopK = 5

// projectFirstHits fills up to top_k slots with hits above min_score, taking the
// current project's own hits first and topping up from the whole store. An
// unscoped search ranks every repo and vendored README by similarity alone, so
// another project's near-match routinely outranks this one's actual decisions.
// The global pass keeps cross-project knowledge (policies, research) reachable.
// Hits whose key is in skip were already injected this session and are passed
// over. A failed search contributes nothing; the hook still never blocks.
func projectFirstHits(ctx context.Context, client orchdv1.OrchdClient, prompt string, projectTags []string, cfg config.AutoRAGSettings, skip map[string]bool) []*orchdv1.SearchHit {
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
			if len(kept) >= limit || float64(h.GetScore()) < cfg.MinScore || seen[key] || skip[hitKey(h.GetSourceUri(), h.GetText())] {
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

// formatInjection renders the hits that fit within maxChars and returns them
// alongside the text. With none written it returns no text, rather than a
// heading with nothing under it.
func formatInjection(hits []*orchdv1.SearchHit, maxChars int) (string, []*orchdv1.SearchHit) {
	var b strings.Builder
	b.WriteString("## Auto-injected RAG context\n")
	b.WriteString("(Top hits from your local orchestrator RAG store. Cite URIs in your answer. If irrelevant, ignore.)\n\n")
	var written []*orchdv1.SearchHit
	for i, h := range hits {
		seg := fmt.Sprintf("### [%d] %s (%s, score=%.3f)\n%s\n\n",
			i+1, h.GetSourceUri(), h.GetSourceType(), h.GetScore(), h.GetText())
		if maxChars > 0 && b.Len()+len(seg) > maxChars {
			break
		}
		b.WriteString(seg)
		written = append(written, h)
	}
	if len(written) == 0 {
		return "", nil
	}
	return b.String(), written
}

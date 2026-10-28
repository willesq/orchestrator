package hook

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/memctx"
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
//
// Before any of that, it reports events that arrived since the last prompt:
// finished subagents and background tasks, which otherwise surfaced only in the
// next session's digest. A queued task's own session gets none, since other
// tasks' statuses are noise there.
func userPromptSubmit(cfg config.AutoRAGSettings, p Payload, out io.Writer, cf clientFactory) error {
	st := loadState(p.SessionID)
	if os.Getenv(agentrun.TaskIDEnv) == "" {
		if lines := takeUpdates(&st, p.SessionID); len(lines) > 0 {
			writeUpdates(out, lines)
		}
	}
	enrichPrompt(cfg, p, out, cf, &st)
	_ = saveState(p.SessionID, st)
	return nil
}

// enrichPrompt writes the auto-RAG block for the prompt, recording what it
// injected in st.
func enrichPrompt(cfg config.AutoRAGSettings, p Payload, out io.Writer, cf clientFactory, st *sessionState) {
	if !cfg.Enabled {
		return
	}
	prompt := strings.TrimSpace(p.Prompt)
	if len(prompt) < cfg.MinPromptChars {
		return
	}
	if !looksLikeQuestion(prompt) {
		cfg.MinScore = math.Max(cfg.MinScore, cfg.MinScoreStatement)
	}
	client, cleanup, err := cf()
	if err != nil {
		return
	}
	defer cleanup()

	shown := make(map[string]bool, len(st.Injected))
	for _, k := range st.Injected {
		shown[k] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kept := projectFirstHits(ctx, client, prompt, detectProject(p.Cwd).searchTags(), cfg, shown)
	text, written := memctx.Render(prompt, kept, injectOptions(cfg))
	if len(written) == 0 {
		return
	}
	fmt.Fprint(out, text)
	for _, h := range written {
		st.Injected = append(st.Injected, hitKey(h.GetSourceUri(), h.GetText()))
	}
}

// defaultTopK mirrors the daemon's search default for an unset hooks.auto_rag.top_k.
const defaultTopK = 5

// maxAutoRAGTopK mirrors the daemon's largest accepted search size.
const maxAutoRAGTopK = 100

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
	// The daemon rejects larger requests, which would silently inject nothing.
	limit = min(limit, maxAutoRAGTopK)
	var kept []*orchdv1.SearchHit
	// One hit per source: only a few are injected, and a second chunk of the
	// same document would take a slot another source could fill. The pointer
	// leads to the rest of it.
	seen := map[string]bool{}
	exactOnly := 0
	take := func(req *orchdv1.SearchRequest) {
		resp, err := client.Search(ctx, req)
		if err != nil {
			return
		}
		for _, h := range resp.GetHits() {
			key := h.GetSourceUri()
			if len(kept) >= limit || seen[key] || skip[hitKey(h.GetSourceUri(), h.GetText())] {
				continue
			}
			if cfg.Untrusted == memctx.UntrustedExclude && memctx.TrustOf(h.GetSourceType(), h.GetSourceUri()).Untrusted() {
				continue
			}
			switch relevance(h, cfg.MinScore) {
			case notRelevant:
				continue
			case byIdentifier:
				if exactOnly >= maxExactOnly {
					continue
				}
				exactOnly++
			}
			seen[key] = true
			kept = append(kept, h)
		}
	}
	if len(projectTags) > 0 {
		take(&orchdv1.SearchRequest{Query: prompt, TopK: int32(limit), Tags: projectTags, PerSource: 1})
	}
	if len(kept) < limit {
		take(&orchdv1.SearchRequest{Query: prompt, TopK: int32(limit), PerSource: 1})
	}
	return kept
}

// injectOptions reads the injection settings, defaulting anything unset or
// unknown to pointers with untrusted content marked.
func injectOptions(cfg config.AutoRAGSettings) memctx.Options {
	o := memctx.Options{Format: memctx.FormatPointers, Untrusted: memctx.UntrustedMark, MaxChars: cfg.MaxChars}
	if cfg.Format == memctx.FormatFull {
		o.Format = memctx.FormatFull
	}
	if cfg.Untrusted == memctx.UntrustedExclude {
		o.Untrusted = memctx.UntrustedExclude
	}
	return o
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

// How a hit qualifies for injection.
const (
	notRelevant = iota
	byMeaning
	byIdentifier
)

// maxExactOnly caps the hits per prompt admitted by identifier alone, so text
// that merely repeats a prompt's identifiers cannot fill the context.
const maxExactOnly = 1

// relevance gates a hit for injection: its meaning must clear the floor, or it
// must contain, as a whole token, a code identifier the prompt names and still
// clear half the floor. Hybrid search finds such exact matches even when their
// wording scores low by meaning, and a prompt naming queue.worker.verify.repos
// wants the chunk that says it; the lower floor keeps unrelated text out.
func relevance(h *orchdv1.SearchHit, floor float64) int {
	score := float64(h.GetScore())
	switch {
	case score >= floor:
		return byMeaning
	case h.GetExact() && score >= floor/2:
		return byIdentifier
	}
	return notRelevant
}

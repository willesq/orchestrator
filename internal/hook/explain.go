package hook

import (
	"context"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/memctx"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Decision is what prompt context did with one search hit: a set Reason says why
// it was passed over.
type Decision struct {
	Hit    *orchdv1.SearchHit
	Reason string
	Shared []string // the prompt's topic terms it contains, under the gate
}

// Explanation is what a prompt would receive, and why.
type Explanation struct {
	// Gated reports that hooks.auto_rag.gate is on. Disabled reports that
	// hooks.auto_rag.enabled is off, so a real prompt gets nothing.
	Gated, Disabled bool
	// Skipped says why no search would run, when none would.
	Skipped string
	// Terms are the prompt's topic terms.
	Terms     []string
	Floor     float64
	Decisions []Decision
	// Errors holds failed searches. A prompt in that state simply gets less.
	Errors []string
	// Block is exactly the text the hook would print.
	Block   string
	Written int
}

// LoadAutoRAG reads the prompt-context settings, as the hook does.
func LoadAutoRAG() config.AutoRAGSettings { return loadAutoRAG() }

// Explain replays the prompt-context decision for a prompt without changing any
// session's state: same gate, same searches, same rendering as the hook, with
// nothing marked as injected.
func Explain(ctx context.Context, cfg config.AutoRAGSettings, client orchdv1.OrchdClient, prompt, cwd string) Explanation {
	ex := Explanation{Gated: gateOn(cfg.Gate), Disabled: !cfg.Enabled, Floor: promptFloor(cfg, prompt)}
	if len(prompt) < cfg.MinPromptChars {
		ex.Skipped = "shorter than hooks.auto_rag.min_prompt_chars"
		return ex
	}
	gate := promptGate(cfg, prompt)
	shown := prompt
	if gate != nil {
		shown = gate.Query
		for t := range gate.Terms {
			ex.Terms = append(ex.Terms, t)
		}
		if !gate.Search {
			ex.Skipped = gate.Reason
			return ex
		}
	}
	cfg.MinScore = ex.Floor
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	trace := func(h *orchdv1.SearchHit, reason string) {
		if h == nil {
			ex.Errors = append(ex.Errors, reason)
			return
		}
		d := Decision{Hit: h, Reason: reason}
		if gate != nil {
			d.Shared = gate.Shared(h)
		}
		ex.Decisions = append(ex.Decisions, d)
	}
	kept := selectHits(ctx, client, prompt, detectProject(cwd).searchTags(), cfg, nil, gate, trace)
	block, written := memctx.Render(shown, kept, injectOptions(cfg))
	ex.Block, ex.Written = block, len(written)
	return ex
}

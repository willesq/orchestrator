package hook

import (
	"context"
	"time"

	"google.golang.org/grpc"

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
	// Searched is what orchd let the caller search (SP-031); nil when no
	// search answered or orchd is older and does not say.
	Searched *Searched
}

// Searched is the boundary a search ran inside, as orchd decided it from the
// caller alone: the caller's space, whether the search was limited to its
// readable spaces, and the collections read.
type Searched struct {
	CallerSpace string
	Scoped      bool
	Collections []string
}

// searchRecorder passes searches through and keeps what the last answer
// said was searched.
type searchRecorder struct {
	orchdv1.OrchdClient
	last *Searched
}

func (r *searchRecorder) Search(ctx context.Context, in *orchdv1.SearchRequest, opts ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	resp, err := r.OrchdClient.Search(ctx, in, opts...)
	if err == nil && (resp.GetScoped() || len(resp.GetCollections()) > 0 || resp.GetCallerSpace() != "") {
		r.last = &Searched{CallerSpace: resp.GetCallerSpace(), Scoped: resp.GetScoped(), Collections: resp.GetCollections()}
	}
	return resp, err
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
	rec := &searchRecorder{OrchdClient: client}
	kept := selectHits(ctx, rec, prompt, detectProject(cwd).searchTags(), cfg, nil, gate, trace)
	ex.Searched = rec.last
	block, written := memctx.Render(shown, kept, injectOptions(cfg))
	ex.Block, ex.Written = block, len(written)
	return ex
}

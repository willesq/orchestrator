package hook

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
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
	gate := promptGate(cfg, prompt)
	if gate != nil && !gate.Search {
		return
	}
	shown := prompt
	if gate != nil {
		shown = gate.Query
	}
	cfg.MinScore = promptFloor(cfg, prompt)
	client, cleanup, err := cf()
	if err != nil {
		return
	}
	defer cleanup()

	injected := make(map[string]bool, len(st.Injected))
	for _, k := range st.Injected {
		injected[k] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kept := selectHits(ctx, client, prompt, detectProject(p.Cwd).searchTags(), cfg, injected, gate, nil)
	text, written := memctx.Render(shown, kept, injectOptions(cfg))
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

// gateDepth is how many candidates per slot a gated search asks for, since the
// gate turns away hits a plain top_k would have stopped at.
const gateDepth = 4

// promptGate is the prompt's verdict under the evidence gate, or nil when the
// gate is off.
func promptGate(cfg config.AutoRAGSettings, prompt string) *memctx.PromptCheck {
	if cfg.Gate != GateEvidence {
		return nil
	}
	check := memctx.CheckPrompt(prompt)
	return &check
}

// promptFloor is the score a hit must clear for this prompt.
func promptFloor(cfg config.AutoRAGSettings, prompt string) float64 {
	if looksLikeQuestion(prompt) {
		return cfg.MinScore
	}
	return math.Max(cfg.MinScore, cfg.MinScoreStatement)
}

// GateEvidence is the hooks.auto_rag.gate value that turns the gate on.
const GateEvidence = "evidence"

// projectFirstHits fills up to top_k slots with hits above min_score, taking the
// current project's own hits first and topping up from the whole store. An
// unscoped search ranks every repo and vendored README by similarity alone, so
// another project's near-match routinely outranks this one's actual decisions.
// The global pass keeps cross-project knowledge (policies, research) reachable.
// Hits whose key is in skip were already injected this session and are passed
// over. A failed search contributes nothing; the hook still never blocks.
func projectFirstHits(ctx context.Context, client orchdv1.OrchdClient, prompt string, projectTags []string, cfg config.AutoRAGSettings, skip map[string]bool) []*orchdv1.SearchHit {
	return selectHits(ctx, client, prompt, projectTags, cfg, skip, nil, nil)
}

// tracer hears what became of each candidate: an empty reason means it was kept.
// A nil hit reports a failed search, with the error as the reason.
type tracer func(h *orchdv1.SearchHit, reason string)

// Why a candidate was passed over before the gate saw it.
const (
	reasonBelowFloor = "scores below the floor"
	reasonInjected   = "already injected this session"
	reasonSameSource = "another chunk of a source already kept"
	reasonUntrusted  = "untrusted, and hooks.auto_rag.untrusted is exclude"
	reasonIdentifier = "only an identifier match, and one is already kept"
	reasonDuplicate  = "says what a kept hit says"
	reasonFull       = "all slots taken"
)

// selectHits is projectFirstHits with the evidence gate, when gate is set, and a
// trace of every decision for `orch rag why`.
func selectHits(ctx context.Context, client orchdv1.OrchdClient, prompt string, projectTags []string, cfg config.AutoRAGSettings, skip map[string]bool, gate *memctx.PromptCheck, trace tracer) []*orchdv1.SearchHit {
	limit := cfg.TopK
	if limit <= 0 {
		limit = defaultTopK
	}
	// The daemon rejects larger requests, which would silently inject nothing.
	limit = min(limit, maxAutoRAGTopK)
	depth := limit
	if gate != nil {
		depth = min(limit*gateDepth, maxAutoRAGTopK)
	}
	if trace == nil {
		trace = func(*orchdv1.SearchHit, string) {}
	}
	var kept []*orchdv1.SearchHit
	// One hit per source: only a few are injected, and a second chunk of the
	// same document would take a slot another source could fill. The pointer
	// leads to the rest of it.
	seen := map[string]bool{}
	lines := map[string]bool{}
	exactOnly := 0
	topEvidence := 0
	lowFloorKept := 0 // at most one note gets in below the floor
	consider := func(h *orchdv1.SearchHit) string {
		key := h.GetSourceUri()
		switch {
		case seen[key]:
			return reasonSameSource
		case len(kept) >= limit:
			return reasonFull
		case skip[hitKey(h.GetSourceUri(), h.GetText())]:
			return reasonInjected
		case cfg.Untrusted == memctx.UntrustedExclude && memctx.TrustOf(h.GetTrust(), h.GetSourceType(), h.GetSourceUri()).Untrusted():
			return reasonUntrusted
		}
		how := relevance(h, cfg.MinScore)
		belowFloor := false
		if how == notRelevant && gate != nil && lowFloorKept == 0 && gate.PassesLowerFloor(h) {
			how, belowFloor = byMeaning, true
		}
		if how == notRelevant {
			return reasonBelowFloor
		}
		if how == byIdentifier && exactOnly >= maxExactOnly {
			return reasonIdentifier
		}
		if gate != nil {
			if why := gate.CheckHit(h); why != "" {
				return why
			}
			if !memctx.Curated(h) && !h.GetExact() && gate.Weak(h, topEvidence) {
				return memctx.ReasonWeakerThanTop
			}
			// The same text under several sources, such as a pasted brief, is one
			// piece of knowledge however many sessions repeated it.
			line := memctx.NormalLine(memctx.BestLine(gate.Query, h.GetText(), h.GetSourceType()))
			if lines[line] {
				return reasonDuplicate
			}
			lines[line] = true
		}
		if how == byIdentifier {
			exactOnly++
		}
		if belowFloor {
			lowFloorKept++
		}
		seen[key] = true
		kept = append(kept, h)
		return ""
	}
	take := func(req *orchdv1.SearchRequest) {
		resp, err := client.Search(ctx, req)
		if err != nil {
			trace(nil, err.Error())
			return
		}
		for _, h := range resp.GetHits() {
			trace(h, consider(h))
		}
	}
	project := &orchdv1.SearchRequest{Query: prompt, TopK: int32(depth), Tags: projectTags, PerSource: 1}
	global := &orchdv1.SearchRequest{Query: prompt, TopK: int32(depth), PerSource: 1}
	if gate != nil {
		evidence := map[*orchdv1.SearchHit]int{}
		for _, h := range gatedCandidates(ctx, client, gate, project, global, len(projectTags) > 0, evidence, trace) {
			why := consider(h)
			trace(h, why)
			// Everything after the first kept hit must hold up against it.
			if why == "" {
				topEvidence = max(topEvidence, evidence[h])
			}
		}
		return kept
	}
	if len(projectTags) > 0 {
		take(project)
	}
	if len(kept) < limit {
		take(global)
	}
	return kept
}

// gatedCandidates runs both searches and orders what they found: notes someone
// chose to remember first, then the project's hits, then the whole store's, each
// by evidence, and untrusted pages last. Notes carry no project tag, so a project pass that filled every
// slot would otherwise hide the decision a prompt most needs. Running the two
// together keeps that from costing a second search's time on the hook's path.
func gatedCandidates(ctx context.Context, client orchdv1.OrchdClient, gate *memctx.PromptCheck, project, global *orchdv1.SearchRequest, scoped bool, evidence map[*orchdv1.SearchHit]int, trace tracer) []*orchdv1.SearchHit {
	var hits [2][]*orchdv1.SearchHit
	var errs [2]error
	var wg sync.WaitGroup
	for i, req := range []*orchdv1.SearchRequest{project, global} {
		if i == 0 && !scoped {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Search(ctx, req)
			hits[i], errs[i] = resp.GetHits(), err
		}()
	}
	wg.Wait()
	var all []*orchdv1.SearchHit
	for i := range hits {
		if errs[i] != nil {
			trace(nil, errs[i].Error())
			continue
		}
		maps.Copy(evidence, gate.ByEvidence(hits[i]))
		all = append(all, hits[i]...)
	}
	sort.SliceStable(all, func(a, b int) bool { return memctx.Rank(all[a]) < memctx.Rank(all[b]) })
	return all
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

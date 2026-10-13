package routing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/willesq/orchestrator/internal/models"
)

// ErrNoRoute is returned when no routable model can serve a kind.
var ErrNoRoute = errors.New("no routable model")

// Router answers "which model for this kind" from the routing table, falling
// back to a live proposal when the table has nothing usable.
type Router struct {
	Store  *Store
	Models *models.Registry
	Policy Policy
}

// Recommendation is a route plus how much observed evidence stands behind it.
type Recommendation struct {
	Route
	Confidence string // low | medium | high
	// FromTable is false when the answer came from a live proposal because the
	// table had no usable route for the kind.
	FromTable bool
}

// confidence grades evidence. Until outcomes are recorded every route is a
// declared-capability opinion and must say so.
func confidence(n int) string {
	switch {
	case n >= 20:
		return "high"
	case n >= 5:
		return "medium"
	}
	return "low"
}

// Candidates returns the routable models of enabled providers.
func (r *Router) Candidates(ctx context.Context) ([]models.Model, error) {
	ps, err := r.Models.Providers(ctx)
	if err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, p := range ps {
		enabled[p.Name] = p.State == models.StateEnabled
	}
	ms, err := r.Models.Models(ctx, models.Filter{State: models.StateEnabled})
	if err != nil {
		return nil, err
	}
	var out []models.Model
	for _, m := range ms {
		if enabled[m.Provider] && m.Routable() {
			out = append(out, m)
		}
	}
	return out, nil
}

// Recommend picks the best usable route for kind. runnable, when non-nil,
// limits the answer to providers the caller can execute; a route to any other
// provider is skipped in favour of the next rank.
func (r *Router) Recommend(ctx context.Context, kind string, runnable func(provider string) bool) (Recommendation, error) {
	if !ValidKind(kind) {
		return Recommendation{}, fmt.Errorf("unknown task kind %q", kind)
	}
	cands, err := r.Candidates(ctx)
	if err != nil {
		return Recommendation{}, err
	}
	usable := map[string]models.Model{}
	for _, m := range cands {
		// The provider list is checked here as well as when proposing, so
		// removing a provider from routing.providers takes effect at once.
		if slices.Contains(r.Policy.Providers, m.Provider) && (runnable == nil || runnable(m.Provider)) {
			usable[m.Ref()] = m
		}
	}
	routes, err := r.Store.Kind(ctx, kind)
	if err != nil {
		return Recommendation{}, err
	}
	ev, err := r.Store.Evidence(ctx)
	if err != nil {
		return Recommendation{}, err
	}
	for _, rt := range routes {
		m, ok := usable[rt.Provider+"/"+rt.Model]
		if !ok {
			continue
		}
		if !m.SupportsEffort(rt.Effort) {
			rt.Effort = ""
		}
		// The count is current, not the one recorded when the table was applied.
		rt.EvidenceN = ev.For(kind, rt.Provider, rt.Model).N()
		return Recommendation{Route: rt, Confidence: confidence(rt.EvidenceN), FromTable: true}, nil
	}
	var pool []models.Model
	for _, m := range usable {
		pool = append(pool, m)
	}
	// The fallback ranks by declared capability alone: evidence reaches dispatch
	// only through a table the user applied.
	live := proposeKind(r.Policy, kind, pool, nil, time.Now().Unix())
	if len(live) == 0 {
		return Recommendation{}, fmt.Errorf("%w for task kind %q: enable one with `orch model enable`", ErrNoRoute, kind)
	}
	return Recommendation{Route: live[0], Confidence: confidence(0)}, nil
}

// Pin resolves an explicit "provider/slug" choice, holding it to the same gates
// as a routed one: the model must be enabled, not vendor-hidden, of an enabled
// provider, and runnable by the caller.
func (r *Router) Pin(ctx context.Context, ref string, runnable func(provider string) bool) (Recommendation, error) {
	provider, slug, err := models.ParseRef(ref)
	if err != nil {
		return Recommendation{}, err
	}
	cands, err := r.Candidates(ctx)
	if err != nil {
		return Recommendation{}, err
	}
	for _, m := range cands {
		if m.Provider != provider || m.Slug != slug {
			continue
		}
		if !slices.Contains(r.Policy.Providers, provider) {
			return Recommendation{}, fmt.Errorf("provider %s is not in routing.providers", provider)
		}
		if runnable != nil && !runnable(provider) {
			return Recommendation{}, fmt.Errorf("the queue worker cannot run %s models yet", provider)
		}
		return Recommendation{Route: Route{Provider: provider, Model: slug, Rationale: "chosen by the caller"}, Confidence: confidence(0)}, nil
	}
	return Recommendation{}, fmt.Errorf("model %s is not routable: enable it and its provider (`orch model list`, `orch provider list`)", ref)
}

// Package routing decides which model a dispatched task of a given kind runs
// on. It ranks the registry's routable models per task kind from a declared-
// capability policy, keeps the chosen table with history for rollback, and
// answers recommendations from it.
package routing

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/models"
)

// Kinds is the closed task-kind vocabulary. It is deliberately small: a larger
// one invites miscategorisation. Kind is orthogonal to the agent, since a dev
// agent writing docs should not route like one writing a parser.
var Kinds = []string{"code", "review", "docs", "research", "ops", "classify", "test", "adversarial"}

// ValidKind reports whether k is in the vocabulary.
func ValidKind(k string) bool { return slices.Contains(Kinds, k) }

// Policy is the declared-capability prior: which tier each kind wants, which
// providers may serve and in what order, and the effort to ask for where a
// model supports one.
type Policy struct {
	Tiers     map[string]string // kind → frontier | balanced | fast
	Providers []string          // preference order; unlisted providers are never proposed
	Efforts   map[string]string // kind → effort, applied only where the model supports it
	Ranks     int               // routes kept per kind
}

// Validate rejects a policy that names unknown kinds or tiers.
func (p Policy) Validate() error {
	for k, t := range p.Tiers {
		if !ValidKind(k) {
			return fmt.Errorf("routing.kinds: unknown task kind %q (want one of %s)", k, strings.Join(Kinds, ", "))
		}
		if tierOrder(t) < 0 {
			return fmt.Errorf("routing.kinds.%s: unknown tier %q", k, t)
		}
	}
	for k, e := range p.Efforts {
		if !ValidKind(k) {
			return fmt.Errorf("routing.efforts: unknown task kind %q", k)
		}
		if e != "" && !models.ValidEffort(e) {
			return fmt.Errorf("routing.efforts.%s: invalid effort %q", k, e)
		}
	}
	if p.Ranks < 1 {
		return fmt.Errorf("routing.ranks must be at least 1")
	}
	return nil
}

func tierOrder(t string) int {
	switch t {
	case models.TierFrontier:
		return 0
	case models.TierBalanced:
		return 1
	case models.TierFast:
		return 2
	}
	return -1
}

// tierDistance is how far a model's tier is from the wanted one; untiered
// models rank after every tiered one.
func tierDistance(want, have string) int {
	w, h := tierOrder(want), tierOrder(have)
	if w < 0 || h < 0 {
		return 3
	}
	if d := w - h; d >= 0 {
		return d
	}
	return h - w
}

// Route is one ranked choice for a task kind.
type Route struct {
	Kind      string
	Rank      int
	Provider  string
	Model     string
	Effort    string
	Rationale string
	EvidenceN int
	DecidedAt int64
	Pinned    bool
}

// Ref is "provider/model[@effort]".
func (r Route) Ref() string {
	s := r.Provider + "/" + r.Model
	if r.Effort != "" {
		s += "@" + r.Effort
	}
	return s
}

// Propose ranks candidates for every kind under the policy. candidates must
// already be routable models of enabled providers.
func Propose(p Policy, candidates []models.Model, now int64) []Route {
	var out []Route
	for _, k := range Kinds {
		out = append(out, proposeKind(p, k, candidates, now)...)
	}
	return out
}

func proposeKind(p Policy, kind string, candidates []models.Model, now int64) []Route {
	want := p.Tiers[kind]
	var pool []models.Model
	for _, m := range candidates {
		if slices.Contains(p.Providers, m.Provider) && m.Routable() {
			pool = append(pool, m)
		}
	}
	sort.SliceStable(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]
		if da, db := tierDistance(want, a.Tier), tierDistance(want, b.Tier); da != db {
			return da < db
		}
		if pa, pb := slices.Index(p.Providers, a.Provider), slices.Index(p.Providers, b.Provider); pa != pb {
			return pa < pb
		}
		// Aliases track each family's latest release, so prefer them to pins.
		if (a.Source == "alias") != (b.Source == "alias") {
			return a.Source == "alias"
		}
		return a.Slug < b.Slug
	})
	if len(pool) > p.Ranks {
		pool = pool[:p.Ranks]
	}
	routes := make([]Route, 0, len(pool))
	for i, m := range pool {
		effort := p.Efforts[kind]
		if len(m.SupportedEfforts) == 0 || !m.SupportsEffort(effort) {
			effort = ""
		}
		routes = append(routes, Route{
			Kind: kind, Rank: i + 1, Provider: m.Provider, Model: m.Slug, Effort: effort,
			Rationale: rationale(kind, want, m), DecidedAt: now,
		})
	}
	return routes
}

func rationale(kind, want string, m models.Model) string {
	have := m.Tier
	if have == "" {
		have = "untiered"
	}
	if want == "" {
		return fmt.Sprintf("declared %s; no tier policy for %s; no outcome evidence yet", have, kind)
	}
	return fmt.Sprintf("declared %s for %s (policy wants %s); no outcome evidence yet", have, kind, want)
}

// Change is one rank whose route differs between two tables.
type Change struct {
	Kind     string
	Rank     int
	From, To string // empty when the rank is added or dropped
}

// Diff lists the ranks that differ from current to proposed, in kind order.
func Diff(current, proposed []Route) []Change {
	key := func(r Route) string { return fmt.Sprintf("%s\x00%d", r.Kind, r.Rank) }
	cur := map[string]Route{}
	for _, r := range current {
		cur[key(r)] = r
	}
	prop := map[string]Route{}
	for _, r := range proposed {
		prop[key(r)] = r
	}
	var out []Change
	for _, k := range Kinds {
		for rank := 1; ; rank++ {
			c, okc := cur[key(Route{Kind: k, Rank: rank})]
			p, okp := prop[key(Route{Kind: k, Rank: rank})]
			if !okc && !okp {
				break
			}
			var from, to string
			if okc {
				from = c.Ref()
			}
			if okp {
				to = p.Ref()
			}
			if from != to {
				out = append(out, Change{Kind: k, Rank: rank, From: from, To: to})
			}
		}
	}
	return out
}

// PolicyFrom builds a Policy from the config's routing section.
func PolicyFrom(s config.RoutingSettings) Policy {
	return Policy{Tiers: s.Kinds, Providers: s.Providers, Efforts: s.Efforts, Ranks: s.Ranks}
}

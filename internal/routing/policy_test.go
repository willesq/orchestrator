package routing

import (
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/models"
)

func testPolicy() Policy {
	return Policy{
		Tiers:       map[string]string{"code": models.TierFrontier, "classify": models.TierFast, "docs": models.TierBalanced},
		Providers:   []string{"claude-code", "codex"},
		Efforts:     map[string]string{"code": "high"},
		Ranks:       3,
		MinEvidence: 5,
	}
}

func enabled(provider, slug, tier, source string, efforts ...string) models.Model {
	return models.Model{Provider: provider, Slug: slug, Tier: tier, Source: source, State: models.StateEnabled,
		Visibility: "list", SupportedEfforts: efforts}
}

func refs(rs []Route) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Ref())
	}
	return strings.Join(out, ",")
}

func TestProposeKind(t *testing.T) {
	cands := []models.Model{
		enabled("claude-code", "haiku", models.TierFast, "alias"),
		enabled("claude-code", "opus", models.TierFrontier, "alias"),
		enabled("claude-code", "sonnet", models.TierBalanced, "alias"),
		enabled("claude-code", "claude-opus-5-5", models.TierFrontier, "cache"),
		enabled("codex", "gpt-sol", models.TierFrontier, "cache", "low", "high"),
		enabled("codex", "gpt-luna", models.TierBalanced, "cache", "low"),
		enabled("claude-code", "mystery", "", "cache"),
		enabled("gemini", "g-pro", models.TierFrontier, "cache"), // provider not in policy
		{Provider: "codex", Slug: "hidden", Tier: models.TierFrontier, Source: "cache", State: models.StateEnabled, Visibility: "hide"},
		{Provider: "claude-code", Slug: "off", Tier: models.TierFrontier, Source: "alias", State: models.StateDisabled},
	}
	tests := []struct {
		kind  string
		ranks int
		want  string
	}{
		// Frontier first; claude-code before codex; alias before pinned id.
		{kind: "code", ranks: 3, want: "claude-code/opus,claude-code/claude-opus-5-5,codex/gpt-sol@high"},
		{kind: "classify", ranks: 2, want: "claude-code/haiku,claude-code/sonnet"},
		// Distance ties (balanced → frontier and fast are both 1) fall to provider order.
		{kind: "docs", ranks: 4, want: "claude-code/sonnet,codex/gpt-luna,claude-code/haiku,claude-code/opus"},
		// No tier policy: every tiered model is equally far, so provider and alias order decide.
		{kind: "ops", ranks: 2, want: "claude-code/haiku,claude-code/opus"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			p := testPolicy()
			p.Ranks = tt.ranks
			got := proposeKind(p, tt.kind, cands, nil, 7)
			if r := refs(got); r != tt.want {
				t.Fatalf("got %s, want %s", r, tt.want)
			}
			for i, r := range got {
				if r.Rank != i+1 || r.Kind != tt.kind || r.DecidedAt != 7 || r.Rationale == "" || r.EvidenceN != 0 {
					t.Errorf("route %d = %+v", i, r)
				}
			}
		})
	}
}

func TestProposeUntieredLast(t *testing.T) {
	p := testPolicy()
	got := proposeKind(p, "code", []models.Model{
		enabled("claude-code", "mystery", "", "alias"),
		enabled("codex", "gpt-luna", models.TierFast, "cache"),
	}, nil, 1)
	if r := refs(got); r != "codex/gpt-luna,claude-code/mystery" {
		t.Fatalf("got %s", r)
	}
	if !strings.Contains(got[1].Rationale, "untiered") {
		t.Errorf("rationale = %q", got[1].Rationale)
	}
}

func TestProposeAllKinds(t *testing.T) {
	got := Propose(testPolicy(), []models.Model{enabled("claude-code", "opus", models.TierFrontier, "alias")}, nil, 1)
	if len(got) != len(Kinds) {
		t.Fatalf("got %d routes, want one per kind", len(got))
	}
	if Propose(testPolicy(), nil, nil, 1) != nil {
		t.Fatal("no candidates should propose nothing")
	}
}

func TestPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Policy)
		wantErr string
	}{
		{name: "ok", mutate: func(*Policy) {}},
		{name: "unknown kind", mutate: func(p *Policy) { p.Tiers["poetry"] = models.TierFast }, wantErr: "unknown task kind"},
		{name: "unknown tier", mutate: func(p *Policy) { p.Tiers["code"] = "huge" }, wantErr: "unknown tier"},
		{name: "effort kind", mutate: func(p *Policy) { p.Efforts["poetry"] = "low" }, wantErr: "unknown task kind"},
		{name: "effort value", mutate: func(p *Policy) { p.Efforts["code"] = "--max" }, wantErr: "invalid effort"},
		{name: "ranks", mutate: func(p *Policy) { p.Ranks = 0 }, wantErr: "at least 1"},
		{name: "min evidence", mutate: func(p *Policy) { p.MinEvidence = 0 }, wantErr: "min_evidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPolicy()
			tt.mutate(&p)
			err := p.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestPolicyFromDefaults(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := PolicyFrom(s.Routing)
	if err := p.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	for _, k := range Kinds {
		if p.Tiers[k] == "" {
			t.Errorf("no default tier for %s", k)
		}
	}
	if p.Tiers["code"] != models.TierFrontier || p.Tiers["classify"] != models.TierFast || p.Ranks != 3 || p.MinEvidence != 5 {
		t.Errorf("defaults = %+v", p)
	}
}

func TestDiff(t *testing.T) {
	cur := []Route{
		{Kind: "code", Rank: 1, Provider: "claude-code", Model: "opus"},
		{Kind: "code", Rank: 2, Provider: "claude-code", Model: "sonnet"},
		{Kind: "docs", Rank: 1, Provider: "claude-code", Model: "sonnet"},
	}
	prop := []Route{
		{Kind: "code", Rank: 1, Provider: "claude-code", Model: "opus"},
		{Kind: "docs", Rank: 1, Provider: "codex", Model: "gpt-luna", Effort: "low"},
		{Kind: "test", Rank: 1, Provider: "claude-code", Model: "sonnet"},
	}
	got := Diff(cur, prop)
	want := []Change{
		{Kind: "code", Rank: 2, From: "claude-code/sonnet"},
		{Kind: "docs", Rank: 1, From: "claude-code/sonnet", To: "codex/gpt-luna@low"},
		{Kind: "test", Rank: 1, To: "claude-code/sonnet"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(Diff(cur, cur)) != 0 {
		t.Fatal("identical tables should not differ")
	}
}

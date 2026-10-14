package models

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewRegistry(conn)
}

func providerState(t *testing.T, reg *Registry, name string) string {
	t.Helper()
	ps, err := reg.Providers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == name {
			return p.State
		}
	}
	return ""
}

func TestRecordDiscoveryProviderState(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		steps   []string // discovered availability per refresh: "ok" or "gone"
		userSet string   // applied after the first refresh, if set
		want    string
	}{
		{name: "first sight takes initial", initial: StateDisabled, steps: []string{"ok"}, want: StateDisabled},
		{name: "first sight unavailable", initial: StateEnabled, steps: []string{"gone"}, want: StateUnavailable},
		{name: "user enable survives refresh", initial: StateDisabled, steps: []string{"ok", "ok"}, userSet: StateEnabled, want: StateEnabled},
		{name: "user disable survives refresh", initial: StateEnabled, steps: []string{"ok", "ok"}, userSet: StateDisabled, want: StateDisabled},
		{name: "goes unavailable", initial: StateEnabled, steps: []string{"ok", "gone"}, want: StateUnavailable},
		{name: "user enable survives unavailability", initial: StateDisabled, steps: []string{"ok", "gone", "ok"}, userSet: StateEnabled, want: StateEnabled},
		{name: "user disable survives unavailability", initial: StateEnabled, steps: []string{"ok", "gone", "gone", "ok"}, userSet: StateDisabled, want: StateDisabled},
		{name: "first seen unavailable comes back initial", initial: StateDisabled, steps: []string{"gone", "ok"}, want: StateDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx := context.Background()
			for i, step := range tt.steps {
				p := Provider{Name: "codex", State: StateEnabled}
				if step == "gone" {
					p = Provider{Name: "codex", State: StateUnavailable, StateReason: "codex not found"}
				}
				if err := reg.RecordDiscovery(ctx, p, tt.initial, nil, int64(i+1)); err != nil {
					t.Fatal(err)
				}
				if i == 0 && tt.userSet != "" {
					if err := reg.SetProviderState(ctx, "codex", tt.userSet); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := providerState(t, reg, "codex"); got != tt.want {
				t.Fatalf("state = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecordDiscoveryPreservesUserChoices(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	p := Provider{Name: "codex", State: StateEnabled}
	first := []Model{{Slug: "m1", DisplayName: "One", Source: "cache", SupportedEfforts: []string{"low", "high"}, DefaultEffort: "low"}}
	if err := reg.RecordDiscovery(ctx, p, StateEnabled, first, 1); err != nil {
		t.Fatal(err)
	}
	m, err := reg.Get(ctx, "codex", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != StateDiscovered || m.Routable() {
		t.Fatalf("new model = %+v, want discovered and not routable", m)
	}
	if err := reg.SetModelState(ctx, "codex", "m1", StateEnabled); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetTier(ctx, "codex", "m1", TierFast); err != nil {
		t.Fatal(err)
	}
	second := []Model{{Slug: "m1", DisplayName: "One renamed", Tier: TierFrontier, Source: "cache", SupportedEfforts: []string{"medium"}, DefaultEffort: "medium"}}
	if err := reg.RecordDiscovery(ctx, p, StateEnabled, second, 2); err != nil {
		t.Fatal(err)
	}
	m, err = reg.Get(ctx, "codex", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != StateEnabled || m.Tier != TierFast {
		t.Fatalf("user choices lost: state=%q tier=%q", m.State, m.Tier)
	}
	if m.DisplayName != "One renamed" || m.DefaultEffort != "medium" || strings.Join(m.SupportedEfforts, ",") != "medium" || m.LastVerified != 2 {
		t.Fatalf("metadata not refreshed: %+v", m)
	}
	if !m.Routable() {
		t.Fatal("enabled listed model should be routable")
	}
}

func TestSetModelState(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		state   string
		wantErr string
	}{
		{name: "enable listed", slug: "shown", state: StateEnabled},
		{name: "disable hidden", slug: "hidden", state: StateDisabled},
		{name: "enable hidden refused", slug: "hidden", state: StateEnabled, wantErr: "hidden by its vendor"},
		{name: "bad state", slug: "shown", state: StateDiscovered, wantErr: "must be"},
		{name: "unknown model", slug: "nope", state: StateEnabled, wantErr: "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			ctx := context.Background()
			ms := []Model{{Slug: "shown", Source: "cache"}, {Slug: "hidden", Visibility: "hide", Source: "cache"}}
			if err := reg.RecordDiscovery(ctx, Provider{Name: "codex", State: StateEnabled}, StateEnabled, ms, 1); err != nil {
				t.Fatal(err)
			}
			err := reg.SetModelState(ctx, "codex", tt.slug, tt.state)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				m, _ := reg.Get(ctx, "codex", tt.slug)
				if m.State != tt.state {
					t.Fatalf("state = %q, want %q", m.State, tt.state)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestSetProviderStateErrors(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	if err := reg.SetProviderState(ctx, "codex", StateEnabled); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown provider err = %v", err)
	}
	if err := reg.RecordDiscovery(ctx, Provider{Name: "codex", State: StateUnavailable, StateReason: "codex not found"}, StateDisabled, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetProviderState(ctx, "codex", StateEnabled); err == nil || !strings.Contains(err.Error(), "codex not found") {
		t.Fatalf("enable unavailable err = %v", err)
	}
	if err := reg.SetProviderState(ctx, "codex", StateDisabled); err != nil {
		t.Fatalf("disable unavailable: %v", err)
	}
	if err := reg.SetProviderState(ctx, "codex", StateUnavailable); err == nil {
		t.Fatal("setting unavailable by hand should fail")
	}
}

func TestSetTierErrors(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	if err := reg.SetTier(ctx, "codex", "m1", "huge"); err == nil {
		t.Fatal("bad tier accepted")
	}
	if err := reg.SetTier(ctx, "codex", "m1", TierFast); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown model err = %v", err)
	}
}

func TestModelsFilter(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	_ = reg.RecordDiscovery(ctx, Provider{Name: "codex", State: StateEnabled}, StateEnabled, []Model{{Slug: "a", Source: "cache"}}, 1)
	_ = reg.RecordDiscovery(ctx, Provider{Name: "claude-code", State: StateEnabled}, StateEnabled,
		[]Model{{Slug: "opus", Source: "alias", State: StateEnabled}, {Slug: "x", Source: "cache"}}, 1)
	tests := []struct {
		f    Filter
		want string
	}{
		{Filter{}, "claude-code/opus,claude-code/x,codex/a"},
		{Filter{Provider: "codex"}, "codex/a"},
		{Filter{State: StateEnabled}, "claude-code/opus"},
		{Filter{Provider: "codex", State: StateEnabled}, ""},
	}
	for _, tt := range tests {
		ms, err := reg.Models(ctx, tt.f)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, m := range ms {
			refs = append(refs, m.Ref())
		}
		if got := strings.Join(refs, ","); got != tt.want {
			t.Errorf("%+v = %q, want %q", tt.f, got, tt.want)
		}
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		in, provider, slug string
		ok                 bool
	}{
		{"codex/gpt-6.1-sol", "codex", "gpt-6.1-sol", true},
		{"claude-code/claude-fable-5-1[1m]", "claude-code", "claude-fable-5-1[1m]", true},
		{"codex", "", "", false},
		{"/x", "", "", false},
		{"codex/", "", "", false},
		{"a/b/c", "", "", false},
	}
	for _, tt := range tests {
		p, s, err := ParseRef(tt.in)
		if (err == nil) != tt.ok || p != tt.provider || s != tt.slug {
			t.Errorf("ParseRef(%q) = %q, %q, %v", tt.in, p, s, err)
		}
	}
}

func TestSupportsEffort(t *testing.T) {
	m := Model{SupportedEfforts: []string{"low", "high"}}
	for effort, want := range map[string]bool{"": true, "low": true, "high": true, "max": false} {
		if got := m.SupportsEffort(effort); got != want {
			t.Errorf("SupportsEffort(%q) = %v", effort, got)
		}
	}
}

func TestRecordDiscoveryEnforcesInvariants(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	ms := []Model{
		{Slug: "cached", Source: "cache", State: StateEnabled}, // an adapter cannot enable a discovered model
		{Slug: "opus", Source: "alias", State: StateEnabled},
		{Slug: "-p", Source: "cache"},
		{Slug: "badeffort", Source: "cache", DefaultEffort: "LOW;rm", SupportedEfforts: []string{"low", "x,y"}},
		{Slug: "efforts", Source: "cache", SupportedEfforts: []string{"low", "x,y", "high"}},
		{Slug: "odd-visibility", Source: "cache", Visibility: "public"},
	}
	if err := reg.RecordDiscovery(ctx, Provider{Name: "codex", State: StateEnabled}, StateEnabled, ms, 1); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Models(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byslug := map[string]Model{}
	for _, m := range got {
		byslug[m.Slug] = m
	}
	if len(byslug) != 4 || byslug["-p"].Slug != "" || byslug["badeffort"].Slug != "" {
		t.Fatalf("invalid rows stored: %v", got)
	}
	if byslug["cached"].State != StateDiscovered || byslug["opus"].State != StateEnabled {
		t.Fatalf("states = %q/%q", byslug["cached"].State, byslug["opus"].State)
	}
	if e := strings.Join(byslug["efforts"].SupportedEfforts, ","); e != "low,high" {
		t.Fatalf("efforts = %q", e)
	}
	if byslug["odd-visibility"].Visibility != "list" {
		t.Fatalf("visibility = %q", byslug["odd-visibility"].Visibility)
	}
}

func TestRecordDiscoveryCapsModels(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	ms := make([]Model, maxModelsPerProvider+10)
	for i := range ms {
		ms[i] = Model{Slug: fmt.Sprintf("m%d", i), Source: "cache"}
	}
	if err := reg.RecordDiscovery(ctx, Provider{Name: "codex", State: StateEnabled}, StateEnabled, ms, 1); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Models(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxModelsPerProvider {
		t.Fatalf("stored %d models, want cap %d", len(got), maxModelsPerProvider)
	}
}

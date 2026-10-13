package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/models"
)

type fakeAdapter struct {
	name, initial string
	p             models.Provider
	ms            []models.Model
}

func (f fakeAdapter) Name() string         { return f.name }
func (f fakeAdapter) InitialState() string { return f.initial }
func (f fakeAdapter) Discover(string) (models.Provider, []models.Model, error) {
	return f.p, f.ms, nil
}

func useModelAdapters(t *testing.T, as ...models.Adapter) {
	t.Helper()
	orig := modelAdapters
	t.Cleanup(func() { modelAdapters = orig })
	modelAdapters = func() []models.Adapter { return as }
}

func TestModelRegistryCommands(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	useModelAdapters(t,
		fakeAdapter{name: "claude-code", initial: models.StateEnabled, p: models.Provider{State: models.StateEnabled},
			ms: []models.Model{{Slug: "opus", Tier: models.TierFrontier, Source: "alias", State: models.StateEnabled}}},
		fakeAdapter{name: "codex", initial: models.StateDisabled, p: models.Provider{State: models.StateEnabled},
			ms: []models.Model{
				{Slug: "gpt-6.1-sol", Source: "cache", SupportedEfforts: []string{"low", "high"}, DefaultEffort: "low"},
				{Slug: "gpt-reserve", Source: "cache", Visibility: "hide"},
			}},
	)

	steps := []struct {
		args    []string
		want    []string
		wantErr string
	}{
		{args: []string{"model", "list"}, want: []string{"no models"}},
		{args: []string{"provider", "list"}, want: []string{"no providers"}},
		{args: []string{"model", "refresh"}, want: []string{"new: codex/gpt-6.1-sol (discovered)", "3 models, 3 new", "stay inert"}},
		{args: []string{"model", "refresh"}, want: []string{"3 models, 0 new"}},
		{args: []string{"model", "list"}, want: []string{"claude-code/opus", "frontier", "low*,high", "(hidden)", "discovered"}},
		{args: []string{"model", "list", "--state", "enabled"}, want: []string{"claude-code/opus"}},
		{args: []string{"model", "enable", "codex/gpt-6.1-sol"}, want: []string{"codex/gpt-6.1-sol enabled"}},
		{args: []string{"model", "enable", "codex/gpt-reserve"}, wantErr: "hidden by its vendor"},
		{args: []string{"model", "disable", "codex/gpt-6.1-sol"}, want: []string{"disabled"}},
		{args: []string{"model", "enable", "codex"}, wantErr: "<provider>/<slug>"},
		{args: []string{"model", "tier", "codex/gpt-6.1-sol", "fast"}, want: []string{"tier fast"}},
		{args: []string{"model", "tier", "codex/gpt-6.1-sol", "huge"}, wantErr: "tier must be"},
		{args: []string{"model", "tier", "nope", "fast"}, wantErr: "<provider>/<slug>"},
		{args: []string{"provider", "list"}, want: []string{"claude-code", "codex", "disabled"}},
		{args: []string{"provider", "enable", "codex"}, want: []string{"codex enabled"}},
		{args: []string{"provider", "disable", "gemini"}, wantErr: "not found"},
		{args: []string{"model", "disable", "codex/none"}, wantErr: "not found"},
	}
	for _, s := range steps {
		out, err := run(nil, "", s.args...)
		name := strings.Join(s.args, " ")
		if s.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), s.wantErr) {
				t.Fatalf("%s: err = %v, want %q", name, err, s.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, w := range s.want {
			if !strings.Contains(out, w) {
				t.Fatalf("%s: output missing %q:\n%s", name, w, out)
			}
		}
	}
}

func TestDefaultModelAdapters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	var names []string
	for _, a := range modelAdapters() {
		names = append(names, a.Name())
	}
	if got := strings.Join(names, ","); got != "claude-code,codex" {
		t.Fatalf("adapters = %q", got)
	}
}

func TestAge(t *testing.T) {
	if age(0) != "never" {
		t.Fatal("zero time should read never")
	}
	if !strings.HasSuffix(age(1), " ago") {
		t.Fatal("past time should read ago")
	}
}

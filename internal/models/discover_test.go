package models

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeLookPath makes only the named CLIs resolvable.
func fakeLookPath(t *testing.T, present ...string) {
	t.Helper()
	orig := LookPath
	t.Cleanup(func() { LookPath = orig })
	LookPath = func(name string) (string, error) {
		for _, p := range present {
			if p == name {
				return "/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const codexCache = `{
  "client_version": "0.160.0",
  "identity": {"account": "SECRET-ACCOUNT-ID", "token": "SECRET-TOKEN"},
  "models": [
    {"slug": "gpt-6.1-sol", "display_name": "GPT-6.1-Sol", "description": "Workhorse\nmodel.",
     "default_reasoning_level": "low",
     "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}, {"effort": "BAD EFFORT"}],
     "visibility": "list"},
    {"slug": "gpt-reserve", "display_name": "Reserve", "default_reasoning_level": "max",
     "supported_reasoning_levels": [{"effort": "medium"}], "visibility": "hide"},
    {"slug": "--model-injection", "display_name": "bad"},
    {"slug": "", "display_name": "empty"}
  ]
}`

const claudeJSON = `{
  "oauthAccount": {"emailAddress": "SECRET-EMAIL", "accessToken": "SECRET-TOKEN"},
  "additionalModelOptionsCache": [
    {"value": "claude-fable-5-1[1m]", "label": "Fable", "description": "1M context"},
    {"value": "-p", "label": "flag-looking"}
  ]
}`

func TestCodexDiscover(t *testing.T) {
	tests := []struct {
		name      string
		present   []string
		cache     string
		wantState string
		wantSlugs string
		wantErr   bool
	}{
		{name: "parses cache", present: []string{"codex"}, cache: codexCache, wantState: StateEnabled, wantSlugs: "gpt-6.1-sol,gpt-reserve"},
		{name: "no cli", cache: codexCache, wantState: StateUnavailable},
		{name: "no cache", present: []string{"codex"}, wantState: StateEnabled},
		{name: "corrupt cache", present: []string{"codex"}, cache: `{"models": [`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeLookPath(t, tt.present...)
			home := t.TempDir()
			if tt.cache != "" {
				writeFile(t, filepath.Join(home, ".codex", "models_cache.json"), tt.cache)
			}
			p, ms, err := codexAdapter{}.Discover(home)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				if strings.Contains(err.Error(), "models") && strings.Contains(err.Error(), "[") {
					t.Fatalf("error quotes file content: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.State != tt.wantState {
				t.Fatalf("state = %q, want %q", p.State, tt.wantState)
			}
			var slugs []string
			for _, m := range ms {
				slugs = append(slugs, m.Slug)
			}
			if got := strings.Join(slugs, ","); got != tt.wantSlugs {
				t.Fatalf("slugs = %q, want %q", got, tt.wantSlugs)
			}
			if tt.wantSlugs == "" {
				return
			}
			sol, reserve := ms[0], ms[1]
			if sol.Description != "Workhorse model." || sol.DefaultEffort != "low" || strings.Join(sol.SupportedEfforts, ",") != "low,high" {
				t.Errorf("sol = %+v", sol)
			}
			if sol.State != "" || sol.Visibility != "list" {
				t.Errorf("sol state/visibility = %q/%q; discovery must not set a state", sol.State, sol.Visibility)
			}
			// gpt-reserve declares a default it does not support: drop it.
			if reserve.Visibility != "hide" || reserve.DefaultEffort != "" {
				t.Errorf("reserve = %+v", reserve)
			}
		})
	}
}

func TestClaudeDiscover(t *testing.T) {
	tests := []struct {
		name      string
		present   []string
		bin       string
		claude    string
		wantState string
		wantSlugs string
	}{
		{name: "aliases plus cache", present: []string{"claude"}, claude: claudeJSON, wantState: StateEnabled,
			wantSlugs: "opus,sonnet,haiku,fable,claude-fable-5-1[1m]"},
		{name: "aliases without claude.json", present: []string{"claude"}, wantState: StateEnabled, wantSlugs: "opus,sonnet,haiku,fable"},
		{name: "configured bin", present: []string{"/opt/claude"}, bin: "/opt/claude", wantState: StateEnabled, wantSlugs: "opus,sonnet,haiku,fable"},
		{name: "no cli", wantState: StateUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeLookPath(t, tt.present...)
			home := t.TempDir()
			if tt.claude != "" {
				writeFile(t, filepath.Join(home, ".claude.json"), tt.claude)
			}
			p, ms, err := claudeAdapter{bin: tt.bin}.Discover(home)
			if err != nil {
				t.Fatal(err)
			}
			if p.State != tt.wantState {
				t.Fatalf("state = %q, want %q", p.State, tt.wantState)
			}
			var slugs []string
			for _, m := range ms {
				slugs = append(slugs, m.Slug)
				wantState := StateEnabled
				if m.Source == "cache" {
					wantState = ""
				}
				if m.State != wantState {
					t.Errorf("%s state = %q, want %q", m.Slug, m.State, wantState)
				}
			}
			if got := strings.Join(slugs, ","); got != tt.wantSlugs {
				t.Fatalf("slugs = %q, want %q", got, tt.wantSlugs)
			}
		})
	}
}

func TestRefreshRecordsWithoutLeaking(t *testing.T) {
	fakeLookPath(t, "claude", "codex")
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex", "models_cache.json"), codexCache)
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSON)
	reg := testRegistry(t)
	ctx := context.Background()
	if err := Refresh(ctx, reg, home, Adapters("")); err != nil {
		t.Fatal(err)
	}
	if got := providerState(t, reg, ProviderClaude); got != StateEnabled {
		t.Errorf("claude-code = %q", got)
	}
	if got := providerState(t, reg, ProviderCodex); got != StateDisabled {
		t.Errorf("codex = %q, want disabled until the user enables it", got)
	}
	ms, err := reg.Models(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var enabled []string
	for _, m := range ms {
		for _, field := range []string{m.Slug, m.DisplayName, m.Description} {
			if strings.Contains(field, "SECRET") {
				t.Fatalf("account data leaked into registry: %+v", m)
			}
		}
		if m.State == StateEnabled {
			enabled = append(enabled, m.Ref())
		}
	}
	want := "claude-code/fable,claude-code/haiku,claude-code/opus,claude-code/sonnet"
	if got := strings.Join(enabled, ","); got != want {
		t.Fatalf("enabled = %q, want only compiled-in aliases %q", got, want)
	}
	// A second refresh is idempotent.
	if err := Refresh(ctx, reg, home, Adapters("")); err != nil {
		t.Fatal(err)
	}
	again, _ := reg.Models(ctx, Filter{})
	if len(again) != len(ms) {
		t.Fatalf("refresh not idempotent: %d then %d", len(ms), len(again))
	}
}

func TestRefreshContinuesPastFailure(t *testing.T) {
	fakeLookPath(t, "claude", "codex")
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex", "models_cache.json"), "not json")
	reg := testRegistry(t)
	err := Refresh(context.Background(), reg, home, Adapters(""))
	if err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("err = %v, want codex failure", err)
	}
	if got := providerState(t, reg, ProviderClaude); got != StateEnabled {
		t.Fatalf("claude-code not recorded after codex failure: %q", got)
	}
}

func TestClean(t *testing.T) {
	if got := clean("  a\x1b[31mb\tc  "); got != "a [31mb c" {
		t.Errorf("clean = %q", got)
	}
	if got := clean("a\u009b31mb\u202ec\u200bd"); got != "a 31mb c d" {
		t.Errorf("clean C1/bidi/zero-width = %q", got)
	}
	if got := len([]rune(clean(strings.Repeat("é", 500)))); got != 200 {
		t.Errorf("clean length = %d", got)
	}
}

func TestReadJSONRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var v any
	if _, err := readJSON(path, &v); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	if _, err := readJSON(dir, &v); err == nil {
		t.Fatal("directory accepted")
	}
}

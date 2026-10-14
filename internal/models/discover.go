package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Built-in provider names.
const (
	ProviderClaude = "claude-code"
	ProviderCodex  = "codex"
)

// maxCacheBytes caps how much of a harness cache file discovery reads.
// ~/.claude.json grows with project history; the keys we need are small.
const maxCacheBytes = 64 << 20

// Adapter discovers one provider's models from its CLI's local cache. Adapters
// read only the model fields they need: the caches sit beside account state that
// must never be copied into the registry.
type Adapter interface {
	Name() string
	// InitialState is the provider's state the first time discovery sees it.
	InitialState() string
	Discover(home string) (Provider, []Model, error)
}

// LookPath resolves a CLI on PATH; a seam for tests.
var LookPath = exec.LookPath

// Adapters returns the built-in adapters. claudeBin is the configured worker
// binary (empty means `claude` on PATH).
func Adapters(claudeBin string) []Adapter {
	return []Adapter{claudeAdapter{bin: claudeBin}, codexAdapter{}}
}

// Refresh runs every adapter and records the results. One adapter failing does
// not stop the others; their errors are joined.
func Refresh(ctx context.Context, reg *Registry, home string, adapters []Adapter) error {
	var errs []error
	now := time.Now().Unix()
	for _, a := range adapters {
		p, ms, err := a.Discover(home)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
			continue
		}
		p.Name = a.Name()
		if err := reg.RecordDiscovery(ctx, p, a.InitialState(), ms, now); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// claudeAliases are Claude Code's own model aliases. They resolve to the latest
// model of each family inside the CLI, so routing to them tracks releases with
// no discovery. They are compiled in rather than discovered, which is why they
// start enabled: no vendor change can add one.
var claudeAliases = []Model{
	{Slug: "opus", DisplayName: "Opus (latest)", Tier: TierFrontier},
	{Slug: "sonnet", DisplayName: "Sonnet (latest)", Tier: TierBalanced},
	{Slug: "haiku", DisplayName: "Haiku (latest)", Tier: TierFast},
	{Slug: "fable", DisplayName: "Fable (latest)"},
}

type claudeAdapter struct{ bin string }

func (claudeAdapter) Name() string { return ProviderClaude }

// The worker already runs Claude Code, so it starts enabled.
func (claudeAdapter) InitialState() string { return StateEnabled }

func (a claudeAdapter) Discover(home string) (Provider, []Model, error) {
	bin := a.bin
	if bin == "" {
		bin = "claude"
	}
	if _, err := LookPath(bin); err != nil {
		return Provider{State: StateUnavailable, StateReason: bin + " not found on PATH"}, nil, nil
	}
	ms := make([]Model, 0, len(claudeAliases))
	for _, m := range claudeAliases {
		m.Source, m.State = "alias", StateEnabled
		ms = append(ms, m)
	}

	var cfg struct {
		Options []struct {
			Value       string `json:"value"`
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"additionalModelOptionsCache"`
	}
	found, err := readJSON(filepath.Join(home, ".claude.json"), &cfg)
	if err != nil {
		return Provider{}, nil, err
	}
	if found {
		for _, o := range cfg.Options {
			if !validSlug(o.Value) {
				continue
			}
			ms = append(ms, Model{Slug: o.Value, DisplayName: clean(o.Label), Description: clean(o.Description), Source: "cache"})
		}
	}
	return Provider{State: StateEnabled}, ms, nil
}

type codexAdapter struct{}

func (codexAdapter) Name() string { return ProviderCodex }

// The worker cannot run Codex yet, and enabling another vendor is the user's
// call, so it starts disabled.
func (codexAdapter) InitialState() string { return StateDisabled }

func (codexAdapter) Discover(home string) (Provider, []Model, error) {
	if _, err := LookPath("codex"); err != nil {
		return Provider{State: StateUnavailable, StateReason: "codex not found on PATH"}, nil, nil
	}
	var cache struct {
		Models []struct {
			Slug          string `json:"slug"`
			DisplayName   string `json:"display_name"`
			Description   string `json:"description"`
			DefaultEffort string `json:"default_reasoning_level"`
			Efforts       []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	path := filepath.Join(home, ".codex", "models_cache.json")
	found, err := readJSON(path, &cache)
	if err != nil {
		return Provider{}, nil, err
	}
	if !found {
		return Provider{State: StateEnabled, StateReason: "no model cache yet; run codex once"}, nil, nil
	}
	var ms []Model
	for _, c := range cache.Models {
		if !validSlug(c.Slug) {
			continue
		}
		m := Model{
			Slug: c.Slug, DisplayName: clean(c.DisplayName), Description: clean(c.Description),
			Visibility: "list", Source: "cache",
		}
		if c.Visibility == "hide" {
			m.Visibility = "hide"
		}
		for _, e := range c.Efforts {
			if validEffort(e.Effort) {
				m.SupportedEfforts = append(m.SupportedEfforts, e.Effort)
			}
		}
		if m.SupportsEffort(c.DefaultEffort) && validEffort(c.DefaultEffort) {
			m.DefaultEffort = c.DefaultEffort
		}
		ms = append(ms, m)
	}
	return Provider{State: StateEnabled}, ms, nil
}

// readJSON decodes path into v, reporting found=false when it does not exist.
func readJSON(path string, v any) (found bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A FIFO or device here would hang or stream forever.
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) // #nosec G304 -- fixed harness cache paths under home.
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	// The error names the file only: a decode error can quote file content.
	if err := json.NewDecoder(io.LimitReader(f, maxCacheBytes)).Decode(v); err != nil {
		return false, fmt.Errorf("parse %s: invalid JSON", path)
	}
	return true, nil
}

// Slugs become a CLI argument (`--model <slug>`), so a slug that could read as a
// flag or carry shell-hostile bytes is dropped rather than stored.
var (
	slugRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$`)
	effortRe = regexp.MustCompile(`^[a-z]{1,16}$`)
)

func validSlug(s string) bool { return slugRe.MatchString(s) }

// ValidSlug reports whether s is safe to pass as a model argument. Callers that
// build a command line re-check it, since a row may predate this validation.
func ValidSlug(s string) bool { return validSlug(s) }

// ValidEffort reports whether s is a well-formed effort level.
func ValidEffort(s string) bool { return validEffort(s) }
func validEffort(s string) bool { return effortRe.MatchString(s) }

// clean strips control and format characters (C0, C1, bidi overrides,
// zero-width) and caps length for display fields that come from a vendor cache
// and end up in terminals and prompts.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

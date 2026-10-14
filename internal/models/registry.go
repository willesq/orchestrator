// Package models keeps the registry of models each coding-agent CLI can run, as
// discovered from that CLI's local cache, and the provider and model gates that
// decide which of them routing may use.
package models

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Provider and model states.
const (
	StateEnabled     = "enabled"
	StateDisabled    = "disabled"
	StateUnavailable = "unavailable"
	StateDiscovered  = "discovered"
)

// Model tiers: a coarse declared-capability prior, not a verdict.
const (
	TierFrontier = "frontier"
	TierBalanced = "balanced"
	TierFast     = "fast"
)

// Provider is one coding-agent CLI.
type Provider struct {
	Name          string
	State         string
	StateReason   string
	LastRefreshed int64
}

// Model is one model a provider's CLI accepts.
type Model struct {
	Provider         string
	Slug             string
	DisplayName      string
	Description      string
	Tier             string
	DefaultEffort    string
	SupportedEfforts []string
	Visibility       string // list | hide
	Source           string // alias | cache
	LastVerified     int64
	State            string
}

// Ref is "provider/slug".
func (m Model) Ref() string { return m.Provider + "/" + m.Slug }

// Routable reports whether routing may send work to the model.
func (m Model) Routable() bool { return m.State == StateEnabled && m.Visibility != "hide" }

// SupportsEffort reports whether effort is valid for the model. An empty effort
// always is: it means the model's default.
func (m Model) SupportsEffort(effort string) bool {
	if effort == "" {
		return true
	}
	for _, e := range m.SupportedEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

// ErrNotFound is returned for an unknown provider or model.
var ErrNotFound = errors.New("not found")

// Registry reads and writes the registry tables.
type Registry struct{ db *sql.DB }

// NewRegistry wraps an open orch.db.
func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

// maxModelsPerProvider bounds what one discovery may write; a real cache lists
// a handful, so anything near this is a corrupt or hostile file.
const maxModelsPerProvider = 500

// RecordDiscovery stores one provider's discovery result. A provider seen for
// the first time takes initial; afterwards only an availability change touches
// its state, so a user's enable or disable stands, including across a spell of
// unavailability. New models start discovered, except compiled-in aliases,
// which may start enabled; existing ones keep whatever state and tier the user
// gave them while their metadata is refreshed. Invalid slugs and efforts are
// dropped here as well as in the adapters, since a slug becomes a CLI argument.
func (r *Registry) RecordDiscovery(ctx context.Context, p Provider, initial string, ms []Model, now int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current, prior string
	err = tx.QueryRowContext(ctx, `SELECT state, prior_state FROM providers WHERE name = ?`, p.Name).Scan(&current, &prior)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		state := initial
		if p.State == StateUnavailable {
			state = StateUnavailable
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO providers (name, state, state_reason, last_refreshed) VALUES (?, ?, ?, ?)`,
			p.Name, state, p.StateReason, now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		state := current
		switch {
		case p.State == StateUnavailable:
			if current != StateUnavailable {
				prior = current
			}
			state = StateUnavailable
		case current == StateUnavailable:
			state = orDefault(prior, initial)
			prior = ""
		}
		if _, err := tx.ExecContext(ctx, `UPDATE providers SET state = ?, prior_state = ?, state_reason = ?, last_refreshed = ? WHERE name = ?`,
			state, prior, p.StateReason, now, p.Name); err != nil {
			return err
		}
	}

	if len(ms) > maxModelsPerProvider {
		ms = ms[:maxModelsPerProvider]
	}
	for _, m := range ms {
		if !validSlug(m.Slug) || (m.DefaultEffort != "" && !validEffort(m.DefaultEffort)) {
			continue
		}
		var efforts []string
		for _, e := range m.SupportedEfforts {
			if validEffort(e) {
				efforts = append(efforts, e)
			}
		}
		state := StateDiscovered
		if m.Source == "alias" && m.State == StateEnabled {
			state = StateEnabled
		}
		visibility := "list"
		if m.Visibility == "hide" {
			visibility = "hide"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO models (provider, slug, display_name, description, tier, default_effort,
			                    supported_efforts, visibility, source, last_verified, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(provider, slug) DO UPDATE SET
			  display_name = excluded.display_name, description = excluded.description,
			  tier = CASE WHEN models.tier = '' THEN excluded.tier ELSE models.tier END,
			  default_effort = excluded.default_effort, supported_efforts = excluded.supported_efforts,
			  visibility = excluded.visibility, source = excluded.source,
			  last_verified = excluded.last_verified`,
			p.Name, m.Slug, m.DisplayName, m.Description, m.Tier, m.DefaultEffort,
			strings.Join(efforts, ","), visibility, m.Source, now, state); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Providers lists every provider, by name.
func (r *Registry) Providers(ctx context.Context) ([]Provider, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, state, state_reason, last_refreshed FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Name, &p.State, &p.StateReason, &p.LastRefreshed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProviderState enables or disables a provider. An unavailable one cannot be
// enabled until discovery finds its CLI again.
func (r *Registry) SetProviderState(ctx context.Context, name, state string) error {
	if state != StateEnabled && state != StateDisabled {
		return fmt.Errorf("provider state must be %s or %s", StateEnabled, StateDisabled)
	}
	var current, reason string
	err := r.db.QueryRowContext(ctx, `SELECT state, state_reason FROM providers WHERE name = ?`, name).Scan(&current, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("provider %q: %w (run `orch model refresh` first)", name, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if current == StateUnavailable && state == StateEnabled {
		return fmt.Errorf("provider %q is unavailable: %s", name, reason)
	}
	_, err = r.db.ExecContext(ctx, `UPDATE providers SET state = ? WHERE name = ?`, state, name)
	return err
}

// Filter narrows a model listing; empty fields match everything.
type Filter struct {
	Provider string
	State    string
}

// Models lists models, by provider then slug.
func (r *Registry) Models(ctx context.Context, f Filter) ([]Model, error) {
	q := `SELECT provider, slug, display_name, description, tier, default_effort, supported_efforts,
	             visibility, source, last_verified, state FROM models`
	var where []string
	var args []any
	if f.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, f.Provider)
	}
	if f.State != "" {
		where = append(where, "state = ?")
		args = append(args, f.State)
	}
	if len(where) > 0 {
		// #nosec G202 -- each element is a compile-time literal; values are bound.
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY provider, slug"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Model
	for rows.Next() {
		var m Model
		var efforts string
		if err := rows.Scan(&m.Provider, &m.Slug, &m.DisplayName, &m.Description, &m.Tier, &m.DefaultEffort,
			&efforts, &m.Visibility, &m.Source, &m.LastVerified, &m.State); err != nil {
			return nil, err
		}
		if efforts != "" {
			m.SupportedEfforts = strings.Split(efforts, ",")
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns one model by provider and slug.
func (r *Registry) Get(ctx context.Context, provider, slug string) (Model, error) {
	ms, err := r.Models(ctx, Filter{Provider: provider})
	if err != nil {
		return Model{}, err
	}
	for _, m := range ms {
		if m.Slug == slug {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("model %s/%s: %w", provider, slug, ErrNotFound)
}

// SetModelState enables or disables a model. A model the vendor hides cannot be
// enabled: the vendor is saying it is not for general use.
func (r *Registry) SetModelState(ctx context.Context, provider, slug, state string) error {
	if state != StateEnabled && state != StateDisabled {
		return fmt.Errorf("model state must be %s or %s", StateEnabled, StateDisabled)
	}
	m, err := r.Get(ctx, provider, slug)
	if err != nil {
		return err
	}
	// The visibility condition is in the UPDATE so a refresh hiding the model
	// between the read and the write cannot slip an enable through.
	res, err := r.db.ExecContext(ctx, `UPDATE models SET state = ? WHERE provider = ? AND slug = ? AND (? <> 'enabled' OR visibility <> 'hide')`,
		state, provider, slug, state)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("model %s is hidden by its vendor and cannot be enabled", m.Ref())
	}
	return nil
}

// SetTier records the user's tier for a model; discovery never overwrites it.
func (r *Registry) SetTier(ctx context.Context, provider, slug, tier string) error {
	switch tier {
	case TierFrontier, TierBalanced, TierFast:
	default:
		return fmt.Errorf("tier must be %s, %s or %s", TierFrontier, TierBalanced, TierFast)
	}
	if _, err := r.Get(ctx, provider, slug); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE models SET tier = ? WHERE provider = ? AND slug = ?`, tier, provider, slug)
	return err
}

// ParseRef splits "provider/slug". Slugs may themselves contain no slash.
func ParseRef(ref string) (provider, slug string, err error) {
	provider, slug, ok := strings.Cut(ref, "/")
	if !ok || provider == "" || slug == "" || strings.Contains(slug, "/") {
		return "", "", fmt.Errorf("model reference %q must be <provider>/<slug>", ref)
	}
	return provider, slug, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

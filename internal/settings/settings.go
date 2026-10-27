// Package settings manages items orch keeps in other tools' configuration
// files — Claude Code hooks, permission rules and plugins, cc-safety-net
// rules — as desired state in orch.db, and applies that state to the files.
//
// The store, the reconciler and the RPCs are generic. Everything that knows a
// file format sits behind Provider: it reads its files, turns their bytes into
// items, writes one item's change back into the bytes, validates additions,
// and describes each kind's fields so a client can build forms. Providers are
// registered once in a Registry; adding one needs no schema or RPC change.
//
// State is per machine. Each machine's orch.db holds that machine's choices
// and nothing syncs them; see docs/internals/settings.md.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// Origin says who put an item in a file.
type Origin string

const (
	// OriginOrch is an item orch install writes. It can be disabled but not
	// removed, because the next install would put it back.
	OriginOrch Origin = "orch"
	// OriginYou is an item added through orch (the app or the CLI).
	OriginYou Origin = "you"
	// OriginFound is an item seen in a file that neither orch install nor the
	// user, through orch, put there.
	OriginFound Origin = "found"
)

// Status is an item's state as clients show it, derived from the row.
type Status string

const (
	// StatusActive items are enabled and in their file.
	StatusActive Status = "active"
	// StatusDisabled items were turned off through orch.
	StatusDisabled Status = "disabled"
	// StatusRemoved items vanished from their file outside orch. They are not
	// put back unless turned on again.
	StatusRemoved Status = "removed"
	// StatusMissing items are enabled orch items absent from their file;
	// orch install restores them.
	StatusMissing Status = "missing"
)

// Errors a Service returns; the daemon maps each to an RPC status code.
var (
	ErrNotFound      = errors.New("not found")
	ErrInvalid       = errors.New("invalid")
	ErrNotAllowed    = errors.New("not allowed")
	ErrAlreadyExists = errors.New("already exists")
	ErrConflict      = errors.New("conflict")
	// ErrUnreadable means a provider's files could not be read or parsed, so
	// no change is computed from them.
	ErrUnreadable = errors.New("unreadable")
)

// Invalidf returns an ErrInvalid error with a message.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Item is one managed setting as a provider sees it in its files.
type Item struct {
	Kind     string
	Identity string
	// Payload is what the provider needs to write the item back, as JSON.
	Payload json.RawMessage
	// Enabled is the item's on/off state in the file. Kinds whose file holds
	// no off state (KindSpec.StateInFile false) are always enabled when seen.
	Enabled bool
	// Origin is OriginOrch or OriginFound when observed, OriginYou when built
	// from an add form.
	Origin Origin
}

// FieldSpec describes one field of a kind's add form.
type FieldSpec struct {
	Name  string
	Label string
	// Type is string, text (multi-line), int, bool or enum.
	Type     string
	Required bool
	// Options are the allowed values of an enum.
	Options     []string
	Help        string
	Placeholder string
}

// KindSpec describes one kind of item a provider manages.
type KindSpec struct {
	Name  string
	Title string
	// GroupTitle names what Describe's group is (an event, a list, a rulebook).
	GroupTitle string
	// Addable kinds accept new items from Fields; Removable ones can be
	// deleted from the file. Orch items are never removable.
	Addable   bool
	Removable bool
	// StateInFile is true when the file records an item's off state (a plugin
	// set to false, a rule overridden off). Otherwise turning an item off
	// removes it from the file and the store keeps its payload.
	StateInFile bool
	Fields      []FieldSpec
}

// Provider adapts one tool's configuration files. Its methods other than
// Read are pure functions of the bytes they are given, so a provider is
// tested without touching a disk and the Service owns every write.
type Provider interface {
	// Name is the stable provider key stored with each item.
	Name() string
	// Title is the human-readable name.
	Title() string
	Kinds() []KindSpec
	// Read returns the current bytes of every file the provider depends on,
	// keyed by absolute path; a missing file maps to nil. Only these paths
	// may be written by Apply.
	Read() (map[string][]byte, error)
	// Observe parses the files into the items they hold.
	Observe(files map[string][]byte) ([]Item, error)
	// Apply returns the new bytes of each file that must change for item to
	// be on (enabled) or off. A file it does not return is left alone.
	Apply(files map[string][]byte, item Item, enabled bool) (map[string][]byte, error)
	// Validate builds a new item from an add form's fields.
	Validate(kind string, fields map[string]string) (Item, error)
	// Describe returns the group an item is listed under and a one-line
	// summary, from its payload.
	Describe(kind string, payload json.RawMessage) (group, summary string)
}

// Registry holds the providers a Service manages, in registration order.
type Registry struct {
	providers []Provider
}

// NewRegistry registers providers; names must be unique.
func NewRegistry(ps ...Provider) (*Registry, error) {
	r := &Registry{}
	for _, p := range ps {
		if r.Get(p.Name()) != nil {
			return nil, fmt.Errorf("settings provider %q registered twice", p.Name())
		}
		r.providers = append(r.providers, p)
	}
	return r, nil
}

// Providers returns the registered providers in order.
func (r *Registry) Providers() []Provider { return slices.Clone(r.providers) }

// Get returns the named provider, or nil.
func (r *Registry) Get(name string) Provider {
	for _, p := range r.providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Kind returns a provider's kind spec by name.
func Kind(p Provider, name string) (KindSpec, bool) {
	for _, k := range p.Kinds() {
		if k.Name == name {
			return k, true
		}
	}
	return KindSpec{}, false
}

// Dedupe gives repeated identities a "#n" suffix in order of appearance, so
// two identical entries in a file stay two items.
func Dedupe(items []Item) []Item {
	seen := map[string]int{}
	for i := range items {
		key := items[i].Kind + "\x00" + items[i].Identity
		seen[key]++
		if n := seen[key]; n > 1 {
			items[i].Identity = fmt.Sprintf("%s#%d", items[i].Identity, n)
		}
	}
	return items
}

// sortedPaths returns a file map's keys in order.
func sortedPaths(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Package spaces holds the spaces policy: which GitHub orgs, folder roots, web
// prefixes and chat tenants belong to which space, and which spaces may read
// which. orchd validates a policy, stores its revisions in orch.db, and
// enforces only the revision an operator applied. See docs/internals/config.md.
package spaces

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Kind says how a space shares its content.
type Kind string

const (
	// Public spaces are readable from every space.
	Public Kind = "public"
	// Private spaces are personal: they read themselves, public, and any
	// private space they list.
	Private Kind = "private"
	// Isolated spaces are one per job and never read another isolated space.
	Isolated Kind = "isolated"
)

// Quarantine is the space of content that cannot be placed. No caller reads it.
const Quarantine = "quarantine"

// Shared is the one implicit space every caller and chunk is in when no
// policy is applied, so orch behaves as it did before spaces.
const Shared = "shared"

// strictness orders kinds: a nested root may only be at least as strict as
// every root above it.
func (k Kind) strictness() (int, bool) {
	switch k {
	case Public:
		return 0, true
	case Private:
		return 1, true
	case Isolated:
		return 2, true
	}
	return 0, false
}

// Space is one space and what is bound to it.
type Space struct {
	Name        string   `json:"name"`
	Kind        Kind     `json:"kind"`
	GitHubOrgs  []string `json:"github_orgs,omitempty"`
	Roots       []string `json:"roots,omitempty"`
	WebPrefixes []string `json:"web_prefixes,omitempty"`
	// Chat names chat tenants as "<provider>:<id>", e.g. "slack:T0123".
	Chat []string `json:"chat,omitempty"`
	// Reads lists the other spaces this one may read.
	Reads []string `json:"reads,omitempty"`
	// TermsFile is the outbound term list, kept outside every git tree.
	TermsFile string `json:"terms_file,omitempty"`
	// GHConfigDir is the gh profile directory (GH_CONFIG_DIR) the space's
	// GitHub calls run with. An isolated space's orgs are synced only through
	// it, once the user has logged it in with gh.
	GHConfigDir string `json:"gh_config_dir,omitempty"`
	// ClaudeConfigDir is the Claude Code configuration folder
	// (CLAUDE_CONFIG_DIR) 'orch run' starts the space's sessions with; ""
	// keeps the user's own (D-59). It holds that folder's own login.
	ClaudeConfigDir string `json:"claude_config_dir,omitempty"`
}

// Policy is a whole spaces policy, as saved in one revision.
type Policy struct {
	Spaces []Space `json:"spaces"`
	// PublicRootSessions is the space for content from sessions pinned under
	// a public root. Empty means the only private space if there is exactly
	// one, else quarantine.
	PublicRootSessions string `json:"public_root_sessions,omitempty"`
}

// maxDocBytes bounds a stored revision, so a saved draft cannot grow orch.db
// without limit.
const maxDocBytes = 1 << 20

// Reason: validation compares every root with every other, and orchd checks
// the newest draft for orch status, so a policy's size must stay bounded or
// one saved draft could stall orchd on every start.
const (
	maxSpaces    = 64
	maxPerList   = 256
	maxRootsSeen = 512
	// maxString is macOS's PATH_MAX; maxDepth bounds the parents the git
	// check walks, each of which costs a path's length.
	maxString = 1024
	maxDepth  = 64
)

// Parse decodes a policy document, refusing fields it does not know so a
// misspelled binding is an error rather than silently ignored.
func Parse(doc []byte) (Policy, error) {
	if len(doc) > maxDocBytes {
		return Policy{}, fmt.Errorf("spaces policy is %d bytes, over the %d-byte limit", len(doc), maxDocBytes)
	}
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("spaces policy: %w", err)
	}
	if dec.More() {
		return Policy{}, fmt.Errorf("spaces policy: unexpected data after the document")
	}
	if err := checkSize(p); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// checkSize refuses a policy too large to validate quickly.
func checkSize(p Policy) error {
	if len(p.Spaces) > maxSpaces {
		return fmt.Errorf("spaces policy has %d spaces, over the limit of %d", len(p.Spaces), maxSpaces)
	}
	roots := 0
	if len(p.PublicRootSessions) > maxString {
		return fmt.Errorf("spaces policy: public_root_sessions is over %d bytes", maxString)
	}
	for _, s := range p.Spaces {
		for _, l := range [][]string{s.GitHubOrgs, s.Roots, s.WebPrefixes, s.Chat, s.Reads} {
			if len(l) > maxPerList {
				return fmt.Errorf("spaces policy: a space has %d entries in one list, over the limit of %d", len(l), maxPerList)
			}
			for _, v := range l {
				if len(v) > maxString {
					return fmt.Errorf("spaces policy: an entry is over %d bytes", maxString)
				}
			}
		}
		for _, v := range []string{s.Name, string(s.Kind), s.TermsFile, s.GHConfigDir, s.ClaudeConfigDir} {
			if len(v) > maxString {
				return fmt.Errorf("spaces policy: a value is over %d bytes", maxString)
			}
		}
		for _, path := range append([]string{s.TermsFile, s.GHConfigDir, s.ClaudeConfigDir}, s.Roots...) {
			if strings.Count(path, "/") > maxDepth {
				return fmt.Errorf("spaces policy: a path is more than %d folders deep", maxDepth)
			}
		}
		roots += len(s.Roots)
	}
	if roots > maxRootsSeen {
		return fmt.Errorf("spaces policy has %d roots, over the limit of %d", roots, maxRootsSeen)
	}
	return nil
}

// Canonical encodes p the same way every time, so equal policies have equal
// hashes.
func Canonical(p Policy) ([]byte, error) {
	if p.Spaces == nil {
		p.Spaces = []Space{}
	}
	return json.Marshal(p)
}

// Hash is the hex sha256 of a stored document.
func Hash(doc []byte) string {
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

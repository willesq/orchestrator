package sources

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	maxURILen   = 2048
	maxScopeLen = 64
	maxTagLen   = 64
	maxTags     = 32
	minRefresh  = time.Minute
	maxRefresh  = 365 * 24 * time.Hour
)

// ownerDirRe is one directory name under a known root: no separators, so an
// owner can never name a path outside the roots.
var ownerDirRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Validate checks a source before it is stored and normalizes it in place:
// trimmed fields, a cleaned repo path, deduplicated tags, and Owner derived
// from the kind. Every way of adding a source (the CLI, orchd's RPCs) goes
// through it, so the registry only ever holds sources refresh can act on.
func Validate(s *Source) error {
	s.URI = strings.TrimSpace(s.URI)
	switch s.Kind {
	case KindWeb:
		if err := validWebURL(s.URI); err != nil {
			return err
		}
		s.Owner = ""
	case KindRepo:
		// Refresh reads the repo straight from this path.
		if !filepath.IsAbs(s.URI) {
			return fmt.Errorf("a repo source is an absolute local path, not %q", s.URI)
		}
		s.URI = filepath.Clean(s.URI)
		s.Owner = ""
	case KindOwner:
		// Refresh joins this name onto each known root.
		if !ownerDirRe.MatchString(s.URI) || strings.Trim(s.URI, ".") == "" {
			return fmt.Errorf("an owner source is one directory name under a known root, not %q", s.URI)
		}
		// Repo sources are indexed under repo://local/, so an owner named
		// local would share their content.
		if s.URI == "local" {
			return fmt.Errorf("%q is reserved; it names the repo sources' content", s.URI)
		}
		s.Owner = s.URI
	default:
		return fmt.Errorf("kind must be web, repo or owner, not %q", s.Kind)
	}
	if len(s.URI) > maxURILen {
		return fmt.Errorf("source is longer than %d characters", maxURILen)
	}
	if d := time.Duration(s.RefreshSecs) * time.Second; d < minRefresh || d > maxRefresh {
		return fmt.Errorf("refresh interval must be between %s and %s", minRefresh, maxRefresh)
	}
	s.Scope = strings.TrimSpace(s.Scope)
	if err := validLabel("scope", s.Scope, maxScopeLen); err != nil {
		return err
	}
	var tags []string
	for _, t := range s.Tags {
		t = strings.TrimSpace(t)
		if t == "" || slices.Contains(tags, t) {
			continue
		}
		// Tags are stored comma-joined.
		if strings.Contains(t, ",") {
			return fmt.Errorf("tag %q contains a comma", t)
		}
		if err := validLabel("tag", t, maxTagLen); err != nil {
			return err
		}
		// Refresh adds these itself; a user tag must not pose as one.
		if strings.HasPrefix(t, "owner:") || strings.HasPrefix(t, "repo:") {
			return fmt.Errorf("tag %q uses a reserved prefix (owner:, repo:)", t)
		}
		tags = append(tags, t)
	}
	if len(tags) > maxTags {
		return fmt.Errorf("at most %d tags", maxTags)
	}
	s.Tags = tags
	return nil
}

func validWebURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("a web source must be an http or https URL, not %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("URL %q has no host", raw)
	}
	// The URI is shown and stored as is; credentials do not belong in it.
	if u.User != nil {
		return fmt.Errorf("URL %q must not carry credentials", raw)
	}
	return nil
}

func validLabel(what, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s %q is longer than %d characters", what, s, max)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s %q contains control characters", what, s)
		}
	}
	return nil
}

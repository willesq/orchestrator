package spaces

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
)

// Rule names the check a policy failed, so the app can mark the line.
type Rule string

const (
	RuleUnknownKind           Rule = "unknown_kind"
	RuleBadName               Rule = "bad_name"
	RuleDuplicateName         Rule = "duplicate_name"
	RuleOrgBoundTwice         Rule = "org_bound_twice"
	RulePrefixBoundTwice      Rule = "prefix_bound_twice"
	RuleTenantBoundTwice      Rule = "tenant_bound_twice"
	RuleRootBoundTwice        Rule = "root_bound_twice"
	RuleBadBinding            Rule = "bad_binding"
	RuleNestedLessStrict      Rule = "nested_root_less_strict"
	RuleIsolatedInIsolated    Rule = "isolated_root_in_other_isolated"
	RuleIsolatedReadsIsolated Rule = "isolated_reads_isolated"
	RulePrivateReadsIsolated  Rule = "private_reads_isolated"
	RulePublicReads           Rule = "public_reads"
	RuleUnknownRead           Rule = "unknown_read"
	RuleTermsInGitTree        Rule = "terms_file_in_git_tree"
	RuleGHProfileInGitTree    Rule = "gh_config_dir_in_git_tree"
	RuleGHProfileShared       Rule = "gh_config_dir_shared"
	RulePublicRootSessions    Rule = "public_root_sessions"
	RuleTooLarge              Rule = "too_large"
	RuleUnparseable           Rule = "unparseable"
)

// Problem is one failed check.
type Problem struct {
	Rule    Rule
	Space   string
	Message string
}

func (p Problem) Error() string { return p.Message }

// Problems is every check a policy failed, up to maxProblems. It is the error
// Validate returns.
type Problems []Problem

// maxProblems bounds what Validate reports: a policy with more is shown its
// first ones, which is enough to start fixing it.
const maxProblems = 100

func (ps Problems) Error() string {
	msgs := make([]string, len(ps))
	for i, p := range ps {
		msgs[i] = p.Message
	}
	return "invalid spaces policy: " + strings.Join(msgs, "; ")
}

// Has reports whether any problem broke rule.
func (ps Problems) Has(rule Rule) bool {
	for _, p := range ps {
		if p.Rule == rule {
			return true
		}
	}
	return false
}

// Env is what validation and apply read outside the policy itself.
type Env struct {
	// Home expands a leading "~" in roots and terms files.
	Home string
	// InGitTree reports whether path lies inside a git working tree; nil
	// looks for a .git entry in path's parents.
	InGitTree func(path string) bool
	// LookupOrg reads a GitHub account's identity, for apply and sync, as
	// the gh profile of the space that binds it; nil refuses to apply a
	// policy that binds any org.
	LookupOrg func(ctx context.Context, login string, gh GHProfile) (OrgIdentity, error)
}

// DefaultEnv reads the user's home and the real filesystem.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Home: home}
}

func (e Env) inGitTree(path string) bool {
	if e.InGitTree != nil {
		return e.InGitTree(path)
	}
	dir := filepath.Dir(path)
	// A symlinked folder can lead into a repository its path does not show.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}

// expand turns "~" and "~/x" into paths under home and cleans the result.
// It returns "" for a path that is not absolute once expanded.
func (e Env) expand(p string) string {
	switch {
	case p == "~":
		p = e.Home
	case strings.HasPrefix(p, "~/"):
		if e.Home == "" {
			return ""
		}
		p = filepath.Join(e.Home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// root is one bound folder root, expanded.
type root struct {
	path  string
	key   string // case-folded: macOS volumes are usually case-insensitive
	space string
	kind  Kind
}

// under reports whether r lies strictly inside above.
func (r root) under(above root) bool {
	return strings.HasPrefix(r.key, strings.TrimSuffix(above.key, string(filepath.Separator))+string(filepath.Separator))
}

// Validate checks p and returns it resolved for lookups, or every problem
// found as Problems.
func Validate(p Policy, env Env) (*Resolved, error) {
	var ps Problems
	add := func(rule Rule, space, format string, args ...any) {
		if len(ps) < maxProblems {
			ps = append(ps, Problem{Rule: rule, Space: space, Message: fmt.Sprintf(format, args...)})
		}
	}
	if err := checkSize(p); err != nil {
		return nil, Problems{{Rule: RuleTooLarge, Message: err.Error()}}
	}

	byName := make(map[string]*Space, len(p.Spaces))
	for i := range p.Spaces {
		s := &p.Spaces[i]
		switch {
		case !namePattern.MatchString(s.Name):
			add(RuleBadName, s.Name, "space name %q must be 1-32 lowercase letters, digits, - or _, starting with a letter", s.Name)
			continue
		case s.Name == Quarantine || s.Name == Shared:
			add(RuleBadName, s.Name, "space name %q is reserved", s.Name)
			continue
		case byName[s.Name] != nil:
			add(RuleDuplicateName, s.Name, "space %q is defined twice", s.Name)
			continue
		}
		if _, ok := s.Kind.strictness(); !ok {
			add(RuleUnknownKind, s.Name, "space %q has unknown kind %q (want public, private or isolated)", s.Name, s.Kind)
			continue
		}
		byName[s.Name] = s
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	// Bindings: each org, web prefix and chat tenant belongs to one space.
	bound := map[Rule]map[string]string{
		RuleOrgBoundTwice: {}, RulePrefixBoundTwice: {}, RuleTenantBoundTwice: {},
	}
	bind := func(rule Rule, what, key, display, space string) {
		if prev, ok := bound[rule][key]; ok {
			add(rule, space, "%s %q is bound to both %q and %q", what, display, prev, space)
			return
		}
		bound[rule][key] = space
	}
	var roots []root
	ghDirs := map[string]string{}
	ghDirOwner := map[string]string{}
	for _, n := range names {
		s := byName[n]
		for _, org := range s.GitHubOrgs {
			if !github.ValidLogin(org) {
				add(RuleBadBinding, n, "space %q has GitHub org %q, which is not a GitHub login", n, org)
				continue
			}
			bind(RuleOrgBoundTwice, "GitHub org", strings.ToLower(org), org, n)
		}
		for _, prefix := range s.WebPrefixes {
			key, ok := prefixKey(prefix)
			if !ok {
				add(RuleBadBinding, n, "space %q has web prefix %q, which is not an http or https URL", n, prefix)
				continue
			}
			bind(RulePrefixBoundTwice, "web prefix", key, prefix, n)
		}
		for _, tenant := range s.Chat {
			provider, id, ok := strings.Cut(tenant, ":")
			if !ok || (provider != "slack" && provider != "teams") || id == "" {
				add(RuleBadBinding, n, "space %q has chat tenant %q (want slack:<id> or teams:<id>)", n, tenant)
				continue
			}
			bind(RuleTenantBoundTwice, "chat tenant", strings.ToLower(tenant), tenant, n)
		}
		for _, r := range s.Roots {
			path := env.expand(r)
			if path == "" {
				add(RuleBadBinding, n, "space %q has root %q, which is not an absolute path", n, r)
				continue
			}
			roots = append(roots, root{path: path, key: strings.ToLower(path), space: n, kind: s.Kind})
		}
		if s.TermsFile != "" {
			path := env.expand(s.TermsFile)
			switch {
			case path == "":
				add(RuleBadBinding, n, "space %q has terms file %q, which is not an absolute path", n, s.TermsFile)
			case env.inGitTree(path):
				// Reason: a terms file in a repository would be committed and
				// pushed, publishing the very terms it keeps from leaving.
				add(RuleTermsInGitTree, n, "space %q has its terms file inside a git working tree", n)
			}
		}
		if s.GHConfigDir != "" {
			path := env.expand(s.GHConfigDir)
			key := strings.ToLower(path)
			switch {
			case path == "":
				add(RuleBadBinding, n, "space %q has gh_config_dir %q, which is not an absolute path", n, s.GHConfigDir)
			case env.inGitTree(path):
				// Reason: gh may store a token in the directory's hosts.yml,
				// which a repository would commit and push.
				add(RuleGHProfileInGitTree, n, "space %q has its gh_config_dir inside a git working tree", n)
			case ghDirOwner[key] != "":
				// Reason: one profile is one GitHub account, so two spaces
				// sharing it would share an account.
				add(RuleGHProfileShared, n, "spaces %q and %q use the same gh_config_dir; give each its own GitHub account", ghDirOwner[key], n)
			default:
				ghDirs[n], ghDirOwner[key] = path, n
			}
		}
	}
	checkRoots(roots, add)

	for _, n := range names {
		s := byName[n]
		for _, target := range s.Reads {
			t, ok := byName[target]
			switch {
			case !ok:
				add(RuleUnknownRead, n, "space %q reads %q, which is not a space", n, target)
			case target == n:
			case s.Kind == Public:
				add(RulePublicReads, n, "public space %q cannot read other spaces (it reads %q)", n, target)
			case t.Kind == Isolated && s.Kind == Isolated:
				add(RuleIsolatedReadsIsolated, n, "isolated space %q cannot read isolated space %q; merge them instead", n, target)
			case t.Kind == Isolated && s.Kind == Private:
				add(RulePrivateReadsIsolated, n, "private space %q cannot read isolated space %q", n, target)
			}
		}
	}

	res := &Resolved{Policy: p, spaces: byName, roots: roots, ghDirs: ghDirs}
	res.PublicRootSessions = publicRootSessions(p.PublicRootSessions, byName, names, add)
	if len(ps) > 0 {
		return nil, ps
	}
	return res, nil
}

// checkRoots enforces D-15: no root bound twice, a nested root at least as
// strict as every root above it, and under an isolated root only that space.
func checkRoots(roots []root, add func(Rule, string, string, ...any)) {
	seen := map[string]root{}
	uniq := roots[:0:0]
	for _, r := range roots {
		if prev, ok := seen[r.key]; ok {
			add(RuleRootBoundTwice, r.space, "root %q is bound to both %q and %q", r.path, prev.space, r.space)
			continue
		}
		seen[r.key] = r
		uniq = append(uniq, r)
	}
	for _, inner := range uniq {
		for _, outer := range uniq {
			if !inner.under(outer) {
				continue
			}
			innerS, _ := inner.kind.strictness()
			outerS, _ := outer.kind.strictness()
			switch {
			case innerS < outerS:
				add(RuleNestedLessStrict, inner.space, "root %q (%s, %s) is inside %q (%s, %s) but less strict",
					inner.path, inner.space, inner.kind, outer.path, outer.space, outer.kind)
			case outer.kind == Isolated && inner.space != outer.space:
				add(RuleIsolatedInIsolated, inner.space, "root %q of isolated space %q is inside root %q of isolated space %q",
					inner.path, inner.space, outer.path, outer.space)
			}
		}
	}
}

// publicRootSessions resolves where content from sessions under a public root
// goes (D-22).
func publicRootSessions(set string, byName map[string]*Space, names []string, add func(Rule, string, string, ...any)) string {
	if set != "" {
		s, ok := byName[set]
		if !ok || s.Kind == Public {
			add(RulePublicRootSessions, set, "public_root_sessions must name a private or isolated space, not %q", set)
			return Quarantine
		}
		return set
	}
	var private []string
	for _, n := range names {
		if byName[n].Kind == Private {
			private = append(private, n)
		}
	}
	if len(private) == 1 {
		return private[0]
	}
	return Quarantine
}

// prefixKey normalizes a web prefix or URL for comparison: the scheme is
// ignored, the host is case-insensitive, a default port and trailing dots on
// the host are dropped, and the path is cleaned of dot segments, re-escaped
// and stripped of its trailing slash, so https://a.com/x/../private,
// http://a.com/%70rivate and https://a.com/private/ all key the same. The
// path stays case-sensitive. A backslash in the path is refused: some
// servers read it as a slash.
func prefixKey(prefix string) (string, bool) {
	u, err := url.Parse(prefix)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Contains(u.Path, `\`) {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", false
	}
	port := 0
	if ps := u.Port(); ps != "" {
		if port, err = strconv.Atoi(ps); err != nil || port < 1 || port > 65535 {
			return "", false
		}
	}
	if port != 0 && !(scheme == "http" && port == 80 || scheme == "https" && port == 443) {
		host = net.JoinHostPort(host, strconv.Itoa(port))
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	p := strings.TrimSuffix(path.Clean("/"+u.Path), "/")
	// Reason: http and https name the same site, so a prefix bound under
	// one scheme also holds pages fetched under the other.
	return "http(s)://" + host + (&url.URL{Path: p}).EscapedPath(), true
}

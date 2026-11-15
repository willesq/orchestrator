package spaces

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Configurable outbound rules (spaces plan SP-075, D-60). The outbound terms
// check reads commands by the baseline rules of outbound_rules.go. The
// operator may switch any of them off (relaxing the check) or back on, and
// add rules of their own that only make the check stricter:
//
//   - command: a command whose words are checked as outbound text sent to
//     a destination that holds no space (glab, a deploy script);
//   - destination: an owner or owner/repo that holds no space for the
//     check, so no space's terms may reach it (a public repo of a job's
//     org);
//   - fail_closed: a term-like pattern (a literal, or re:<regex>; case-
//     insensitive, on word boundaries) that refuses a tool call whose
//     tool name or text it matches as outbound_unchecked (a GitHub MCP
//     tool, curl to the GitHub API).
//
// A rule's value is configuration that may name what the operator keeps
// private, so, like a term (INV-07), it never appears in logs, refusals,
// errors, the app or indexed content: a rule is named by its id alone, and
// the value is write-only outside orch.db and the guard snapshot.

// UserRuleKind is the kind of rule the operator may add.
type UserRuleKind string

// The kinds of rule the operator may add. Each can only add to what the
// check refuses.
const (
	UserRuleCommand     UserRuleKind = "command"
	UserRuleDestination UserRuleKind = "destination"
	UserRuleFailClosed  UserRuleKind = "fail_closed"
)

// UserRuleKinds lists the kinds in a fixed order.
var UserRuleKinds = []UserRuleKind{UserRuleCommand, UserRuleDestination, UserRuleFailClosed}

// UserRule is a rule the operator added.
type UserRule struct {
	// ID is "user-<n>", given when the rule is added and never reused.
	ID    string       `json:"id"`
	Kind  UserRuleKind `json:"kind"`
	Value string       `json:"value"`
	// Off is set while the operator has switched the rule off.
	Off bool `json:"off,omitempty"`
}

// OutboundRules is the operator's rule configuration. The zero value is the
// baseline with every rule on and no rule added.
type OutboundRules struct {
	// Off are the baseline rules the operator switched off, in baseline
	// order.
	Off []OutboundRule `json:"off,omitempty"`
	// User are the rules the operator added, in the order added.
	User []UserRule `json:"user,omitempty"`
	// LastID is the number of the last user rule ever added.
	LastID int `json:"last_id,omitempty"`
}

// Limits on what the operator may add, so the rules cannot slow every
// outbound check.
const (
	maxUserRules     = 256
	maxUserRuleValue = maxTermLen
)

// Errors for rule changes. None carries a rule's value.
var (
	// ErrRuleLoosens is returned for an added rule that would let through
	// something the baseline refuses.
	ErrRuleLoosens = errors.New("an added rule may only make the check stricter")
	// ErrUnknownRule is returned for an id that names no rule.
	ErrUnknownRule = errors.New("no outbound rule has that id")
	// ErrBadRule is returned for a value its kind cannot take.
	ErrBadRule = errors.New("not a valid outbound rule")
)

// loosening kinds are ones an operator might try that would allow more;
// they get ErrRuleLoosens rather than a plain "unknown kind".
var looseningKinds = flagSet("allow", "allowed", "exempt", "except", "exception", "skip", "trust", "trusted",
	"ignore", "permit", "safe", "unchecked")

// userRuleID matches the id of an added rule.
var userRuleID = regexp.MustCompile(`^user-[1-9][0-9]{0,8}$`)

// ruleCommandName is what a command rule may name: a program's name, as
// the walk compares it (base name, case folded).
var ruleCommandName = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)

// ruleDestination is an owner or owner/repo as GitHub spells them.
var ruleDestination = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38})(?:/[a-z0-9._-]{1,100})?$`)

// baselineReadNames are commands the walk already reads in its own way
// (git and gh, shells, wrappers, source, alias, cd): a command rule naming
// one would never apply, or would replace how it is read.
var baselineReadNames = flagSet("git", "gh", "cd", "pushd", "popd", "source", ".", "alias", "unset", "env",
	"eval", "xargs", "find", "watch")

// IsBaseline reports whether id names a baseline rule.
func IsBaseline(id OutboundRule) bool {
	for _, b := range OutboundBaseline {
		if b.ID == id {
			return true
		}
	}
	return false
}

// On reports whether baseline rule id is on. A nil rule set is the
// baseline, every rule on.
func (r *OutboundRules) On(id OutboundRule) bool {
	return r == nil || !slices.Contains(r.Off, id)
}

// clone is a deep copy of r, so a change never alters the rules a reader
// holds.
func (r OutboundRules) clone() OutboundRules {
	r.Off = slices.Clone(r.Off)
	r.User = slices.Clone(r.User)
	return r
}

// userIndex is the index of added rule id in r.User, -1 for none.
func (r OutboundRules) userIndex(id string) int {
	return slices.IndexFunc(r.User, func(u UserRule) bool { return u.ID == id })
}

// Switch turns rule id, baseline or added, on or off. changed is false when
// it already was.
func (r OutboundRules) Switch(id string, on bool) (next OutboundRules, changed bool, err error) {
	next = r.clone()
	if IsBaseline(OutboundRule(id)) {
		rid := OutboundRule(id)
		if next.On(rid) == on {
			return r, false, nil
		}
		if on {
			next.Off = slices.DeleteFunc(next.Off, func(o OutboundRule) bool { return o == rid })
		} else {
			next.Off = append(next.Off, rid)
			// Reason: a fixed order, so equal configurations store the
			// same JSON and hash.
			slices.SortFunc(next.Off, func(a, b OutboundRule) int { return baselineIndex(a) - baselineIndex(b) })
		}
		return next, true, nil
	}
	i := next.userIndex(id)
	if i < 0 {
		return r, false, ErrUnknownRule
	}
	if next.User[i].Off == !on {
		return r, false, nil
	}
	next.User[i].Off = !on
	return next, true, nil
}

// baselineIndex is id's position in OutboundBaseline.
func baselineIndex(id OutboundRule) int {
	return slices.IndexFunc(OutboundBaseline, func(b OutboundBaselineRule) bool { return b.ID == id })
}

// Add adds a rule of kind with value, on, and returns it with its new id.
func (r OutboundRules) Add(kind, value string) (OutboundRules, UserRule, error) {
	u, err := ParseUserRule(kind, value)
	if err != nil {
		return r, UserRule{}, err
	}
	if len(r.User) >= maxUserRules {
		return r, UserRule{}, fmt.Errorf("%w: at most %d rules may be added", ErrBadRule, maxUserRules)
	}
	for _, have := range r.User {
		if have.Kind == u.Kind && have.Value == u.Value {
			// Reason: names the rule by id, never by its value.
			return r, UserRule{}, fmt.Errorf("%w: rule %s already says that", ErrBadRule, have.ID)
		}
	}
	next := r.clone()
	next.LastID++
	u.ID = "user-" + strconv.Itoa(next.LastID)
	next.User = append(next.User, u)
	return next, u, nil
}

// Remove removes added rule id. A baseline rule cannot be removed, only
// switched off.
func (r OutboundRules) Remove(id string) (OutboundRules, error) {
	if IsBaseline(OutboundRule(id)) {
		return r, fmt.Errorf("%w: a baseline rule cannot be removed; switch it off instead", ErrBadRule)
	}
	i := r.userIndex(id)
	if i < 0 {
		return r, ErrUnknownRule
	}
	next := r.clone()
	next.User = slices.Delete(next.User, i, i+1)
	return next, nil
}

// ParseUserRule checks an added rule of kind with value and returns it in
// its stored form, without an id. An error never quotes the value.
func ParseUserRule(kind, value string) (UserRule, error) {
	k := UserRuleKind(strings.ToLower(strings.TrimSpace(kind)))
	v := strings.TrimSpace(value)
	switch {
	case looseningKinds[string(k)]:
		return UserRule{}, fmt.Errorf("%w: rules that allow or exempt cannot be added; switch a baseline rule off instead", ErrRuleLoosens)
	case !slices.Contains(UserRuleKinds, k):
		// Reason: the kind is not quoted, since an operator may have
		// swapped kind and value.
		return UserRule{}, fmt.Errorf("%w: the kind is not one of command, destination, fail_closed", ErrBadRule)
	case v == "":
		return UserRule{}, fmt.Errorf("%w: the %s rule's value is empty", ErrBadRule, k)
	case strings.HasPrefix(v, "!"):
		// Reason: a negation, as in ignore files, would exempt.
		return UserRule{}, fmt.Errorf("%w: a value starting with ! would exempt", ErrRuleLoosens)
	case len(v) > maxUserRuleValue:
		return UserRule{}, fmt.Errorf("%w: the %s rule's value is over %d bytes", ErrBadRule, k, maxUserRuleValue)
	case strings.ContainsAny(v, "\n\r\x00"):
		return UserRule{}, fmt.Errorf("%w: the %s rule's value is not one line", ErrBadRule, k)
	}
	switch k {
	case UserRuleCommand:
		v = strings.ToLower(v)
		if !ruleCommandName.MatchString(v) {
			return UserRule{}, fmt.Errorf("%w: a command rule names one program by its name, without a path", ErrBadRule)
		}
		if baselineReadNames[v] || shellNames[v] || outboundWrappers[v] || strings.HasPrefix(v, "git-") {
			return UserRule{}, fmt.Errorf("%w: the baseline already reads that command; a rule cannot change how", ErrBadRule)
		}
	case UserRuleDestination:
		v = strings.ToLower(strings.TrimSuffix(v, ".git"))
		if !ruleDestination.MatchString(v) {
			return UserRule{}, fmt.Errorf("%w: a destination rule names a GitHub owner or owner/repo", ErrBadRule)
		}
	case UserRuleFailClosed:
		if _, err := compileFailClosed(v); err != nil {
			return UserRule{}, err
		}
	}
	return UserRule{Kind: k, Value: v}, nil
}

// compileFailClosed compiles a fail_closed value as a terms file line.
func compileFailClosed(v string) (term, error) {
	if strings.HasPrefix(v, "#") {
		return term{}, fmt.Errorf("%w: a fail_closed pattern cannot start with #", ErrBadRule)
	}
	l, err := ParseTerms("", []byte(v))
	if err != nil || l.Len() != 1 {
		// Reason: ParseTerms names the line, which here is always 1; say
		// only what kind of value it is.
		return term{}, fmt.Errorf("%w: a fail_closed pattern is a literal or re:<regex> that matches some text", ErrBadRule)
	}
	return l.terms[0], nil
}

// Validate checks a stored configuration, as read back from orch.db or the
// guard snapshot.
func (r OutboundRules) Validate() error {
	for _, id := range r.Off {
		if !IsBaseline(id) {
			return fmt.Errorf("%w: a switched-off id is not a baseline rule", ErrBadRule)
		}
	}
	if len(r.User) > maxUserRules {
		return fmt.Errorf("%w: over %d rules added", ErrBadRule, maxUserRules)
	}
	seen := map[string]bool{}
	for _, u := range r.User {
		if !userRuleID.MatchString(u.ID) || seen[u.ID] {
			return fmt.Errorf("%w: an added rule's id is not valid", ErrBadRule)
		}
		seen[u.ID] = true
		n, _ := strconv.Atoi(strings.TrimPrefix(u.ID, "user-"))
		if n > r.LastID {
			return fmt.Errorf("%w: rule %s is newer than the last id given", ErrBadRule, u.ID)
		}
		p, err := ParseUserRule(string(u.Kind), u.Value)
		if err != nil {
			return fmt.Errorf("rule %s: %w", u.ID, err)
		}
		if p.Kind != u.Kind || p.Value != u.Value {
			return fmt.Errorf("%w: rule %s is not stored in its normal form", ErrBadRule, u.ID)
		}
	}
	return nil
}

// ruleSet is a configuration compiled for one check. A nil *ruleSet is the
// baseline.
type ruleSet struct {
	rules    *OutboundRules
	commands map[string]string
	dests    map[string]string
	fail     []failRule
}

type failRule struct {
	id string
	t  term
}

// compile builds the rule set the walk and the check use. It skips rules
// that are off.
func (r *OutboundRules) compile() (*ruleSet, error) {
	if r == nil {
		return nil, nil
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	s := &ruleSet{rules: r, commands: map[string]string{}, dests: map[string]string{}}
	for _, u := range r.User {
		if u.Off {
			continue
		}
		switch u.Kind {
		case UserRuleCommand:
			s.commands[u.Value] = u.ID
		case UserRuleDestination:
			s.dests[u.Value] = u.ID
		case UserRuleFailClosed:
			t, err := compileFailClosed(u.Value)
			if err != nil {
				return nil, err
			}
			s.fail = append(s.fail, failRule{u.ID, t})
		}
	}
	return s, nil
}

// on reports whether baseline rule id is on.
func (s *ruleSet) on(id OutboundRule) bool {
	return s == nil || s.rules.On(id)
}

// command is the id of the on command rule naming name, "" for none.
func (s *ruleSet) command(name string) string {
	if s == nil {
		return ""
	}
	return s.commands[name]
}

// mentionsCommand reports whether cmd may run a command a rule names, for
// the cheap test that skips parsing.
func (s *ruleSet) mentionsCommand(cmd string) bool {
	if s == nil || len(s.commands) == 0 {
		return false
	}
	c := strings.ToLower(unquote.Replace(cmd))
	for name := range s.commands {
		if strings.Contains(c, name) {
			return true
		}
	}
	return false
}

// destination is the id of the on destination rule matching owner or
// owner/repo, "" for none.
func (s *ruleSet) destination(owner, repo string) string {
	if s == nil || len(s.dests) == 0 || owner == "" {
		return ""
	}
	o := strings.ToLower(owner)
	if id := s.dests[o]; id != "" {
		return id
	}
	if repo == "" {
		return ""
	}
	return s.dests[o+"/"+strings.ToLower(strings.TrimSuffix(repo, ".git"))]
}

// failClosed is the id of the first on fail_closed rule whose pattern
// text matches, "" for none. It stops, finding nothing, once done is
// closed.
func (s *ruleSet) failClosed(done <-chan struct{}, text string) string {
	if s == nil {
		return ""
	}
	for _, f := range s.fail {
		select {
		case <-done:
			return ""
		default:
		}
		if f.t.re.MatchString(text) {
			return f.id
		}
	}
	return ""
}

// OutboundCandidate reports whether the terms check has anything to decide
// for a tool call under rules: it holds an outbound action or a command
// that cannot be read, the rules have an on fail_closed rule (which may
// match any call), or the rules cannot be compiled. The hook asks before
// reading the terms files, so most calls skip them.
func OutboundCandidate(tool string, input []byte, cwd, home string, rules *OutboundRules) bool {
	rs, err := rules.compile()
	if err != nil || rs != nil && len(rs.fail) > 0 {
		return true
	}
	acts, unknown, _ := readOutbound(tool, input, cwd, home, rs)
	return len(acts) > 0 || unknown
}

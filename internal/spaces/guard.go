package spaces

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// GuardMode is the tool guard's enforcement state (D-12). SP-070 stores it
// with the applied policy and lets the operator switch it.
type GuardMode string

const (
	// GuardOff decides nothing that is enforced; events are still logged
	// with the verdict the guard would have given.
	GuardOff GuardMode = "off"
	// GuardLog is like GuardOff: the verdict is logged, never enforced.
	GuardLog GuardMode = "log"
	// GuardBlockOtherJobs, the default, refuses calls that reach another
	// isolated space (another job) or a protected path.
	GuardBlockOtherJobs GuardMode = "block-other-jobs"
	// GuardStrict also refuses calls that reach any space the session
	// cannot read and calls whose targets cannot be fully read (opaque).
	GuardStrict GuardMode = "strict"
)

// Enforcing reports whether the mode's refusals take effect.
func (m GuardMode) Enforcing() bool {
	return m != GuardOff && m != GuardLog
}

// Verdict is what the guard does with one tool call.
type Verdict string

const (
	Allow  Verdict = "allow"
	Refuse Verdict = "refuse"
)

// GuardRule names the rule behind a verdict. It is safe to show to the
// session and to log: it names a rule, never the matched path or text.
type GuardRule string

const (
	// RuleNoPolicy: no spaces are configured, so nothing is guarded (INV-10).
	RuleNoPolicy GuardRule = "no_policy"
	// RulePinMissing: the calling session has no pin, so its space is
	// unknown; enforcing modes fail closed (INV-13).
	RulePinMissing GuardRule = "pin_missing"
	// RuleSelfProtection: a file tool writes to a path orch protects.
	RuleSelfProtection GuardRule = "self_protection"
	// RuleOtherJobPath: the cwd, a path, a cd destination, a git dir or a
	// glob prefix lies in another isolated space.
	RuleOtherJobPath GuardRule = "other_job_path"
	// RuleOtherJobAncestor: a path, git dir or prefix contains another
	// isolated space's root, so the call may reach into it (grep -r ~/code).
	RuleOtherJobAncestor GuardRule = "other_job_ancestor"
	// RuleOtherJobRepo: a GitHub repo is owned by an org bound to another
	// isolated space.
	RuleOtherJobRepo GuardRule = "other_job_repo"
	// RuleUnreadableSpace: strict only; a target lies in, contains or is a
	// repo of a space the session may not read.
	RuleUnreadableSpace GuardRule = "unreadable_space"
	// RuleAnalysisLimit: Targets stopped at one of its limits, so the call
	// was not fully read; enforcing modes refuse it (RC-36).
	RuleAnalysisLimit GuardRule = "analysis_limit"
	// RuleMalformedTarget: the cwd or a target is not an absolute path, or
	// a repo is not "owner/repo", so it cannot be placed; enforcing modes
	// refuse it.
	RuleMalformedTarget GuardRule = "malformed_target"
	// RuleOpaqueStrict: strict only; part of the call cannot be analysed.
	RuleOpaqueStrict GuardRule = "opaque_strict"
	// RuleOpaqueUnverified: block-other-jobs allows an opaque call whose
	// known targets stay out of other jobs; the log keeps it for audit.
	RuleOpaqueUnverified GuardRule = "opaque_unverified"
	// RuleWithinSpace: every target is in the session's space, a space it
	// may reach under the mode, or no space at all.
	RuleWithinSpace GuardRule = "within_space"
)

// GuardEventKind is the hook event a decision is for.
type GuardEventKind string

const (
	// EventPreToolUse is a tool call the harness waits on; it can be refused.
	EventPreToolUse GuardEventKind = "pre-tool-use"
	// EventCwdChanged reports a directory move that already happened
	// (Claude Code's /cd or a cd in Bash). It cannot be refused; its verdict
	// is logged, and the next tool call runs in the new cwd and is judged
	// there.
	EventCwdChanged GuardEventKind = "cwd-changed"
)

// GuardPolicy is what the guard decides against. Build it with
// NewGuardPolicy; the zero value is no policy and guards nothing.
type GuardPolicy struct {
	res       *Resolved
	roots     []guardRoot
	protected [][]string
}

// guardRoot is one bound root under every spelling a target may use.
type guardRoot struct {
	keys  []string
	space string
}

// NewGuardPolicy prepares res and the protected paths for Decide. Targets
// reports physical paths, so every root and protected path is matched both
// as written and with its symlinks resolved; that resolution reads the
// filesystem, once per policy, which keeps Decide itself pure. Protected
// are absolute paths file tools may not write: orch's config and data,
// harness settings and the like (SP-042). A directory protects everything
// under it; a path that is not absolute is an error, never silently
// unprotected. A nil or unconfigured res guards nothing. The resolution
// is a snapshot: rebuild the policy when roots or their links change.
func NewGuardPolicy(res *Resolved, protected []string) (GuardPolicy, error) {
	for _, p := range protected {
		if !filepath.IsAbs(p) {
			return GuardPolicy{}, fmt.Errorf("protected path %q is not absolute", p)
		}
	}
	if !res.Configured() {
		return GuardPolicy{}, nil
	}
	pr := newPathResolver()
	g := GuardPolicy{res: res}
	for _, r := range res.roots {
		g.roots = append(g.roots, guardRoot{keys: spellings(pr, r.path), space: r.space})
	}
	for _, p := range protected {
		g.protected = append(g.protected, spellings(pr, p))
	}
	return g, nil
}

// spellings are p's match keys: as written and physical.
func spellings(pr *pathResolver, p string) []string {
	out := []string{guardKey(p)}
	if phys := guardKey(pr.resolve(p).path); phys != out[0] {
		out = append(out, phys)
	}
	return out
}

// guardKey is how paths compare: cleaned, Unicode NFC and case-folded, as
// macOS volumes usually treat names.
func guardKey(p string) string {
	return strings.ToLower(norm.NFC.String(filepath.Clean(p)))
}

// under reports whether key is base or lies below it.
func under(key, base string) bool {
	return key == base || strings.HasPrefix(key, strings.TrimSuffix(base, "/")+"/")
}

// GuardPin is the calling session's pin, as orchd recorded it.
type GuardPin struct {
	// Found is false when the caller has no pin.
	Found bool
	// Space is the pinned space. "", Quarantine or a space the policy no
	// longer has mean the session has no space: it is "another job" to
	// every isolated space.
	Space string
}

// GuardEnforcement is the enforcement state the guard reads (SP-070).
type GuardEnforcement struct {
	// Guard is the mode. "" is the default, GuardBlockOtherJobs; a value
	// the guard does not know is read as GuardStrict, so a damaged state
	// never enforces less.
	Guard GuardMode
}

// GuardEvent is one hook event, with its targets already extracted.
type GuardEvent struct {
	Kind GuardEventKind
	Tool string
	// Cwd is the directory the call runs in.
	Cwd string
	// Targets are the call's targets from Targets; for EventCwdChanged the
	// new directory is in CdDest.
	Targets ToolTargets
}

// GuardDecision is the guard's answer for one event.
type GuardDecision struct {
	// Verdict is what happens: Refuse only when the mode enforces and the
	// event can be refused.
	Verdict Verdict
	// Would is the verdict of the mode, or of the default mode when the
	// guard is off or logging: what the log records as "would have given".
	Would Verdict
	// Rule is the rule behind Would.
	Rule GuardRule
	// Mode is the mode the decision was made in.
	Mode GuardMode
	// Enforced is set when Would took effect.
	Enforced bool
}

// writeTools are file tools whose path fields are written. Shell tools'
// writes cannot be told from reads in Targets; SP-042 adds those.
var writeTools = map[string]bool{
	"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true, "apply_patch": true,
}

// limitReasons are the opaque reasons Targets gives when it stops at one
// of its own limits, or lacks the cwd or home it resolves paths against,
// rather than meeting a form it cannot read. Then even the targets it did
// find may be incomplete in ways an attacker chose, so an enforcing guard
// refuses (RC-36). TestDecideLimitReasons keeps this list in step with
// targets*.go.
var limitReasons = map[string]bool{
	"cwd is not absolute":               true,
	"home directory is unknown":         true,
	"analysis took too long":            true,
	"too many targets":                  true,
	"shell scripts nested too deep":     true,
	"path too long":                     true,
	"word too long to expand":           true,
	"text too long to search for paths": true,
	"script too large to read":          true,
	"too many symlinks":                 true,
	"too many brace expansions":         true,
}

// Decide decides one hook event (spaces plan SP-040). It is a pure
// function: it reads nothing but its arguments, so a logged event can be
// replayed and gives the same verdict and rule. Moving within the
// session's own space is allowed (D-09).
func Decide(pol GuardPolicy, pin GuardPin, enf GuardEnforcement, ev GuardEvent) GuardDecision {
	mode := enf.Guard
	switch mode {
	case "":
		mode = GuardBlockOtherJobs
	case GuardOff, GuardLog, GuardBlockOtherJobs, GuardStrict:
	default:
		mode = GuardStrict
	}
	judge := mode
	if !mode.Enforcing() {
		judge = GuardBlockOtherJobs
	}
	would, rule := assess(pol, pin, judge, ev)
	d := GuardDecision{Verdict: Allow, Would: would, Rule: rule, Mode: mode}
	// Reason: only a report of a move that already happened is exempt, so a
	// caller that misspells or omits the kind still gets enforcement.
	if mode.Enforcing() && ev.Kind != EventCwdChanged {
		d.Enforced = true
		d.Verdict = would
	}
	return d
}

// repoPattern is a repo as Targets reports it: "owner/repo".
var repoPattern = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9_-]*/[a-z0-9._-]+$`)

// assess is the verdict and rule of an enforcing mode.
func assess(pol GuardPolicy, pin GuardPin, mode GuardMode, ev GuardEvent) (Verdict, GuardRule) {
	res := pol.res
	if !res.Configured() {
		return Allow, RuleNoPolicy
	}
	if !pin.Found {
		return Refuse, RulePinMissing
	}
	own := pin.Space
	if _, ok := res.Space(own); !ok {
		own = ""
	}
	t := ev.Targets
	if writeTools[ev.Tool] && pol.anyProtected(t.Paths, t.CdDest, t.GitDirs, t.Prefixes) {
		return Refuse, RuleSelfProtection
	}
	g := guardScope{pol: pol, own: own, strict: mode == GuardStrict}

	// Reason: the order is fixed so the same targets always give the same
	// rule; the most specific refusal is reported first. Every target is
	// placed by the root holding it; those that name a directory a call
	// may search are also judged by the roots below them. The cwd is one
	// only in strict mode: a session started under a public root such as
	// ~/code would otherwise be refused every call.
	var exact, contain []string
	exact = append(exact, ev.Cwd)
	if g.strict {
		contain = append(contain, ev.Cwd)
	}
	contain = append(contain, t.CdDest...)
	contain = append(contain, t.Paths...)
	contain = append(contain, t.GitDirs...)
	contain = append(contain, t.Prefixes...)
	exact = append(exact, t.CdDest...)
	exact = append(exact, t.Paths...)
	exact = append(exact, t.GitDirs...)
	exact = append(exact, t.Prefixes...)

	// Reason: a refusal that does not end the scan keeps the rule ranked
	// first in the documented order, whatever order the targets come in.
	var weakest GuardRule
	note := func(r GuardRule) {
		if weakest == "" || (weakest == RuleMalformedTarget && r == RuleUnreadableSpace) {
			weakest = r
		}
	}
	for _, p := range exact {
		if !filepath.IsAbs(p) {
			note(RuleMalformedTarget)
			continue
		}
		r := g.judge(g.spaceOf(p))
		if g.otherJobHolds(p) {
			r = reachJob
		}
		switch r {
		case reachJob:
			return Refuse, RuleOtherJobPath
		case reachUnreadable:
			note(RuleUnreadableSpace)
		}
	}
	for _, p := range contain {
		switch g.contains(p) {
		case reachJob:
			return Refuse, RuleOtherJobAncestor
		case reachUnreadable:
			note(RuleUnreadableSpace)
		}
	}
	for _, r := range t.GHRepos {
		if !repoPattern.MatchString(r) {
			note(RuleMalformedTarget)
			continue
		}
		owner, _, _ := strings.Cut(r, "/")
		space, _ := res.GitHubOrgSpace(owner)
		switch g.judge(space) {
		case reachJob:
			return Refuse, RuleOtherJobRepo
		case reachUnreadable:
			note(RuleUnreadableSpace)
		}
	}
	if weakest != "" {
		return Refuse, weakest
	}
	if t.Opaque {
		for _, r := range t.OpaqueReasons {
			if limitReasons[r] {
				return Refuse, RuleAnalysisLimit
			}
		}
		if g.strict {
			return Refuse, RuleOpaqueStrict
		}
		return Allow, RuleOpaqueUnverified
	}
	return Allow, RuleWithinSpace
}

// guardScope is what one decision compares targets against.
type guardScope struct {
	pol    GuardPolicy
	own    string
	strict bool
}

// spaceOf is the space of the deepest root holding path, "" for none.
func (g guardScope) spaceOf(path string) string {
	k := guardKey(path)
	space, depth := "", -1
	for _, r := range g.pol.roots {
		for _, rk := range r.keys {
			if len(rk) > depth && under(k, rk) {
				space, depth = r.space, len(rk)
			}
		}
	}
	return space
}

// otherJobHolds reports whether any root of another isolated space holds
// path, not only the deepest. Validation keeps written roots from nesting
// across jobs, but a root reached through a symlink can resolve inside, or
// onto, another job's root; then the job's root still decides.
func (g guardScope) otherJobHolds(path string) bool {
	k := guardKey(path)
	for _, r := range g.pol.roots {
		for _, rk := range r.keys {
			if under(k, rk) && g.judge(r.space) == reachJob {
				return true
			}
		}
	}
	return false
}

// reach is how far a target is from the session's space.
type reach int

const (
	// reachOK: the session may reach it in this mode.
	reachOK reach = iota
	// reachUnreadable: strict mode refuses it; the session may not read
	// its space.
	reachUnreadable
	// reachJob: it is in another isolated space.
	reachJob
)

// judge places a target's space. A target in no space is allowed: the
// guard separates spaces, and other layers cover the rest of the machine.
func (g guardScope) judge(space string) reach {
	if space == "" || space == g.own {
		return reachOK
	}
	res := g.pol.res
	s, ok := res.Space(space)
	if !ok {
		return reachOK
	}
	if s.Kind == Isolated {
		return reachJob
	}
	if g.strict && s.Kind != Public && (g.own == "" || !res.CanRead(g.own, space)) {
		return reachUnreadable
	}
	return reachOK
}

// contains judges the roots strictly below path, the furthest first: a
// call naming a directory may reach everything below it.
func (g guardScope) contains(path string) reach {
	if !filepath.IsAbs(path) {
		return reachOK
	}
	k := guardKey(path)
	out := reachOK
	for _, r := range g.pol.roots {
		for _, rk := range r.keys {
			if rk != k && under(rk, k) {
				out = max(out, g.judge(r.space))
			}
		}
	}
	return out
}

// anyProtected reports whether a path is, or lies under, a protected path.
func (pol GuardPolicy) anyProtected(lists ...[]string) bool {
	for _, l := range lists {
		for _, p := range l {
			k := guardKey(p)
			for _, keys := range pol.protected {
				for _, q := range keys {
					if under(k, q) {
						return true
					}
				}
			}
		}
	}
	return false
}

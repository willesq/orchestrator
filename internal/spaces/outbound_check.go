package spaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
)

// The outbound terms check (spaces plan SP-044): an outbound action is
// refused when the text it sends holds a term of a space one of its
// possible destinations (the target repo's owner) does not hold. Text is
// the command's own words, the files it sends, a commit's staged changes,
// and the commits and tag messages a push sends.

// Rules of the outbound terms check. Like the guard's, they never name the
// matched text.
const (
	// RuleOutboundTerm: the text sent holds another space's term.
	RuleOutboundTerm GuardRule = "outbound_term"
	// RuleOutboundUnchecked: the text sent could not all be read (a
	// variable, an alias, an unreadable file or terms file, git failing
	// or timing out), so the check fails closed.
	RuleOutboundUnchecked GuardRule = "outbound_unchecked"
)

// maxOutboundText bounds what is read of one source of outbound text: a
// file, a staged diff, the commits of a push. Reason: matching every term
// over it must end well within the hook's time (termsTimeout), so a
// larger source is refused as unchecked.
const maxOutboundText = 4 << 20

// OutboundResult is the check's answer for one tool call.
type OutboundResult struct {
	Refuse bool
	Rule   GuardRule
	// Hit names the term found, for RuleOutboundTerm.
	Hit TermHit
	// Kind is the action refused; Dest the space its destination holds,
	// "" for none (or, with several possible destinations, the first that
	// does not hold the term).
	Kind, Dest string
	// UserRule is the id of the operator's rule that refused the call
	// (SP-075), "" for the baseline. Only its id is ever shown.
	UserRule string
}

// UserCommandKind is the kind of an action a command rule (SP-075) names.
// Reason: it never names the program, which is the rule's value (INV-07).
const UserCommandKind = "a command an outbound rule names"

// userFailKind is the kind of a call a fail_closed rule refused.
const userFailKind = "a call an outbound rule checks"

// Reason is the refusal as the session sees it: the rule, the action and,
// for a term, its space, line and hash; never the term.
func (r OutboundResult) Reason() string {
	dest := "a destination that holds no space"
	if r.Dest != "" {
		dest = fmt.Sprintf("space %q", r.Dest)
	}
	switch {
	case r.Rule == RuleOutboundUnchecked && r.UserRule != "":
		return fmt.Sprintf("orch outbound terms refused %s (rule %s): the operator's outbound rule %s refuses it. Ask the operator, or run it yourself outside the session.",
			safeKind(r.Kind), r.Rule, r.UserRule)
	case r.Rule == RuleOutboundUnchecked && r.Kind == unreadableRulesKind:
		return fmt.Sprintf("orch outbound terms refused %s (rule %s): the operator's outbound rules cannot be read, so nothing that may send text out can be checked. Ask the operator to look at 'orch spaces terms rules list'.",
			safeKind(r.Kind), r.Rule)
	case r.Rule == RuleOutboundTerm:
		return fmt.Sprintf("orch outbound terms refused %s (rule %s): it would send a term of %s to %s. Remove it and try again.",
			safeKind(r.Kind), r.Rule, r.Hit, dest)
	case r.Rule == RuleOutboundUnchecked:
		return fmt.Sprintf("orch outbound terms refused %s (rule %s): what it sends to %s could not all be checked for other spaces' terms (a variable, command output or alias, another repository named through git's options or environment, an unreadable file or terms file, or git failing). Write the text out in the command or a file and try again.",
			safeKind(r.Kind), r.Rule, dest)
	}
	return ""
}

// OutboundChecker checks outbound actions against the spaces' terms.
type OutboundChecker struct {
	Policy *Resolved
	// Terms are the spaces' parsed terms; TermErrs the spaces whose terms
	// file could not be read, which fail closed.
	Terms    []TermList
	TermErrs map[string]error
	// Home expands ~ in cd destinations.
	Home string
	// Getenv reads the hook's own environment (GH_REPO); nil is os.Getenv.
	// Reason: git variables need no check here; the git the check runs
	// inherits the same environment as the command's.
	Getenv func(string) string
	// Git runs git in dir and returns its output; nil runs the git binary.
	Git func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// Rules are the operator's outbound rules (SP-075); nil is the
	// baseline, every rule on and none added. RulesBroken is set when
	// orchd could not read them.
	Rules       *OutboundRules
	RulesBroken bool

	// rules is Rules compiled, set by Check.
	rules *ruleSet
}

// unreadableRulesKind is the kind of a call refused because the rules
// could not be read.
const unreadableRulesKind = "a call the operator's outbound rules may cover"

// rulesMayCover reports whether rules that cannot be read could have
// refused a call by tool: any shell command (a command rule may name any
// program, a fail_closed rule any text) and any MCP tool. Local file tools
// send nothing out.
func rulesMayCover(tool string, input []byte) bool {
	if _, _, ok := shellCommand(tool, input); ok {
		return true
	}
	return strings.HasPrefix(tool, "mcp__")
}

// Check checks one tool call.
func (c OutboundChecker) Check(ctx context.Context, tool string, input []byte, cwd string) OutboundResult {
	if len(c.Terms) == 0 && len(c.TermErrs) == 0 {
		return OutboundResult{}
	}
	rules, rerr := c.Rules.compile()
	if rerr != nil || c.RulesBroken {
		// Reason: rules that cannot be read cannot be vouched for, as
		// terms that cannot be read, and the operator's stricter rules
		// may have covered any shell command or MCP tool; a stored rule
		// set is checked when saved, so this is damage.
		if rulesMayCover(tool, input) && c.applies(nil) {
			return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: unreadableRulesKind}
		}
		return OutboundResult{}
	}
	c.rules = rules
	acts, unknown, words := readOutbound(tool, input, cwd, c.Home, rules)
	if len(acts) == 0 && !unknown && (rules == nil || len(rules.fail) == 0) {
		return OutboundResult{}
	}
	// Reason: the text as written, decoded, so a term after a newline or
	// tab escape is still on a word boundary; and the words as the shell
	// passes them, so a term split by quoting ('fal''con') is found.
	text := JSONText(input) + "\n" + strings.Join(words, "\n")
	if rules != nil && len(rules.fail) > 0 && c.applies(nil) {
		// Reason: matching every pattern must end within the hook's
		// time, so a larger call is refused, as a larger file is.
		if len(text) > maxOutboundText {
			return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: userFailKind, UserRule: rules.fail[0].id}
		}
		// The operator's fail_closed rules see the tool's name too, so
		// one can name a tool that is not a shell (an MCP tool).
		if id := rules.failClosed(ctx.Done(), tool+"\n"+text); id != "" {
			return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: userFailKind, UserRule: id}
		}
		if ctx.Err() != nil {
			return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: userFailKind}
		}
	}
	if len(acts) == 0 {
		if !unknown {
			return OutboundResult{}
		}
		// A command that may send something out but could not be read.
		r := OutboundResult{Kind: "a command that may send text out"}
		if hit, ok := c.match(ctx, text, nil); ok {
			r.Refuse, r.Rule, r.Hit = true, RuleOutboundTerm, hit
		} else if c.applies(nil) && rules.on(OutboundFailClosed) {
			r.Refuse, r.Rule = true, RuleOutboundUnchecked
		}
		return r
	}
	for _, a := range acts {
		if r := c.checkAction(ctx, a, text, unknown); r.Refuse {
			return r
		}
	}
	return OutboundResult{}
}

func (c OutboundChecker) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c OutboundChecker) checkAction(ctx context.Context, a Outbound, text string, unknown bool) OutboundResult {
	dests, known := c.destinations(ctx, a)
	r := OutboundResult{Kind: a.Kind}
	if len(dests) > 0 {
		r.Dest = dests[0]
	}
	if !c.applies(dests) {
		return r
	}
	refuse := func(rule GuardRule) OutboundResult {
		r.Refuse, r.Rule = true, rule
		return r
	}
	if hit, ok := c.match(ctx, text, dests); ok {
		r.Hit, r.Dest = hit, firstOther(dests, hit.Space)
		return refuse(RuleOutboundTerm)
	}
	if ctx.Err() != nil {
		return refuse(RuleOutboundUnchecked)
	}
	for space := range c.TermErrs {
		if !allHold(dests, space) {
			return refuse(RuleOutboundUnchecked)
		}
	}
	// Baseline rule fail_closed_unreadable: what cannot be read is
	// refused. Switched off, what can be read is still checked, and the
	// rest is let through.
	failClosed := c.rules.on(OutboundFailClosed)
	if !known || unknown || a.Dir == "" {
		if failClosed {
			return refuse(RuleOutboundUnchecked)
		}
		if a.Dir == "" {
			return OutboundResult{}
		}
	}
	if a.Alias != "" {
		out, err := c.git(ctx, a.Dir, "config", "--get", "alias."+a.Alias)
		if err == nil && strings.TrimSpace(string(out)) != "" && failClosed {
			return refuse(RuleOutboundUnchecked)
		}
		return OutboundResult{}
	}
	texts, err := c.sent(ctx, a)
	for _, t := range texts {
		if ctx.Err() != nil {
			return refuse(RuleOutboundUnchecked)
		}
		if hit, ok := c.match(ctx, t, dests); ok {
			r.Hit, r.Dest = hit, firstOther(dests, hit.Space)
			return refuse(RuleOutboundTerm)
		}
	}
	if err != nil && failClosed || ctx.Err() != nil {
		return refuse(RuleOutboundUnchecked)
	}
	return r
}

// allHold reports whether every destination holds space.
func allHold(dests []string, space string) bool {
	for _, d := range dests {
		if d != space {
			return false
		}
	}
	return len(dests) > 0
}

func firstOther(dests []string, space string) string {
	for _, d := range dests {
		if d != space {
			return d
		}
	}
	return ""
}

// applies reports whether a space not held by every destination has terms.
func (c OutboundChecker) applies(dests []string) bool {
	for _, l := range c.Terms {
		if !allHold(dests, l.Space) && l.Len() > 0 {
			return true
		}
	}
	for space := range c.TermErrs {
		if !allHold(dests, space) {
			return true
		}
	}
	return false
}

// match finds a term of a space not held by every destination in text.
// It stops, finding nothing, when ctx ends; the caller then fails closed.
func (c OutboundChecker) match(ctx context.Context, text string, dests []string) (TermHit, bool) {
	for _, l := range c.Terms {
		if allHold(dests, l.Space) {
			continue
		}
		if hit, ok := l.matchUntil(ctx.Done(), text); ok {
			return hit, true
		}
	}
	return TermHit{}, false
}

// destinations are the spaces a's text may reach, and whether they could
// be told: the repo or org the command names; else, for git, the
// checkout's push remote; for gh, the repo gh picks (ghDefaultURLs).
func (c OutboundChecker) destinations(ctx context.Context, a Outbound) ([]string, bool) {
	space := c.destSpace
	switch {
	case a.NoRepo:
		return []string{""}, true
	case a.Repo != "":
		owner, repo, _ := strings.Cut(a.Repo, "/")
		return []string{space(owner, repo)}, true
	case a.Org != "":
		return []string{space(a.Org, "")}, true
	case a.Dir == "":
		return nil, false
	case a.GHDefault:
		urls, ok := c.ghDefaultURLs(ctx, a.Dir, a.GHEnv, a.GHUnmarked)
		if !ok {
			return nil, false
		}
		return urlSpaces(urls, space), true
	}
	url, err := c.remoteURL(ctx, a)
	if err != nil {
		return nil, false
	}
	return urlSpaces(strings.Split(url, "\n"), space), true
}

// destSpace is the space a GitHub destination holds for the check: its
// owner's, or none when an operator's destination rule (SP-075) names the
// owner or the repo.
func (c OutboundChecker) destSpace(owner, repo string) string {
	if c.rules.destination(owner, repo) != "" {
		return ""
	}
	s, _ := c.Policy.GitHubOrgSpace(owner)
	return s
}

// urlSpaces are the spaces of remote URLs; a URL that is not GitHub, or
// none at all, holds no space.
func urlSpaces(urls []string, space func(owner, repo string) string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, u := range urls {
		if owner, repo, ok := githubOwnerRepo(u); ok {
			add(space(owner, repo))
		} else {
			add("")
		}
	}
	if len(out) == 0 {
		add("")
	}
	return out
}

// isGitHubHost reports whether host is GitHub: github.com and its
// subdomains, or an ssh alias (a name without dots) that says github, as
// per-account setups use.
func isGitHubHost(host string) bool {
	h := strings.ToLower(host)
	return h == "github.com" || strings.HasSuffix(h, ".github.com") ||
		!strings.Contains(h, ".") && strings.Contains(h, "github")
}

// githubOwnerRepo is the owner and repo of a GitHub remote URL.
func githubOwnerRepo(url string) (owner, repo string, ok bool) {
	host := ""
	if m := urlRemote.FindStringSubmatch(url); m != nil {
		host, owner, repo = m[1], m[2], m[3]
	} else if m := scpRemote.FindStringSubmatch(url); m != nil {
		host, owner, repo = m[2], m[3], m[4]
	}
	return owner, repo, owner != "" && isGitHubHost(host)
}

// remoteURL is the push URL of the remote a's checkout sends to: the one a
// push names, else the branch's push remote, remote.pushDefault, the
// branch's remote, then origin. "" when the checkout has none.
func (c OutboundChecker) remoteURL(ctx context.Context, a Outbound) (string, error) {
	remote, err := c.pushRemote(ctx, a)
	if err != nil || isURLRemote(remote) {
		return remote, err
	}
	out, err := c.git(ctx, a.Dir, "remote", "get-url", "--push", "--all", remote)
	if err != nil {
		var ee *osexec.ExitError
		if errors.As(err, &ee) {
			// git says there is no such remote.
			return "", nil
		}
		return "", err
	}
	// Reason: a remote may push to several URLs; each is a destination.
	return strings.Join(strings.Fields(string(out)), "\n"), nil
}

// pushRemote is the remote a push sends to.
func (c OutboundChecker) pushRemote(ctx context.Context, a Outbound) (string, error) {
	if a.Remote != "" {
		return a.Remote, nil
	}
	branch, _ := c.git(ctx, a.Dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	b := strings.TrimSpace(string(branch))
	for _, key := range []string{"branch." + b + ".pushRemote", "remote.pushDefault", "branch." + b + ".remote"} {
		if b == "" && strings.HasPrefix(key, "branch.") {
			continue
		}
		if out, err := c.git(ctx, a.Dir, "config", "--get", key); err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "origin", ctx.Err()
}

// sent reads what a sends besides its command line: its files, a commit's
// staged changes, a push's commits and tag messages.
func (c OutboundChecker) sent(ctx context.Context, a Outbound) ([]string, error) {
	var texts []string
	for _, f := range a.Files {
		if f == "-" {
			return texts, errors.New("text read from stdin")
		}
		b, err := readSmall(f, maxOutboundText+1)
		if err != nil || len(b) > maxOutboundText {
			return texts, errors.New("a file it sends cannot be read")
		}
		texts = append(texts, string(b))
	}
	diff := []string{"--no-ext-diff", "--no-textconv", "--no-color", "--unified=0"}
	if a.Kind == "git tag" || a.Kind == "git notes" {
		// The tagger or the notes commit's author is sent too.
		texts = append(texts, c.identity(ctx, a.Dir))
	}
	switch {
	case a.Staged:
		out, err := c.git(ctx, a.Dir, append([]string{"diff", "--cached"}, diff...)...)
		texts = append(texts, string(out))
		if err != nil {
			return texts, err
		}
		texts = append(texts, c.identity(ctx, a.Dir))
		bin, err := c.blobTexts(ctx, a.Dir, true, "diff", "--cached", "--numstat", "--no-renames")
		texts = append(texts, bin...)
		if err != nil {
			return texts, err
		}
		if a.All {
			// Reason: tracked changes not staged yet; clean filters, which
			// a repository can make run anything, are switched off.
			args, err := c.noFilters(ctx, a.Dir)
			if err != nil {
				return texts, err
			}
			out, err := c.git(ctx, a.Dir, append(append(args, "diff"), diff...)...)
			texts = append(texts, string(out))
			if err != nil {
				return texts, err
			}
			bin, err := c.blobTexts(ctx, a.Dir, false, append(args, "diff", "--numstat", "--no-renames")...)
			texts = append(texts, bin...)
			if err != nil {
				return texts, err
			}
		}
	case a.Push:
		t, err := c.pushed(ctx, a, diff)
		texts = append(texts, t...)
		if err != nil {
			return texts, err
		}
	}
	if a.Pending {
		t, err := c.pending(ctx, a.Dir, diff)
		texts = append(texts, t...)
		if err != nil {
			return texts, err
		}
	}
	return texts, nil
}

// git runs git in dir.
func (c OutboundChecker) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if c.Git != nil {
		return c.Git(ctx, dir, args...)
	}
	return RunGit(ctx, dir, args...)
}

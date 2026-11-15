package spaces

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// The git hook helper (spaces plan SP-045): commit-msg and pre-push hooks
// that `orch guard git-hook --install` adds to a repository run the
// outbound terms check (SP-044) on what git itself is about to commit or
// push. They see every commit and push in the repository, whoever makes
// it: an agent's command the tool hook could not read, a script that runs
// git, or the user's own terminal (RC-117, D-60). A refusal names the
// term's space, line and hash, never the term (INV-07).

// Git hook names the check runs as.
const (
	GitHookCommitMsg = "commit-msg"
	GitHookPrePush   = "pre-push"
)

// GitHookTimeout bounds one hook's check, git included. Reason: unlike the
// tool hook, no harness kills a git hook, but a check that hangs would
// hang the user's commit; a check that runs out refuses (fails closed).
const GitHookTimeout = 20 * time.Second

// maxPushUpdates bounds the ref updates read from pre-push's input.
// Reason: each one adds a git log argument; a push of more refs than this
// is refused as unchecked rather than read without bound.
const maxPushUpdates = 4096

// ReadTerms reads what the outbound terms check needs from the guard
// files, as Decide does for the tool hook (RC-116): the terms layer's
// switch, the applied policy and its home. Known is unset when the policy
// cannot be read; the check then refuses (fails closed).
func (f GuardFiles) ReadTerms(ctx context.Context) GuardTerms {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	snap, err := ReadGuardSnapshot(f.Dir)
	if err != nil {
		return f.unreadable(ctx, err, now()).Terms
	}
	terms := GuardTerms{State: snap.TermsState(now()), Home: snap.Home}
	if snap.Broken {
		return terms
	}
	var res *Resolved
	if snap.Policy != nil {
		// Reason: as in decide; orchd validated the policy at apply.
		r, verr := Validate(*snap.Policy, Env{Home: snap.Home, InGitTree: func(string) bool { return false }})
		if verr != nil {
			return terms
		}
		res = r
	}
	terms.Known, terms.Policy = true, res
	return terms
}

// PushUpdate is one line of pre-push's input: a local ref and object it
// pushes, and the remote ref and the object the remote holds there.
type PushUpdate struct {
	LocalRef, LocalSHA, RemoteRef, RemoteSHA string
}

var objectName = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// zeroObject reports whether sha is git's all-zero object name: a deleted
// ref, or a remote ref that does not exist yet.
func zeroObject(sha string) bool {
	return strings.Trim(sha, "0") == ""
}

// ParsePushUpdates reads pre-push's input. A line that is not four fields
// with object names is an error, so the check never guesses what a push
// sends.
func ParsePushUpdates(r io.Reader) ([]PushUpdate, error) {
	var ups []PushUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 || !objectName.MatchString(f[1]) || !objectName.MatchString(f[3]) {
			return nil, errors.New("pre-push input line is not <local ref> <local object> <remote ref> <remote object>")
		}
		if len(ups) == maxPushUpdates {
			return nil, fmt.Errorf("pre-push input has over %d ref updates", maxPushUpdates)
		}
		ups = append(ups, PushUpdate{LocalRef: f[0], LocalSHA: f[1], RemoteRef: f[2], RemoteSHA: f[3]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ups, nil
}

// GitHookInput is what a git hook was given.
type GitHookInput struct {
	// Hook is GitHookCommitMsg or GitHookPrePush.
	Hook string
	// Dir is the directory git ran the hook in (the work tree's top, or
	// the git directory of a bare repository).
	Dir string
	// MsgFile is commit-msg's message file, absolute.
	MsgFile string
	// Remote and URL are pre-push's arguments: the remote's name (the URL
	// itself for a push to a URL) and the URL pushed to.
	Remote, URL string
	// Updates are pre-push's ref updates.
	Updates []PushUpdate
}

// CheckGitHook checks what a commit-msg or pre-push hook is about to let
// git send, against the terms g names. With the terms layer off it allows;
// when g's policy is not known it refuses.
func CheckGitHook(ctx context.Context, g GuardTerms, in GitHookInput) OutboundResult {
	kind := "git commit"
	if in.Hook == GitHookPrePush {
		kind = "git push"
	}
	if g.State == SwitchOff {
		return OutboundResult{}
	}
	if !g.Known {
		return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: kind}
	}
	home, _ := os.UserHomeDir()
	termsHome := g.Home
	if termsHome == "" {
		termsHome = home
	}
	lists, errs := SpaceTerms(g.Policy, termsHome)
	c := OutboundChecker{Policy: g.Policy, Terms: lists, TermErrs: errs, Home: home}
	return c.CheckGitHook(ctx, in)
}

// CheckGitHook checks one git hook's commit or push.
func (c OutboundChecker) CheckGitHook(ctx context.Context, in GitHookInput) OutboundResult {
	if len(c.Terms) == 0 && len(c.TermErrs) == 0 {
		return OutboundResult{}
	}
	switch in.Hook {
	case GitHookCommitMsg:
		// Reason: git runs commit-msg with GIT_INDEX_FILE naming the index
		// the commit is made from (commit -a and commit <paths> use a
		// temporary one), and RunGit keeps it, so the staged diff read is
		// exactly what the commit holds.
		a := Outbound{Kind: "git commit", Dir: in.Dir, Files: []string{in.MsgFile}, Staged: true}
		if in.MsgFile == "" {
			return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: a.Kind}
		}
		return c.checkAction(ctx, a, "", false)
	case GitHookPrePush:
		return c.checkPush(ctx, in)
	}
	return OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: "a git hook orch does not know"}
}

// maxTagDepth bounds how many tags of tags are read for one pushed object.
const maxTagDepth = 8

// checkPush checks a pre-push hook's push: the names of the refs it makes,
// the tag objects it sends, and the commits and tag messages its ref
// updates send, less what the destination already holds.
func (c OutboundChecker) checkPush(ctx context.Context, in GitHookInput) OutboundResult {
	unchecked := OutboundResult{Refuse: true, Rule: RuleOutboundUnchecked, Kind: "git push"}
	// Reason: the destination is the URL git hands the hook, after
	// insteadOf, pushurl and any -c override on git's command line; the
	// remote's config, which the check's own git reads without those
	// overrides, may name another (SP-045 review).
	url := in.URL
	if url == "" || strings.HasPrefix(url, "-") {
		return unchecked
	}
	if !isURLRemote(url) {
		// A bare word is a directory relative to the work tree.
		url = "./" + url
	}
	a := Outbound{Kind: "git push", Dir: in.Dir, Push: true, Remote: url, ExcludeAll: true}
	var refs []string
	for _, u := range in.Updates {
		if zeroObject(u.LocalSHA) {
			// A deletion sends no commits, and the remote has the name.
			continue
		}
		// Reason: the remote ref's name is sent and made on the remote.
		refs = append(refs, u.RemoteRef)
		a.Refs = append(a.Refs, u.LocalSHA)
		if tag, ok := strings.CutPrefix(u.LocalRef, "refs/tags/"); ok && tag != "" {
			a.Tags = append(a.Tags, tag)
		}
		if !zeroObject(u.RemoteSHA) {
			// Reason: commits the remote's ref already holds are not sent;
			// one this checkout does not have cannot be excluded, and its
			// ancestors are then read too (stricter, never looser).
			if _, err := c.git(ctx, in.Dir, "cat-file", "-e", u.RemoteSHA+"^{commit}"); err == nil {
				a.Have = append(a.Have, u.RemoteSHA)
			}
		}
	}
	if len(a.Refs) == 0 {
		return OutboundResult{}
	}
	tags, err := c.tagObjects(ctx, in.Dir, a.Refs)
	if err != nil {
		return unchecked
	}
	if a.HaveRemotes, err = c.sameSpaceRemotes(ctx, in.Dir, url); err != nil {
		return unchecked
	}
	return c.checkAction(ctx, a, strings.Join(refs, "\n")+"\n"+strings.Join(tags, "\n"), false)
}

// tagObjects reads the tag objects among the pushed objects, and the tags
// they in turn point at: their messages and taggers are sent with them,
// whatever ref name pushes them.
func (c OutboundChecker) tagObjects(ctx context.Context, dir string, objects []string) ([]string, error) {
	var texts []string
	for _, obj := range objects {
		for depth := 0; ; depth++ {
			kind, err := c.git(ctx, dir, "cat-file", "-t", obj)
			if err != nil {
				return texts, err
			}
			if strings.TrimSpace(string(kind)) != "tag" {
				break
			}
			if depth == maxTagDepth {
				return texts, errors.New("tags of tags nested too deep")
			}
			body, err := c.git(ctx, dir, "cat-file", "tag", obj)
			if err != nil {
				return texts, err
			}
			texts = append(texts, string(body))
			first, _, _ := strings.Cut(string(body), "\n")
			next, ok := strings.CutPrefix(first, "object ")
			if !ok || !objectName.MatchString(next) {
				return texts, errors.New("a pushed tag names no object")
			}
			obj = next
		}
	}
	return texts, nil
}

// sameSpaceRemotes are the remotes whose remote-tracking refs a push to url
// need not send again: those whose fetch URLs all lie in the spaces url
// lies in. Reason: tracking refs hold what was fetched, from the fetch
// URLs, not what a remote's push URL (or an override) holds (SP-045
// review). A destination in no space is one place only, so only remotes
// fetched from that very URL count for it: other servers and paths in no
// space may hold anything. A name git would read as a pattern is left out.
func (c OutboundChecker) sameSpaceRemotes(ctx context.Context, dir, url string) ([]string, error) {
	space := func(owner string) string {
		s, _ := c.Policy.GitHubOrgSpace(owner)
		return s
	}
	want := urlSpaces([]string{url}, space)
	out, err := c.git(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	var same []string
	for _, name := range strings.Fields(string(out)) {
		if strings.ContainsAny(name, "*?[\\") || strings.HasPrefix(name, "-") {
			continue
		}
		urls, err := c.git(ctx, dir, "remote", "get-url", "--all", name)
		if err != nil {
			continue
		}
		fetched := strings.Fields(string(urls))
		if slicesContains(want, "") {
			if len(fetched) > 0 && allEqual(fetched, url) {
				same = append(same, name)
			}
			continue
		}
		if sameSpaces(urlSpaces(fetched, space), want) {
			same = append(same, name)
		}
	}
	return same, nil
}

// allEqual reports whether every string in list is s.
func allEqual(list []string, s string) bool {
	for _, x := range list {
		if x != s {
			return false
		}
	}
	return true
}

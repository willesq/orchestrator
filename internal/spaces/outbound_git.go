package spaces

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"
)

// How the outbound terms check (SP-044) asks git what a commit or push
// sends, without letting the repository run anything.

// noFilters are -c settings that switch off every filter driver the
// checkout's config defines.
func (c OutboundChecker) noFilters(ctx context.Context, dir string) ([]string, error) {
	out, err := c.git(ctx, dir, "config", "--name-only", "--get-regexp", `^filter\.`)
	var ee *osexec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, err
	}
	var args []string
	seen := map[string]bool{}
	for _, key := range strings.Fields(string(out)) {
		name := strings.TrimSuffix(strings.TrimPrefix(key, "filter."), "."+key[strings.LastIndex(key, ".")+1:])
		if seen[name] || name == "" {
			continue
		}
		seen[name] = true
		for _, k := range []string{"clean", "smudge", "process"} {
			args = append(args, "-c", "filter."+name+"."+k+"=")
		}
		args = append(args, "-c", "filter."+name+".required=false")
	}
	return args, nil
}

// pushed reads the commits and tag messages a push sends: the refs it
// names (the current branch, or every branch when the remote's push
// refspec or push.default says so), less what the destination remote's
// tracking refs already hold.
func (c OutboundChecker) pushed(ctx context.Context, a Outbound, diff []string) ([]string, error) {
	remote, err := c.pushRemote(ctx, a)
	if err != nil {
		return nil, err
	}
	refs, tags := a.Refs, a.Tags
	cfg := func(key string) string {
		out, _ := c.git(ctx, a.Dir, "config", "--get-all", key)
		return strings.TrimSpace(string(out))
	}
	if !isURLRemote(remote) {
		if cfg("remote."+remote+".mirror") == "true" {
			refs, tags = append(refs, "--all"), append(tags, "*")
		}
		if spec := cfg("remote." + remote + ".push"); spec != "" && len(a.Refs) == 0 {
			refs = append(refs, "--branches")
			if strings.Contains(spec, "refs/tags") || strings.Contains(spec, "refs/notes") || strings.Contains(spec, "*:") && !strings.Contains(spec, "refs/heads") {
				refs, tags = append(refs, "--all"), append(tags, "*")
			}
		}
	}
	if len(a.Refs) == 0 {
		refs = append(refs, "HEAD")
		if cfg("push.default") == "matching" {
			refs = append(refs, "--branches")
		}
	}
	if cfg("push.followTags") == "true" {
		tags = append(tags, "*")
	}
	var not []string
	if !a.ExcludeAll {
		if not, err = c.excluded(ctx, a, remote); err != nil {
			return nil, err
		}
	}
	for _, h := range a.Have {
		if !objectName.MatchString(h) {
			return nil, errors.New("a commit the destination holds is not an object name")
		}
		not = append(not, h)
	}
	for _, r := range a.HaveRemotes {
		not = append(not, "--remotes="+r)
	}
	// Reason: the author's and committer's names and emails are sent with
	// each commit; --cc shows what a merge itself adds.
	args := append([]string{"log", "-p", "--cc", "--no-show-signature", "--format=%an <%ae>%n%cn <%ce>%n%B"}, diff...)
	if a.Pending {
		// Reason: a branch or tag made earlier in the same call does not
		// exist yet; its content is in what pending reads.
		args = append(args, "--ignore-missing")
	}
	for _, ref := range refs {
		if strings.HasPrefix(ref, "-") && ref != "--branches" && ref != "--all" {
			return nil, errors.New("a pushed ref looks like an option")
		}
		args = append(args, ref)
	}
	if slicesContains(tags, "*") {
		// Reason: commits only a tag reaches are pushed with --tags.
		args = append(args, "--tags")
	}
	if len(not) > 0 {
		args = append(append(args, "--not"), not...)
	}
	out, err := c.git(ctx, a.Dir, args...)
	texts := []string{string(out)}
	if err != nil {
		return texts, err
	}
	if err := c.utf8Commits(ctx, a.Dir, args); err != nil {
		return texts, err
	}
	if err := c.commitsOnly(ctx, a.Dir, refs, tags, a.Pending); err != nil {
		return texts, err
	}
	bin, err := c.blobTexts(ctx, a.Dir, false, c.numstatArgs(args)...)
	texts = append(texts, bin...)
	if err != nil {
		return texts, err
	}
	for _, tag := range tags {
		pattern := "refs/tags/" + tag
		if tag == "*" {
			pattern = "refs/tags"
		}
		if strings.HasPrefix(tag, "-") {
			return texts, errors.New("a pushed tag looks like an option")
		}
		out, err := c.git(ctx, a.Dir, "for-each-ref", "--format=%(taggername) %(taggeremail)%0a%(contents)", pattern)
		texts = append(texts, string(out))
		if err != nil {
			return texts, err
		}
	}
	return texts, nil
}

// numstatArgs turn the push's log args into a listing of the binary files
// of each commit it sends.
func (c OutboundChecker) numstatArgs(logArgs []string) []string {
	args := []string{"log", "--numstat", "--no-renames", "--format=%x01%H"}
	for _, a := range logArgs[1:] {
		if a == "-p" || a == "--cc" || strings.HasPrefix(a, "--format=") || strings.HasPrefix(a, "--unified") ||
			a == "--no-ext-diff" || a == "--no-textconv" || a == "--no-color" || a == "--no-show-signature" {
			continue
		}
		args = append(args, a)
	}
	return args
}

// utf8Commits fails when a commit the log args list declares an encoding
// other than UTF-8: git re-encodes its message for the log, so a term in
// it would not be read as written.
func (c OutboundChecker) utf8Commits(ctx context.Context, dir string, logArgs []string) error {
	args := []string{"log", "--format=%e"}
	for _, a := range logArgs[1:] {
		// Keep the refs and options that pick the commits, not the patch.
		if a == "-p" || a == "--cc" || strings.HasPrefix(a, "--format=") || a == "--text" ||
			strings.HasPrefix(a, "--unified") || a == "--no-ext-diff" || a == "--no-textconv" || a == "--no-color" {
			continue
		}
		args = append(args, a)
	}
	out, err := c.git(ctx, dir, args...)
	if err != nil {
		return err
	}
	for _, e := range strings.Fields(string(out)) {
		if l := strings.ToLower(e); l != "utf-8" && l != "utf8" {
			return errors.New("a pushed commit declares another encoding")
		}
	}
	return nil
}

// identity is who a commit made now would name as author and committer.
// Reason: a commit that fails for want of one sends nothing, so an error
// adds no text rather than failing closed.
func (c OutboundChecker) identity(ctx context.Context, dir string) string {
	var b strings.Builder
	for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		out, _ := c.git(ctx, dir, "var", v)
		b.Write(out)
	}
	return b.String()
}

// gitScrubbed are variables RunGit drops from git's environment: they
// would let the calling process change what git runs or reads.
var gitScrubbed = []string{"GIT_EXEC_PATH", "GIT_EXTERNAL_DIFF", "GIT_PAGER", "GIT_SSH", "GIT_SSH_COMMAND",
	"GIT_ASKPASS", "GIT_EDITOR", "GIT_TRACE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_DIFF_OPTS"}

// RunGit runs the git binary in dir with settings that keep it from
// running anything the repository configures (fsmonitor, external diff
// drivers, signature checks, pagers) and bounds its output and its wait.
func RunGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// Reason: the repository's own config must not change what the check
	// reads: replace refs, a hidden root commit, paths relative to a
	// subdirectory, quoted non-ASCII paths and re-encoded messages.
	full := append([]string{"--no-replace-objects", "-C", dir, "-c", "core.fsmonitor=false", "-c", "diff.external=",
		"-c", "core.pager=cat", "-c", "log.showSignature=false", "-c", "gpg.program=false", "-c", "core.sshCommand=false",
		"-c", "log.showRoot=true", "-c", "diff.relative=false", "-c", "core.quotePath=false",
		"-c", "i18n.logOutputEncoding=UTF-8"}, args...)
	cmd := osexec.CommandContext(ctx, "git", full...)
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !slicesContains(gitScrubbed, k) && !strings.HasPrefix(k, "GIT_CONFIG_KEY_") && !strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	// Reason: a child git starts that keeps stdout open must not hold the
	// hook past its deadline.
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, limit: maxOutboundText}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

// limitedBuffer fails a write past limit, which stops git.
type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int
}

var errOutboundTooLarge = errors.New("output too large to check")

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.limit {
		return 0, errOutboundTooLarge
	}
	return l.buf.Write(p)
}

// isURLRemote reports whether a push's remote is a URL or path rather
// than a remote's name.
func isURLRemote(remote string) bool {
	return strings.ContainsAny(remote, "/:")
}

// excluded are the remote-tracking refs whose commits a push to remote
// does not send again: a named remote's own, or for a URL those of every
// remote whose push URLs all lie in the same spaces as the URL's (none
// when no remote does), so work already on another space's remote still
// counts (SP-044 review).
func (c OutboundChecker) excluded(ctx context.Context, a Outbound, remote string) ([]string, error) {
	if !isURLRemote(remote) {
		return []string{"--remotes=" + remote}, nil
	}
	space := func(owner string) string {
		s, _ := c.Policy.GitHubOrgSpace(owner)
		return s
	}
	want := urlSpaces([]string{remote}, space)
	out, err := c.git(ctx, a.Dir, "remote")
	if err != nil {
		return nil, err
	}
	var not []string
	for _, name := range strings.Fields(string(out)) {
		urls, err := c.git(ctx, a.Dir, "remote", "get-url", "--push", "--all", name)
		if err != nil {
			continue
		}
		got := urlSpaces(strings.Fields(string(urls)), space)
		if sameSpaces(got, want) {
			not = append(not, "--remotes="+name)
		}
	}
	return not, nil
}

// sameSpaces reports whether two space lists hold the same spaces.
func sameSpaces(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slicesContains(b, x) {
			return false
		}
	}
	return true
}

// pending reads what an earlier command of the same call may stage or
// commit before this one runs (git add && git commit && git push): the
// staged and work tree changes, with filters off, and every untracked
// file that is not ignored.
func (c OutboundChecker) pending(ctx context.Context, dir string, diff []string) ([]string, error) {
	args, err := c.noFilters(ctx, dir)
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, d := range [][]string{{"diff", "--cached"}, {"diff"}} {
		out, err := c.git(ctx, dir, append(append(append([]string{}, args...), d...), diff...)...)
		texts = append(texts, string(out))
		if err != nil {
			return texts, err
		}
		bin, err := c.blobTexts(ctx, dir, len(d) == 2, append(append(append([]string{}, args...), d...), "--numstat", "--no-renames")...)
		texts = append(texts, bin...)
		if err != nil {
			return texts, err
		}
	}
	out, err := c.git(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return texts, err
	}
	total := 0
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		b, err := readSmall(filepath.Join(dir, p), maxOutboundText+1)
		if err != nil {
			continue
		}
		if total += len(b); total > maxOutboundText {
			return texts, errOutboundTooLarge
		}
		texts = append(texts, string(b))
	}
	return texts, nil
}

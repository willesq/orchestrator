package spaces

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The live GitHub guard (spaces plan SP-043): gh and git commands reach
// GitHub repos and orgs directly, bypassing memory, so Targets reports the
// orgs a call reaches without naming one repo (gh api orgs/<o>, gh repo
// list <o>, gh search --owner <o>, github.com/<o>), and the remotes git
// fetch, pull and push use when the command names a remote rather than a
// URL, read from the checkout's git config.

// githubLogin is GitHub's grammar for a login: letters, digits and
// single hyphens, not at either end.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

// loginPart is githubLogin inside a larger pattern.
const loginPart = `[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?`

// addOrg records a GitHub account (org or user) the call names in a gh
// argument; one that is not a login makes the call opaque.
func (e *extractor) addOrg(org string) {
	if !githubLogin.MatchString(org) {
		e.opaque("gh owner could not be read")
		return
	}
	if e.count >= maxTargets {
		e.opaque("too many targets")
		return
	}
	e.count++
	e.out.GHOrgs = append(e.out.GHOrgs, strings.ToLower(org))
}

// ghOrgEndpoint reads the account of an API endpoint about one org or
// user: orgs/<o>[/...] and users/<o>[/...].
func ghOrgEndpoint(ep string) (string, bool) {
	for _, p := range []string{"orgs/", "users/"} {
		if rest, ok := strings.CutPrefix(ep, p); ok {
			org, _, _ := strings.Cut(rest, "/")
			return org, org != ""
		}
	}
	return "", false
}

// ghSearchQualifier matches the qualifiers that pick an account or repo in
// a gh search query: owner:, org:, user: and repo:.
var ghSearchQualifier = regexp.MustCompile(`(?i)(?:^|[\s"'])(owner|org|user|repo):("?)([A-Za-z0-9_.\-/]+)`)

// ghOrgArgs reads the accounts a gh command names outside a repo flag: the
// owner of gh repo list, gh search's --owner and query qualifiers.
func (e *extractor) ghOrgArgs(subs []string, args []word) {
	if len(subs) == 0 {
		return
	}
	switch {
	case len(subs) >= 2 && subs[0] == "repo" && subs[1] == "list":
		// gh repo list [<owner>]: the owner is the first operand after list.
		seen := false
		for i := 0; i < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a.val, "-") {
				if ghRepoListValued[a.val] {
					i++
				}
				continue
			}
			if !seen {
				seen = a.val == "list"
				continue
			}
			e.ghOrgWord(a)
			return
		}
	case subs[0] == "search":
		for i := 0; i < len(args); i++ {
			v := args[i].val
			switch {
			case v == "--owner" && i+1 < len(args):
				i++
				e.ghOrgWord(args[i])
			case strings.HasPrefix(v, "--owner="):
				e.ghOrgWord(word{val: strings.TrimPrefix(v, "--owner="), partial: args[i].partial})
			case v == "--repo" || v == "-R":
				// Read by gh itself as a repo flag.
			default:
				e.ghQualifiers(v)
			}
		}
	}
}

// ghRepoListValued are gh repo list flags that take a separate value.
var ghRepoListValued = flagSet("-L", "--limit", "-l", "--language", "--topic", "--visibility", "--json",
	"-q", "--jq", "-t", "--template")

// ghOrgWord records one account named by a word.
func (e *extractor) ghOrgWord(w word) {
	if !w.exact() {
		e.opaque("gh owner is not static")
		return
	}
	for _, org := range strings.Split(w.val, ",") {
		if org = strings.TrimSpace(org); org != "" {
			e.addOrg(org)
		}
	}
}

// ghQualifiers records the accounts and repos a search query names.
func (e *extractor) ghQualifiers(s string) {
	for _, m := range ghSearchQualifier.FindAllStringSubmatch(s, -1) {
		if strings.EqualFold(m[1], "repo") {
			if repo, ok := parseRepo(m[3]); ok {
				e.addRepo(repo)
			}
			continue
		}
		org := strings.TrimRight(m[3], ".")
		if githubLogin.MatchString(org) {
			e.addOrg(org)
		}
	}
}

// githubAccountURL matches a URL about one GitHub account, whatever
// follows: the API's and the site's orgs/<o> and users/<o>.
var githubAccountURL = regexp.MustCompile(`(?i)github\.com/(?:orgs|users)/(` + loginPart + `)`)

// githubProfileURL matches a bare github.com/<o>, an account's page (also
// on www. and gist.), ended by anything that cannot continue a path.
var githubProfileURL = regexp.MustCompile(`(?i)(?:^|[^\w.-]|(?:www|gist)\.)github\.com/(` + loginPart + `)/?(?:$|[^A-Za-z0-9_./-]|\.(?:$|\s))`)

// githubOrgURLs records the account of every GitHub URL in s that names an
// account rather than a repo.
func (e *extractor) githubOrgURLs(s string) {
	if !strings.Contains(strings.ToLower(s), "github.com") {
		return
	}
	for _, m := range githubAccountURL.FindAllStringSubmatch(s, -1) {
		e.addOrg(m[1])
	}
	for _, m := range githubProfileURL.FindAllStringSubmatch(s, -1) {
		if !githubOwnerless[strings.ToLower(m[1])] {
			e.addOrg(m[1])
		}
	}
}

// gitRemoteSubs are the git subcommands that talk to a remote.
var gitRemoteSubs = flagSet("fetch", "pull", "push", "ls-remote", "remote")

// gitValued are git fetch, pull, push and ls-remote flags that take a
// separate value, so it is not read as the remote.
var gitValued = flagSet("--depth", "--deepen", "--shallow-since", "--shallow-exclude", "-j", "--jobs",
	"--negotiation-tip", "--server-option", "-o", "--push-option", "--refmap", "--filter",
	"--submodule-prefix", "--recurse-submodules-default", "-s", "--strategy", "-X", "--strategy-option",
	"--sort", "--upload-pack", "--receive-pack", "--exec", "--repo", "--rebase", "--log", "-q", "--quiet")

// gitValuedOnly are the gitValued flags that always take a value; the rest
// (--rebase, --log, -q) are switches whose value only follows "=".
var gitValuedOnly = flagSet("--depth", "--deepen", "--shallow-since", "--shallow-exclude", "-j", "--jobs",
	"--negotiation-tip", "--server-option", "-o", "--push-option", "--refmap", "--filter",
	"--submodule-prefix", "--recurse-submodules-default", "-s", "--strategy", "-X", "--strategy-option",
	"--sort", "--upload-pack", "--receive-pack", "--exec", "--repo")

// maxGitConfig bounds what is read of a checkout's git config.
const maxGitConfig = 256 << 10

// maxGitSmall bounds the small files a checkout's git directory leads
// through: a .git file, commondir and HEAD.
const maxGitSmall = 4 << 10

// gitNamedRemotes resolves the remotes git sub uses when the command names
// a remote rather than a URL, from the checkout's config: the named ones,
// a remote group's members, or, with none named, the remote git would use
// for the current branch (every remote when that cannot be told). gitDir
// is the --git-dir the command gave, "" for none.
func (e *extractor) gitNamedRemotes(sub string, rest []word, at where, gitDir string) {
	if !gitRemoteSubs[sub] {
		return
	}
	names, multi, ok := gitRemoteOperands(sub, rest)
	if !ok {
		e.opaque("git remote is not static")
		return
	}
	if sub == "remote" && names == nil && !multi {
		return
	}
	dirs := at.dirs
	if gitDir != "" {
		dirs = []string{gitDir}
	}
	for _, dir := range dirs {
		cfg, gd, err := readCheckoutConfig(dir, gitDir != "")
		switch {
		case err != nil:
			e.opaque("git config could not be read")
			continue
		case cfg == nil:
			continue
		}
		if cfg.rewrites {
			e.opaque("git config rewrites remote URLs")
		}
		var want []string
		switch {
		case len(names) > 0:
			for _, n := range names {
				if _, ok := cfg.remotes[n]; ok {
					want = append(want, n)
				} else if members, ok := cfg.groups[n]; ok {
					want = append(want, members...)
				} else if !strings.ContainsAny(n, "/:") {
					e.opaque("git remote is not in the checkout's config")
				}
			}
		case multi:
			want = cfg.names()
		default:
			want = cfg.defaultRemotes(sub, gd)
		}
		top := filepath.Dir(gd)
		for _, n := range want {
			for _, u := range cfg.remotes[n] {
				e.configRemote(u, top)
			}
		}
	}
}

// gitRemoteOperands reads the remotes a remote-talking git command names:
// names, or multi when it means every remote (fetch --all, remote update
// with no group). ok is false when a name is not static.
func gitRemoteOperands(sub string, rest []word) (names []string, multi bool, ok bool) {
	var operands []word
	all, multiple := false, false
	for i := 0; i < len(rest); i++ {
		v := rest[i].val
		flag, val, hasVal := strings.Cut(v, "=")
		switch {
		case v == "--":
			operands = append(operands, rest[i+1:]...)
			i = len(rest)
		case v == "--all":
			all = true
		case v == "--multiple":
			multiple = true
		case flag == "--repo":
			if hasVal {
				operands = append([]word{{val: val, partial: rest[i].partial, glob: rest[i].glob}}, operands...)
			} else if i+1 < len(rest) {
				i++
				operands = append([]word{rest[i]}, operands...)
			}
		case strings.HasPrefix(v, "-"):
			if gitValuedOnly[v] && !hasVal && i+1 < len(rest) {
				i++
			}
		default:
			operands = append(operands, rest[i])
		}
	}
	if sub == "remote" {
		// Only "git remote update [<group>|<remote>...]" talks to remotes.
		if len(operands) == 0 || operands[0].val != "update" {
			return nil, false, true
		}
		operands = operands[1:]
		multi = len(operands) == 0
		multiple = true
	}
	if all {
		return nil, true, true
	}
	if !multiple && len(operands) > 1 {
		operands = operands[:1]
	}
	for _, o := range operands {
		if !o.exact() {
			return nil, false, false
		}
		names = append(names, o.val)
	}
	return names, multi, true
}

// configRemote reads one remote URL from a checkout's config, whose top
// level is top. Unlike a command-line word it fails closed: a form it
// cannot place makes the call opaque, and a local path is a git target.
func (e *extractor) configRemote(u, top string) {
	switch {
	case strings.HasPrefix(u, "file://"):
		e.add(kindGit, e.abs(top, strings.TrimPrefix(u, "file://")))
	case filepath.IsAbs(u) || strings.HasPrefix(u, ".") || strings.HasPrefix(u, "~"):
		for _, p := range e.absAll(where{dirs: []string{top}}, u) {
			e.add(kindGit, p)
		}
	default:
		if m := configURLRemote.FindStringSubmatch(u); m != nil {
			e.githubRemote(m[1], m[2], m[3])
			return
		}
		if m := configSCPRemote.FindStringSubmatch(u); m != nil {
			e.githubRemote(m[1], m[2], m[3])
			return
		}
		e.opaque("git remote in the checkout's config could not be read")
	}
}

// githubRemote records owner/repo on host when host is GitHub, and makes
// the call opaque otherwise: an ssh alias such as "work" may stand for
// github.com, and another host's space is unknown.
func (e *extractor) githubRemote(host, owner, repo string) {
	if strings.Contains(strings.ToLower(host), "github") {
		e.addRepo(owner + "/" + strings.TrimSuffix(repo, ".git"))
		return
	}
	e.opaque("git remote on a host other than GitHub")
}

var (
	configURLRemote = regexp.MustCompile(`^(?i:(?:git\+ssh|ssh\+git|ssh|git|https?))://(?:[^@/]+@)?([^/:]+)(?::\d+)?/+([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)/?$`)
	configSCPRemote = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9._-]+):/*([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)/?$`)
)

// gitConfig is what Targets reads of a checkout's git config.
type gitConfig struct {
	// remotes are each remote's url and pushurl values, by name.
	remotes map[string][]string
	// groups are remote groups (remotes.<group>), by name.
	groups map[string][]string
	// branchRemote and pushRemote are branch.<b>.remote and pushRemote;
	// pushDefault is remote.pushDefault.
	branchRemote, pushRemote map[string]string
	pushDefault              string
	// rewrites is set when the config can rewrite or add to remotes:
	// url.<base>.insteadOf, or an include.
	rewrites bool
}

func (c *gitConfig) names() []string {
	out := make([]string, 0, len(c.remotes))
	for n := range c.remotes {
		out = append(out, n)
	}
	return out
}

// defaultRemotes are the remotes sub uses with none named: the current
// branch's push remote, remote.pushDefault or the branch's remote, else
// origin; every remote when the branch cannot be told.
func (c *gitConfig) defaultRemotes(sub, gitDir string) []string {
	branch, ok := headBranch(gitDir)
	pick := func(names ...string) []string {
		for _, n := range names {
			if _, ok := c.remotes[n]; ok && n != "" {
				return []string{n}
			}
		}
		return nil
	}
	if !ok {
		return c.names()
	}
	var r []string
	if sub == "push" {
		r = pick(c.pushRemote[branch], c.pushDefault, c.branchRemote[branch], "origin")
	} else {
		r = pick(c.branchRemote[branch], "origin")
	}
	if r == nil {
		return c.names()
	}
	return r
}

// headBranch reads the branch HEAD names in gitDir.
func headBranch(gitDir string) (string, bool) {
	b, err := readBounded(filepath.Join(gitDir, "HEAD"), maxGitSmall)
	if err != nil {
		return "", false
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	return ref, ok && ref != ""
}

// readCheckoutConfig reads the config of the checkout holding dir (or, when
// isGitDir, of the git directory dir), and returns that git directory. A
// nil config means dir is in no checkout.
func readCheckoutConfig(dir string, isGitDir bool) (*gitConfig, string, error) {
	gd, head := dir, dir
	if !isGitDir {
		var ok bool
		var err error
		gd, head, ok, err = findGitDir(dir)
		if err != nil || !ok {
			return nil, "", err
		}
	}
	b, err := readBounded(filepath.Join(gd, "config"), maxGitConfig)
	if err != nil {
		return nil, "", err
	}
	cfg, err := parseGitConfig(b)
	if err != nil {
		return nil, "", err
	}
	return cfg, head, nil
}

// gitConfigRemotes reads the remote URLs of the checkout holding dir, by
// name; ok is false when dir is in no checkout or its config cannot be
// read.
func gitConfigRemotes(dir string) (map[string][]string, bool) {
	cfg, _, err := readCheckoutConfig(dir, false)
	if err != nil || cfg == nil {
		return nil, false
	}
	return cfg.remotes, true
}

// readBounded reads a regular file of at most limit bytes; a larger one
// is an error, never a cut-short result.
func readBounded(path string, limit int64) ([]byte, error) {
	b, err := readSmall(path, limit+1)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is over %d bytes", filepath.Base(path), limit)
	}
	return b, nil
}

// findGitDir finds the git directory holding dir's config: the .git
// directory above dir (followed through a symlink, as git does), or for a
// linked worktree the common directory its .git file leads to. head is
// where that checkout's HEAD lives: the worktree's own git directory.
func findGitDir(dir string) (gitDir, head string, ok bool, err error) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		p := filepath.Join(d, ".git")
		if st, serr := os.Stat(p); serr == nil {
			if st.IsDir() {
				return p, p, true, nil
			}
			gd, common, lerr := linkedGitDir(p)
			if lerr != nil {
				return "", "", false, lerr
			}
			return common, gd, true, nil
		}
		if parent := filepath.Dir(d); parent == d {
			return "", "", false, nil
		}
	}
}

// linkedGitDir follows a worktree's .git file to its git directory and the
// common directory it shares with the main checkout.
func linkedGitDir(dotGit string) (gitDir, common string, err error) {
	b, err := readBounded(dotGit, maxGitSmall)
	if err != nil {
		return "", "", err
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return "", "", fmt.Errorf("%s names no git directory", dotGit)
	}
	gd = strings.TrimSpace(gd)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(filepath.Dir(dotGit), gd)
	}
	gd = filepath.Clean(gd)
	c, err := readBounded(filepath.Join(gd, "commondir"), maxGitSmall)
	if err != nil {
		if os.IsNotExist(err) {
			return gd, gd, nil
		}
		return "", "", err
	}
	common = strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gd, common)
	}
	return gd, filepath.Clean(common), nil
}

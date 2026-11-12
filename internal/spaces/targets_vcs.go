package spaces

import (
	"regexp"
	"strings"
)

// safeGitConfig are git -c keys that cannot run code, change where git
// reads or writes, or redirect a remote.
var safeGitConfig = []string{"user.", "color.", "advice.", "init.defaultbranch", "commit.gpgsign",
	"tag.gpgsign", "pull.rebase", "pull.ff", "push.default", "push.autosetupremote", "log.", "column.",
	"merge.conflictstyle", "rebase.autosquash", "rebase.autostash", "fetch.prune", "safe.directory",
	"protocol.version", "http.lowspeed", "gc.auto", "maintenance.auto", "feature.", "status."}

func safeGitKey(kv string) bool {
	k, _, _ := strings.Cut(strings.ToLower(kv), "=")
	for _, p := range safeGitConfig {
		if strings.HasPrefix(k, p) || k == strings.TrimSuffix(p, ".") {
			return true
		}
	}
	return false
}

// gitRuns are git subcommands that run commands given as arguments or
// configured outside the call.
var gitRuns = flagSet("filter-branch", "difftool", "mergetool", "submodule", "send-email", "instaweb")

// gitExecFlags are subcommand flags that take a command to run.
var gitExecFlags = flagSet("--upload-pack", "--receive-pack", "-u", "--exec", "-x", "--extcmd", "--tool", "-t",
	"--gpg-sign-program", "--signoff-command")

// gitCreates are git subcommands that write files from repo content,
// which may include symlinks.
var gitCreates = flagSet("clone", "checkout", "switch", "worktree", "apply", "am", "pull", "merge",
	"rebase", "reset", "restore", "stash", "cherry-pick", "revert", "submodule")

// git records -C, --git-dir and --work-tree. -C options are cumulative,
// and the subcommand's paths are relative to the last one. -c with a key
// that can run code or redirect git is opaque.
func (e *extractor) git(args []word, at where, st *shellState, depth int) {
	i, gitDir := 0, ""
	for ; i < len(args); i++ {
		a := args[i]
		flag, val, hasVal := strings.Cut(a.val, "=")
		switch flag {
		case "-C", "--git-dir", "--work-tree":
			arg := word{val: val, partial: a.partial, glob: a.glob, lit: a.lit}
			if !hasVal {
				if i+1 >= len(args) {
					return
				}
				i++
				arg = args[i]
			}
			if !arg.exact() {
				e.opaque("git directory is not static")
				e.pathArg(arg, at)
				continue
			}
			next := e.moveAll(at, arg.val)
			if flag == "-C" {
				at = next
			}
			if flag == "--git-dir" && len(next.dirs) == 1 {
				gitDir = next.dirs[0]
			}
			for _, d := range next.dirs {
				e.add(kindGit, d)
			}
			continue
		case "-c", "--config-env":
			kv := val
			if !hasVal && i+1 < len(args) {
				i++
				kv = args[i].val
			}
			if flag == "--config-env" || !safeGitKey(kv) {
				e.opaque("git config that can run code or redirect git")
			}
			continue
		case "--exec-path":
			if hasVal {
				e.opaque("git config that can run code or redirect git")
			}
			continue
		case "--namespace", "--super-prefix", "--list-cmds":
			if !hasVal {
				i++
			}
			continue
		}
		if strings.HasPrefix(a.val, "-") {
			continue
		}
		break
	}
	if i >= len(args) {
		return
	}
	sub := args[i].val
	rest := args[i+1:]
	if gitRuns[sub] || sub == "bisect" && len(rest) > 0 && rest[0].val == "run" {
		e.opaque("git runs a command")
	}
	for i, a := range rest {
		flag, val, hasVal := strings.Cut(a.val, "=")
		switch {
		case gitExecFlags[flag] && (flag != "-x" || sub == "rebase" || sub == "difftool") &&
			(flag != "-u" || sub == "fetch" || sub == "clone" || sub == "pull" || sub == "ls-remote"):
			e.opaque("git runs a command")
		case flag == "-c" || flag == "--config":
			if !hasVal && i+1 < len(rest) {
				val = rest[i+1].val
			}
			if !safeGitKey(val) {
				e.opaque("git config that can run code or redirect git")
			}
		}
	}
	e.nested(rest, at, st, depth)
	if sub == "config" {
		for _, a := range rest {
			if !strings.HasPrefix(a.val, "-") && strings.Contains(a.val, ".") && !safeGitKey(a.val) {
				e.opaque("git config that can run code or redirect git")
				break
			}
		}
	}
	for _, a := range rest {
		if a.exact() {
			e.gitRemote(a.val)
		}
	}
	e.gitNamedRemotes(sub, rest, at, gitDir)
	e.genericArgs(rest, at)
	if gitCreates[sub] {
		e.creates()
	}
}

var (
	scpRemote = regexp.MustCompile(`^(?:([A-Za-z0-9._-]+)@)?([A-Za-z0-9._-]+):/?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)
	urlRemote = regexp.MustCompile(`^(?:ssh|git|https?)://(?:[^@/]+@)?([^/:]+)(?::\d+)?/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)
)

// gitRemote reads a git remote written as user@host:owner/repo or as a
// URL. A host that names github, such as an ssh alias "github-acme", is
// GitHub; a remote on another host is opaque, since its space is unknown.
func (e *extractor) gitRemote(s string) {
	var host, owner, repo string
	if m := urlRemote.FindStringSubmatch(s); m != nil {
		host, owner, repo = m[1], m[2], m[3]
	} else if m := scpRemote.FindStringSubmatch(s); m != nil && (m[1] != "" || strings.Contains(strings.ToLower(m[2]), "github")) {
		host, owner, repo = m[2], m[3], m[4]
	} else {
		return
	}
	if strings.Contains(strings.ToLower(host), "github") {
		e.addRepo(owner + "/" + repo)
		return
	}
	e.opaque("git remote on a host other than GitHub")
}

func (e *extractor) make(args []word, at where) {
	var rest, files []word
	for i := 0; i < len(args); i++ {
		v := args[i].val
		var dir string
		switch {
		case v == "-C" || v == "--directory":
			if i+1 < len(args) {
				i++
				dir = args[i].val
				if !args[i].exact() {
					e.opaque("make directory is not static")
					e.pathArg(args[i], at)
					continue
				}
			}
		case strings.HasPrefix(v, "--directory="):
			dir = strings.TrimPrefix(v, "--directory=")
		case strings.HasPrefix(v, "-C") && len(v) > 2:
			dir = v[2:]
		case v == "-E" || v == "--eval" || strings.HasPrefix(v, "--eval="):
			e.opaque("make --eval")
			rest = append(rest, args[i])
			continue
		case (v == "-f" || v == "--file" || v == "--makefile") && i+1 < len(args):
			i++
			if args[i].val == "-" {
				e.opaque("make reads its makefile from stdin")
			} else {
				files = append(files, args[i])
			}
			continue
		case !strings.HasPrefix(v, "-") && strings.Contains(v, "="):
			// A variable set on the command line can change SHELL or a
			// recipe.
			e.opaque("make variable set on the command line")
			rest = append(rest, args[i])
			continue
		default:
			rest = append(rest, args[i])
			continue
		}
		if dir != "" {
			at = e.moveAll(at, dir)
			for _, d := range at.dirs {
				e.add(kindCd, d)
			}
		}
	}
	// The makefile's recipes run too; keep the paths written in it.
	for _, d := range at.dirs {
		names := []string{"GNUmakefile", "makefile", "Makefile"}
		if len(files) > 0 {
			names = nil
			for _, f := range files {
				e.pathArg(f, at)
				if f.exact() {
					names = append(names, f.val)
				}
			}
		}
		for _, f := range names {
			if data, err := readSmall(e.res.resolve(e.abs(d, f)).path, maxScript); err == nil {
				e.embedded(string(data), where{dirs: []string{d}}, true, true)
				if len(files) == 0 {
					break
				}
			}
		}
	}
	e.genericArgs(rest, at)
}

// gh records the repos a gh command targets. Without -R it targets the
// cwd's repo, which the guard already knows.
func (e *extractor) gh(args []word, at where) {
	var subs []string
	for i := 0; i < len(args); i++ {
		v := args[i].val
		switch {
		case v == "-R" || v == "--repo":
			if i+1 < len(args) {
				i++
				e.ghRepoArg(args[i])
			}
			continue
		case strings.HasPrefix(v, "--repo="):
			e.ghRepoArg(word{val: strings.TrimPrefix(v, "--repo="), partial: args[i].partial})
			continue
		case strings.HasPrefix(v, "-R") && len(v) > 2:
			e.ghRepoArg(word{val: v[2:], partial: args[i].partial})
			continue
		}
		if strings.HasPrefix(v, "-") {
			if len(subs) > 0 && subs[0] == "api" && ghAPIValued[v] {
				i++
			}
			continue
		}
		if len(subs) < 2 {
			subs = append(subs, v)
			if len(subs) == 1 && ghCrossRepo[v] {
				e.opaque("gh command outside one repo")
			}
			if len(subs) == 2 && subs[0] == "repo" && subs[1] == "list" {
				e.opaque("gh command outside one repo")
			}
			if len(subs) < 2 || subs[0] != "api" {
				continue
			}
		}
		switch subs[0] {
		case "api":
			if len(subs) > 2 {
				break
			}
			ep, _, _ := strings.Cut(strings.TrimPrefix(v, "/"), "?")
			switch {
			case strings.HasPrefix(ep, "repos/"):
				if args[i].partial {
					e.opaque("gh api endpoint is not static")
				}
				e.ghRepoArg(word{val: strings.TrimPrefix(ep, "repos/")})
			case strings.HasPrefix(ep, "orgs/") || strings.HasPrefix(ep, "users/"):
				if org, ok := ghOrgEndpoint(ep); ok && !args[i].partial {
					e.addOrg(org)
				} else {
					e.opaque("gh api endpoint is not static")
				}
			case !ghSafeEndpoints[ep]:
				e.opaque("gh api endpoint outside one repo")
			}
			subs = append(subs, v)
		case "repo":
			if repo, ok := parseRepo(v); ok {
				e.addRepo(repo)
			}
		}
	}
	e.ghOrgArgs(subs, args)
	e.genericArgs(args, at)
	if len(subs) >= 2 && subs[0] == "repo" && subs[1] == "clone" {
		e.creates()
	}
}

// ghCrossRepo are gh commands that reach beyond one repo or run code:
// search, aliases, extensions and org-wide listings.
var ghCrossRepo = flagSet("search", "alias", "extension", "org", "gist", "project", "codespace", "status", "browse")

// ghAPIValued are gh api flags that take a separate value.
var ghAPIValued = flagSet("-X", "--method", "-H", "--header", "-f", "--raw-field", "-F", "--field", "--input",
	"-q", "--jq", "-t", "--template", "--hostname", "-p", "--preview", "--cache")

// ghSafeEndpoints are gh api endpoints about the signed-in account itself.
var ghSafeEndpoints = flagSet("user", "rate_limit", "meta", "octocat", "zen", "emojis", "versions")

func (e *extractor) ghRepoArg(w word) {
	if !w.exact() {
		e.opaque("gh repo is not static")
		return
	}
	repo, ok := parseRepo(w.val)
	if !ok {
		e.opaque("gh repo could not be read")
		return
	}
	e.addRepo(repo)
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseRepo reads OWNER/REPO, HOST/OWNER/REPO or a github.com URL. Extra
// path segments after the repo, as in an API endpoint, are ignored.
func parseRepo(s string) (string, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "?")
	parts := strings.Split(s, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) < 2 || !repoPart.MatchString(parts[0]) || !repoPart.MatchString(parts[1]) {
		return "", false
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), true
}

var githubURL = regexp.MustCompile(`(?i)(?:api\.github\.com/repos/|github\.com[/:])([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)`)

// githubOwnerless are github.com paths whose first segment is not an owner.
var githubOwnerless = flagSet("orgs", "settings", "login", "features", "marketplace", "sponsors",
	"apps", "topics", "collections", "notifications", "search", "explore", "users", "enterprises")

// githubURLs records the repo of every github.com URL in s.
func (e *extractor) githubURLs(s string) {
	if !strings.Contains(strings.ToLower(s), "github.com") {
		return
	}
	for _, m := range githubURL.FindAllStringSubmatch(s, -1) {
		if githubOwnerless[strings.ToLower(m[1])] {
			continue
		}
		e.addRepo(m[1] + "/" + strings.TrimSuffix(m[2], ".git"))
	}
}

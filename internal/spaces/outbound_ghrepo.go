package spaces

import (
	"context"
	"errors"
	osexec "os/exec"
	"sort"
	"strings"
)

// Which repo a gh command that names none writes to (baseline rule
// gh_default_repo, D-60). gh takes GH_REPO; else, among the checkout's
// remotes in its own order (upstream, github, origin, then the rest), the
// first one 'gh repo set-default' marked (remote.<name>.gh-resolved:
// "base" for the remote itself, or the owner/repo it names); else, since
// a tool call has no terminal to ask on, the first GitHub remote.
//
// gh only counts remotes on hosts it knows, which this check cannot see
// (ssh aliases, GitHub Enterprise logins). So without a mark, every remote
// before the first github.com one counts too, and with GH_HOST naming
// another host (the command's own before the hook's), or GH_CONFIG_DIR set
// in the command, every remote does: the destinations may be more than
// gh's one, never fewer.

// ghSetsDefault reports whether gh args (after "gh") change the default
// repo: gh repo set-default with a repo or --unset.
func ghSetsDefault(args []string) bool {
	var subs []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			subs = append(subs, a)
		}
	}
	return len(subs) >= 2 && subs[0] == "repo" && subs[1] == "set-default" &&
		(len(subs) > 2 || slicesContains(args, "--unset") || slicesContains(args, "-u"))
}

// ghPickEnv are the variables that change which repo gh picks.
var ghPickEnv = []string{"GH_HOST", "GH_CONFIG_DIR"}

// ghEnvOf keeps the ghPickEnv variables env sets.
func ghEnvOf(env map[string]string) map[string]string {
	var out map[string]string
	for _, k := range ghPickEnv {
		if v, ok := env[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// ghRemoteScore is gh's order of remotes by name.
func ghRemoteScore(name string) int {
	switch strings.ToLower(name) {
	case "upstream":
		return 3
	case "github":
		return 2
	case "origin":
		return 1
	}
	return 0
}

type ghRemote struct{ name, url string }

// ghDefaultURLs are the URLs of the repos a gh command run in dir without
// a repo may write to; ok is false when they cannot be told.
//
// unmarked is set for gh api's {owner}/{repo} placeholders, which gh may
// fill from its first remote whatever the mark says: both then count.
func (c OutboundChecker) ghDefaultURLs(ctx context.Context, dir string, cmdEnv map[string]string, unmarked bool) (urls []string, ok bool) {
	if r := c.getenv("GH_REPO"); r != "" {
		return []string{"https://github.com/" + r}, true
	}
	host, hostSet := cmdEnv["GH_HOST"]
	if !hostSet {
		host = c.getenv("GH_HOST")
	}
	_, configMoved := cmdEnv["GH_CONFIG_DIR"]
	if host == unknownValue {
		return nil, false
	}
	out, err := c.git(ctx, dir, "remote", "-v")
	if err != nil {
		return nil, false
	}
	var remotes []ghRemote
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || seen[f[0]] {
			continue
		}
		// Reason: git lists a remote's fetch URL first, which gh reads
		// the repo from.
		seen[f[0]] = true
		remotes = append(remotes, ghRemote{f[0], f[1]})
	}
	// Reason: another host, or another gh config whose logins the check
	// cannot read, may make gh skip github.com remotes. With baseline
	// rule gh_default_repo switched off, every remote counts, as before
	// D-60.
	if host != "" && !strings.EqualFold(host, "github.com") || configMoved || len(remotes) == 0 ||
		!c.rules.on(OutboundGHDefaultRepo) {
		var all []string
		for _, r := range remotes {
			all = append(all, r.url)
		}
		return all, true
	}
	sort.SliceStable(remotes, func(i, j int) bool { return ghRemoteScore(remotes[i].name) > ghRemoteScore(remotes[j].name) })
	resolved, err := c.ghResolved(ctx, dir)
	if err != nil {
		return nil, false
	}
	for _, r := range remotes {
		mark := ""
		switch v := resolved[r.name]; {
		case v == "base":
			mark = r.url
		case v != "":
			repo, ok := parseRepo(v)
			if !ok {
				return nil, false
			}
			mark = "https://github.com/" + repo
		}
		if mark != "" && !unmarked {
			return []string{mark}, true
		}
		if mark != "" {
			urls = append(urls, mark)
			break
		}
	}
	for _, r := range remotes {
		urls = append(urls, r.url)
		if isGitHubDotCom(r.url) {
			break
		}
	}
	return urls, true
}

// ghResolved reads the remotes 'gh repo set-default' marked, by name.
func (c OutboundChecker) ghResolved(ctx context.Context, dir string) (map[string]string, error) {
	out, err := c.git(ctx, dir, "config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	var ee *osexec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		// git config exits 1 when no key matches.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	marks := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, _ := strings.Cut(strings.TrimSpace(line), " ")
		name, ok := strings.CutPrefix(key, "remote.")
		if !ok {
			continue
		}
		if name, ok = strings.CutSuffix(name, ".gh-resolved"); ok {
			marks[name] = strings.TrimSpace(val)
		}
	}
	return marks, nil
}

// isGitHubDotCom reports whether a remote URL is on github.com itself (or
// ssh.github.com), a host gh always knows.
func isGitHubDotCom(url string) bool {
	host := ""
	if m := urlRemote.FindStringSubmatch(url); m != nil {
		host = m[1]
	} else if m := scpRemote.FindStringSubmatch(url); m != nil {
		host = m[2]
	}
	h := strings.ToLower(host)
	return h == "github.com" || h == "ssh.github.com" || h == "www.github.com"
}

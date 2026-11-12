package spaces

import (
	"path/filepath"
	"strings"
)

// flagValue reads the value of a flag at args[i]: "-F v", "-Fv",
// "--file v" or "--file=v", for the short and long names given. next is the
// index of the last word used.
func flagValue(args []string, i int, short, long string) (val string, next int, ok bool) {
	a := args[i]
	switch {
	case long != "" && a == long, short != "" && a == short:
		if i+1 < len(args) {
			return args[i+1], i + 1, true
		}
		return "", i, false
	case long != "" && strings.HasPrefix(a, long+"="):
		return strings.TrimPrefix(a, long+"="), i, true
	case short != "" && len(short) == 2 && strings.HasPrefix(a, short) && len(a) > 2 && !strings.HasPrefix(a, "--"):
		return a[2:], i, true
	}
	return "", i, false
}

// inDir resolves a file a command names against the directory it runs in.
func inDir(dir, p string) string {
	if p == "-" || filepath.IsAbs(p) || dir == "" {
		return p
	}
	return filepath.Join(dir, p)
}

// gitEnvRedirects are environment variables that point git at another
// repository, index or configuration than the check would read.
var gitEnvRedirects = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_NAMESPACE",
	"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR"}

// redirectsGit reports whether env sets a variable that redirects git.
func redirectsGit(env map[string]string) bool {
	for _, k := range gitEnvRedirects {
		if _, ok := env[k]; ok {
			return true
		}
	}
	return false
}

// gitSafeGlobal are git's global options that leave where git sends and
// what it runs alone.
var gitSafeGlobal = flagSet("--no-pager", "-P", "--paginate", "-p", "--no-optional-locks", "--literal-pathspecs",
	"--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--no-replace-objects", "--no-lazy-fetch",
	"--no-advice", "--bare")

// gitSafeConfig are -c keys that do not change where git sends or what it
// runs instead of the command named.
var gitSafeConfig = []string{"user.", "commit.gpgsign", "commit.verbose", "color.", "i18n.", "init.",
	"core.quotepath", "core.editor", "advice.", "log.", "gpg.format", "tag.gpgsign"}

func safeConfig(kv string) bool {
	k, _, _ := strings.Cut(strings.ToLower(kv), "=")
	for _, p := range gitSafeConfig {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// gitKnown are git subcommands that send nothing out, or whose outbound
// part is read below; any other may be an alias.
var gitKnown = flagSet("add", "am", "apply", "archive", "bisect", "blame", "branch", "bundle", "cat-file",
	"check-ignore", "checkout", "cherry", "cherry-pick", "clean", "clone", "commit", "config", "count-objects",
	"describe", "diff", "difftool", "fetch", "for-each-ref", "format-patch", "fsck", "gc", "grep", "help",
	"init", "log", "ls-files", "ls-remote", "ls-tree", "merge", "merge-base", "mergetool", "mv", "notes",
	"pull", "push", "range-diff", "rebase", "reflog", "remote", "reset", "restore", "rev-list", "rev-parse",
	"revert", "rm", "shortlog", "show", "show-ref", "sparse-checkout", "stash", "status", "submodule",
	"switch", "symbolic-ref", "tag", "update-index", "update-ref", "var", "version", "whatchanged",
	"worktree", "write-tree", "maintenance", "lfs")

// gitSends are git subcommands that send text out in ways the check does
// not read.
var gitSends = flagSet("send-email", "subtree", "request-pull", "imap-send", "p4", "svn", "cvsexportcommit")

// gitOutbound reads a git command (its arguments after "git") run in dir
// with env; nil when it sends nothing out. unknown is set when it may send
// text the check cannot read.
func gitOutbound(args []string, dir string, env map[string]string) (*Outbound, bool) {
	unknown := redirectsGit(env)
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			i++
			dir = inDir(dir, args[i])
		case a == "-c" && i+1 < len(args):
			i++
			if !safeConfig(args[i]) {
				unknown = true
			}
		case strings.HasPrefix(a, "-"):
			// Reason: any other global option (--git-dir, --work-tree,
			// --config-env, --exec-path, …) may point git elsewhere.
			if !gitSafeGlobal[a] {
				unknown = true
				if (a == "--git-dir" || a == "--work-tree" || a == "--namespace" || a == "--exec-path" ||
					a == "--config-env" || a == "--super-prefix") && i+1 < len(args) {
					i++
				}
			}
		default:
			// Reason: rebase -x, submodule foreach, bisect run and the like
			// run a command; one that runs git or gh is not followed.
			if mentionsGit(args[i+1:]) {
				unknown = true
			}
			act := gitSubOutbound(a, args[i+1:], dir)
			switch {
			case gitSends[a]:
				return &Outbound{Kind: "git " + a, Dir: dir}, true
			case act == nil && !gitKnown[a]:
				return &Outbound{Kind: "git " + a, Dir: dir, Alias: a}, unknown
			}
			if act != nil && act.unknownFlags {
				unknown = true
			}
			return act.public(), unknown
		}
	}
	return nil, unknown
}

// outboundRead is an action as read, with what made it unreadable.
type outboundRead struct {
	Outbound
	unknownFlags bool
}

func (r *outboundRead) public() *Outbound {
	if r == nil {
		return nil
	}
	o := r.Outbound
	return &o
}

func gitSubOutbound(sub string, rest []string, dir string) *outboundRead {
	switch sub {
	case "commit":
		act := &outboundRead{Outbound: Outbound{Kind: "git commit", Dir: dir, Staged: true}}
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if v, n, ok := flagValue(rest, i, "-F", "--file"); ok {
				act.Files, i = append(act.Files, inDir(dir, v)), n
				continue
			}
			if _, n, ok := flagValue(rest, i, "-m", "--message"); ok {
				i = n
				continue
			}
			if v, n, ok := flagValue(rest, i, "-t", "--template"); ok {
				act.Files, i = append(act.Files, inDir(dir, v)), n
				continue
			}
			if a == "-a" || a == "--all" || shortBundle(a, 'a') {
				act.All = true
			}
		}
		return act
	case "tag":
		var act *outboundRead
		for i := 0; i < len(rest); i++ {
			if v, n, ok := flagValue(rest, i, "-F", "--file"); ok {
				act = orNew(act, "git tag", dir)
				act.Files, i = append(act.Files, inDir(dir, v)), n
				continue
			}
			if _, n, ok := flagValue(rest, i, "-m", "--message"); ok {
				act, i = orNew(act, "git tag", dir), n
			}
		}
		return act
	case "notes":
		// git notes add|append -m/-F: notes are pushed like refs.
		act := &outboundRead{Outbound: Outbound{Kind: "git notes", Dir: dir}}
		for i := 0; i < len(rest); i++ {
			if v, n, ok := flagValue(rest, i, "-F", "--file"); ok {
				act.Files, i = append(act.Files, inDir(dir, v)), n
			}
		}
		return act
	case "push":
		return gitPush(rest, dir)
	}
	return nil
}

// shortBundle reports whether a is a short-flag bundle holding c before
// any flag that takes a value (m, F, c, C, t).
func shortBundle(a string, c byte) bool {
	if len(a) < 3 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for i := 1; i < len(a); i++ {
		switch a[i] {
		case c:
			return true
		case 'm', 'F', 'c', 'C', 't':
			return false
		}
	}
	return false
}

func gitPush(rest []string, dir string) *outboundRead {
	act := &outboundRead{Outbound: Outbound{Kind: "git push", Dir: dir, Push: true}}
	var operands []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if v, n, ok := flagValue(rest, i, "", "--repo"); ok {
			act.Remote, i = v, n
			continue
		}
		switch {
		case a == "-o" || a == "--push-option" || a == "--receive-pack" || a == "--exec":
			i++
		case a == "--all" || a == "--mirror" || a == "--branches":
			act.Refs = append(act.Refs, "--branches")
			if a == "--mirror" {
				act.Tags = append(act.Tags, "*")
			}
		case a == "--tags" || a == "--follow-tags":
			act.Tags = append(act.Tags, "*")
		case strings.HasPrefix(a, "--recurse-submodules"):
			// Submodules' commits leave too, from repositories not read.
			act.unknownFlags = true
		case a == "--":
			operands = append(operands, rest[i+1:]...)
			i = len(rest)
		case strings.HasPrefix(a, "-"):
		default:
			operands = append(operands, a)
		}
	}
	if len(operands) > 0 && act.Remote == "" {
		act.Remote, operands = operands[0], operands[1:]
	}
	for _, spec := range operands {
		src, _, _ := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if src == "" {
			continue
		}
		act.Refs = append(act.Refs, src)
		// A ref that names a tag sends its message too.
		act.Tags = append(act.Tags, strings.TrimPrefix(src, "refs/tags/"))
	}
	return act
}

func orNew(act *outboundRead, kind, dir string) *outboundRead {
	if act == nil {
		return &outboundRead{Outbound: Outbound{Kind: kind, Dir: dir}}
	}
	return act
}

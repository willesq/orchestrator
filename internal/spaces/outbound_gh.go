package spaces

import "strings"

// How the outbound terms check (SP-044) reads gh commands: every gh
// command but a known read-only one sends text out.

// ghReadOnly are the gh commands that send nothing out; every other gh
// command is treated as a write. A top-level command not listed here or in
// ghKnown may be an alias or extension, which the check cannot read.
var ghReadOnly = map[string]map[string]bool{
	"pr":          {"list": true, "view": true, "diff": true, "checks": true, "status": true, "checkout": true},
	"issue":       {"list": true, "view": true, "status": true},
	"repo":        {"view": true, "list": true, "clone": true},
	"run":         {"list": true, "view": true, "watch": true, "download": true},
	"release":     {"list": true, "view": true, "download": true},
	"workflow":    {"list": true, "view": true},
	"label":       {"list": true},
	"gist":        {"list": true, "view": true, "clone": true},
	"cache":       {"list": true},
	"ruleset":     {"list": true, "view": true, "check": true},
	"variable":    {"list": true, "get": true},
	"secret":      {"list": true},
	"org":         {"list": true},
	"codespace":   {"list": true, "view": true},
	"extension":   {"list": true, "search": true, "browse": true},
	"alias":       {"list": true},
	"config":      {"get": true, "list": true},
	"auth":        {"status": true, "token": true},
	"project":     {"list": true, "view": true, "field-list": true, "item-list": true},
	"attestation": {"verify": true, "download": true},
}

// ghTopReadOnly are gh commands that send nothing whatever follows.
var ghTopReadOnly = flagSet("search", "browse", "status", "help", "version", "completion")

// ghKnown are gh's own top-level commands; any other is an alias or an
// extension.
var ghKnown = flagSet("pr", "issue", "repo", "run", "release", "workflow", "label", "gist", "cache",
	"ruleset", "variable", "secret", "org", "codespace", "extension", "alias", "config", "auth", "project",
	"api", "search", "browse", "status", "help", "version", "completion", "ssh-key", "gpg-key",
	"attestation", "copilot", "agent-task", "preview")

// ghBodyFiles are flags whose value is a file whose content is sent.
var ghBodyFiles = [][2]string{{"-F", "--body-file"}, {"", "--notes-file"}, {"", "--recover"},
	{"", "--env-file"}}

// ghOutbound reads a gh command (its arguments after "gh") run in dir with
// env; nil when it sends nothing out.
func ghOutbound(args []string, dir string, env map[string]string) (*Outbound, bool) {
	repo, set := env["GH_REPO"]
	// Reason: an empty GH_REPO (cleared or unset in the command) leaves gh
	// to pick the repo where the hook's own GH_REPO would say otherwise.
	unknown := repo == unknownValue || set && repo == ""
	var subs, positional, flags []string
	var files []string
	method, fields, input, owner := "", false, "", ""
	crossSource := false
	api := len(args) > 0 && args[0] == "api"
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, n, ok := flagValue(args, i, "-R", "--repo"); ok {
			repo, i = v, n
			continue
		}
		if api {
			if v, n, ok := flagValue(args, i, "-X", "--method"); ok {
				method, i = strings.ToUpper(v), n
				continue
			}
			if v, n, ok := flagValue(args, i, "", "--input"); ok {
				input, i = inDir(dir, v), n
				continue
			}
			if v, n, ok, isField := apiField(args, i); ok {
				i = n
				if isField {
					fields = true
					if _, val, ok := strings.Cut(v, "="); ok && strings.HasPrefix(val, "@") {
						files = append(files, inDir(dir, strings.TrimPrefix(val, "@")))
					}
				}
				continue
			}
		} else {
			matched := false
			bodyFiles := ghBodyFiles
			if len(subs) > 0 && (subs[0] == "secret" || subs[0] == "variable") {
				// secret set -f and variable set -f read values from a file.
				bodyFiles = append(bodyFiles, [2]string{"-f", "--env-file"})
			}
			for _, f := range bodyFiles {
				if v, n, ok := flagValue(args, i, f[0], f[1]); ok {
					files, i, matched = append(files, inDir(dir, v)), n, true
				}
			}
			if matched {
				continue
			}
			for _, f := range [][2]string{{"", "--owner"}, {"", "--org"}} {
				if v, n, ok := flagValue(args, i, f[0], f[1]); ok {
					owner, i, matched = v, n, true
				}
			}
			for _, f := range [][2]string{{"", "--template"}, {"-s", "--source"}, {"-H", "--head"}} {
				if v, n, ok := flagValue(args, i, f[0], f[1]); ok {
					// Reason: content copied from another repo (a template,
					// a sync source, another owner's head branch) is not
					// read here.
					if strings.Contains(v, "/") || strings.Contains(v, ":") {
						crossSource = true
					}
					i, matched = n, true
				}
			}
			if matched {
				continue
			}
			if len(subs) > 0 && ghValued[subs[0]][a] && i+1 < len(args) {
				i++
				continue
			}
		}
		switch {
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		case len(subs) < 2:
			subs = append(subs, a)
		default:
			positional = append(positional, a)
		}
	}
	if len(subs) == 0 {
		return nil, unknown
	}
	if len(subs) > 1 && subs[1] == "new" {
		// gh's own alias of create (pr, issue, repo, release, gist).
		subs[1] = "create"
	}
	act := &Outbound{Dir: dir, Files: files, GHEnv: ghEnvOf(env)}
	if r, ok := parseRepo(repo); ok {
		act.Repo = r
	} else if repo != "" && repo != unknownValue {
		unknown = true
	}
	if api {
		a, unread := ghAPIOutbound(act, subs, method, fields, input)
		return a, unknown || unread
	}
	top := subs[0]
	switch {
	case !ghKnown[top] || top == "extension" && len(subs) > 1 && subs[1] == "exec":
		// An alias or extension: what it runs is not in the command.
		return &Outbound{Kind: "gh " + top, Dir: dir, GHDefault: true, GHEnv: act.GHEnv}, true
	case ghTopReadOnly[top]:
		return nil, unknown
	case len(subs) < 2:
		return nil, unknown
	case ghReadOnly[top][subs[1]]:
		return nil, unknown
	}
	act.Kind = "gh " + top + " " + subs[1]
	if owner != "" {
		act.Repo, act.Org = "", owner
	}
	if crossSource || top == "repo" && subs[1] == "fork" || top == "label" && subs[1] == "clone" ||
		top == "issue" && subs[1] == "transfer" || top == "pr" && subs[1] == "create" && fills(flags) ||
		top == "release" && slicesContains(flags, "--notes-from-tag") {
		// A fork, a cloned label set, an issue moved to another repo, a PR
		// filled from its commits' messages or release notes taken from a
		// local tag copies content the check does not read, or sends it to
		// a second destination.
		unknown = true
	}
	for _, p := range positional {
		// A PR or issue named by URL, or a repo named as owner/repo.
		if i := strings.Index(strings.ToLower(p), "github.com/"); i >= 0 {
			if i > 0 && !strings.ContainsRune("/.@", rune(p[i-1])) {
				// Reason: evilgithub.com/... is another host.
				unknown = true
			} else if r, ok := parseRepo(p[i:]); ok {
				act.Repo = r
			}
		} else if top == "repo" && strings.Count(p, "/") == 1 && act.Repo == "" {
			if r, ok := parseRepo(p); ok {
				act.Repo = r
			}
		}
	}
	switch {
	case top == "gist":
		// A gist belongs to no repo or org; its files are sent, or stdin
		// when none is named.
		act.NoRepo = true
		for _, p := range positional {
			act.Files = append(act.Files, inDir(dir, p))
		}
		if subs[1] == "create" && len(positional) == 0 {
			act.Files = append(act.Files, "-")
		}
	case top == "release" && (subs[1] == "upload" || subs[1] == "create") && len(positional) > 1:
		for _, p := range positional[1:] {
			f, _, _ := strings.Cut(p, "#")
			act.Files = append(act.Files, inDir(dir, f))
		}
	case top == "repo" && subs[1] == "create" && slicesContains(args, "--push"):
		// A new repository holds nothing yet: every commit is sent.
		act.Push, act.ExcludeAll = true, true
		act.Refs = []string{"--branches"}
	case top == "ssh-key" || top == "gpg-key" || top == "auth":
		act.NoRepo = true
	}
	if act.Repo == "" && act.Org == "" && !act.NoRepo {
		// Baseline rule gh_default_repo (D-60): gh picks the repo.
		act.GHDefault = true
	}
	return act, unknown
}

// fills reports whether gh pr create's flags fill the PR from its
// commits: --fill, --fill-first, --fill-verbose (also with =value), or -f
// alone or in a bundle of short flags (-fB main).
func fills(flags []string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, "--fill") || !strings.HasPrefix(f, "--") && strings.ContainsRune(f, 'f') {
			return true
		}
	}
	return false
}

// ghValued are, per gh command, flags whose separate value is not a file
// or an operand.
var ghValued = map[string]map[string]bool{
	"release": {"-t": true, "--title": true, "-n": true, "--notes": true, "--target": true,
		"--discussion-category": true, "--notes-start-tag": true},
	"gist": {"-d": true, "--desc": true, "-f": true, "--filename": true},
	"pr": {"-t": true, "--title": true, "-b": true, "--body": true, "-B": true, "--base": true,
		"-a": true, "--assignee": true, "-l": true, "--label": true, "-m": true, "--milestone": true,
		"-r": true, "--reviewer": true, "-p": true, "--project": true},
	"issue": {"-t": true, "--title": true, "-b": true, "--body": true, "-a": true, "--assignee": true,
		"-l": true, "--label": true, "-m": true, "--milestone": true, "-p": true, "--project": true,
		"-c": true, "--comment": true},
}

// apiField reads gh api's -f/--raw-field, -F/--field and -H/--header
// flags; isField is set for the fields.
func apiField(args []string, i int) (val string, next int, ok, isField bool) {
	for _, f := range [][2]string{{"-f", "--raw-field"}, {"-F", "--field"}} {
		if v, n, ok := flagValue(args, i, f[0], f[1]); ok {
			return v, n, true, true
		}
	}
	if v, n, ok := flagValue(args, i, "-H", "--header"); ok {
		return v, n, true, false
	}
	for _, f := range [][2]string{{"-q", "--jq"}, {"-t", "--template"}, {"", "--hostname"}, {"-p", "--preview"}, {"", "--cache"}} {
		if v, n, ok := flagValue(args, i, f[0], f[1]); ok {
			return v, n, true, false
		}
	}
	return "", i, false, false
}

// ghAPIOutbound reads gh api: a write is any method but GET, or fields or
// an input without one. unread is set for a write whose destination the
// check cannot tell: one that copies a repo to an owner named in its
// fields (a fork, a transfer, a repo generated from a template), a URL on
// another host than api.github.com, or a repos/ path it cannot read.
func ghAPIOutbound(act *Outbound, subs []string, method string, fields bool, input string) (_ *Outbound, unread bool) {
	if input != "" {
		act.Files = append(act.Files, input)
	}
	write := method != "" && method != "GET" && method != "HEAD" || method == "" && (fields || input != "")
	if !write || len(subs) < 2 {
		return nil, false
	}
	path, _, _ := strings.Cut(strings.TrimSuffix(strings.ToLower(subs[1]), "/"), "?")
	unread = strings.HasSuffix(path, "/forks") || strings.HasSuffix(path, "/transfer") || strings.HasSuffix(path, "/generate")
	act.Kind = "gh api " + method
	ep := strings.TrimPrefix(subs[1], "/")
	low := strings.ToLower(ep)
	for _, p := range []string{"https://api.github.com/", "http://api.github.com/", "api.github.com/"} {
		if strings.HasPrefix(low, p) {
			ep, low = ep[len(p):], low[len(p):]
			break
		}
	}
	if strings.Contains(low, "://") || dotSegments(low) {
		// Reason: evil.example/api.github.com/... is another host, and a
		// dot segment may move the request to another repo than the one
		// its path starts with.
		unread = true
	}
	if rest, ok := strings.CutPrefix(ep, "repos/"); ok {
		switch r, ok := parseRepo(rest); {
		case ok && !strings.ContainsAny(r, "{}:"):
			act.Repo = r
		case strings.HasPrefix(rest, "{owner}/{repo}") || strings.HasPrefix(rest, ":owner/:repo"):
			// gh fills in its own repo (gh_default_repo).
			act.GHDefault, act.GHUnmarked = true, true
		default:
			unread = true
		}
	} else if org, ok := ghOrgOf(ep); ok {
		act.Org = org
	} else {
		// graphql, user/..., gists: no repo picks the destination.
		act.Repo, act.NoRepo = "", true
	}
	return act, unread
}

// dotSegments reports whether a lowercase API path holds a "." or ".."
// segment, or an escaped dot, slash or backslash that could make one.
func dotSegments(path string) bool {
	if strings.Contains(path, "%2e") || strings.Contains(path, "%2f") || strings.Contains(path, "%5c") {
		return true
	}
	p, _, _ := strings.Cut(path, "?")
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ghOrgOf reads orgs/<o>[/...] endpoints.
func ghOrgOf(ep string) (string, bool) {
	rest, ok := strings.CutPrefix(ep, "orgs/")
	if !ok {
		return "", false
	}
	org, _, _ := strings.Cut(rest, "/")
	return org, org != ""
}

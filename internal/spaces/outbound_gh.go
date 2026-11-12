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
	repo := env["GH_REPO"]
	unknown := repo == unknownValue
	var subs, positional []string
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
		case len(subs) < 2:
			subs = append(subs, a)
		default:
			positional = append(positional, a)
		}
	}
	if len(subs) == 0 {
		return nil, unknown
	}
	act := &Outbound{Dir: dir, Files: files}
	if r, ok := parseRepo(repo); ok {
		act.Repo = r
	} else if repo != "" && repo != unknownValue {
		unknown = true
	}
	if api {
		return ghAPIOutbound(act, subs, method, fields, input), unknown
	}
	top := subs[0]
	switch {
	case !ghKnown[top] || top == "extension" && len(subs) > 1 && subs[1] == "exec":
		// An alias or extension: what it runs is not in the command.
		return &Outbound{Kind: "gh " + top, Dir: dir, AllRemotes: true}, true
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
		slicesContains(args, "--fill") || slicesContains(args, "--fill-first") || slicesContains(args, "--fill-verbose") {
		// A fork, a cloned label set, or a PR filled from commits copies
		// content the check does not read; it is safe only within one
		// space, which the destination check alone cannot tell.
		if crossSource || top == "repo" || top == "label" {
			unknown = true
		}
	}
	for _, p := range positional {
		// A PR or issue named by URL, or a repo named as owner/repo.
		if strings.Contains(strings.ToLower(p), "github.com/") {
			if r, ok := parseRepo(p[strings.Index(strings.ToLower(p), "github.com/"):]); ok {
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
		act.AllRemotes = true
	}
	return act, unknown
}

// ghValued are, per gh command, flags whose separate value is not a file
// or an operand.
var ghValued = map[string]map[string]bool{
	"release": {"-t": true, "--title": true, "-n": true, "--notes": true, "--target": true,
		"--discussion-category": true, "--notes-start-tag": true, "--notes-from-tag": true},
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
// an input without one.
func ghAPIOutbound(act *Outbound, subs []string, method string, fields bool, input string) *Outbound {
	if input != "" {
		act.Files = append(act.Files, input)
	}
	write := method != "" && method != "GET" && method != "HEAD" || method == "" && (fields || input != "")
	if !write || len(subs) < 2 {
		return nil
	}
	act.Kind = "gh api " + method
	ep := strings.TrimPrefix(subs[1], "/")
	if i := strings.Index(strings.ToLower(ep), "api.github.com/"); i >= 0 {
		ep = ep[i+len("api.github.com/"):]
	}
	if rest, ok := strings.CutPrefix(ep, "repos/"); ok {
		if r, ok := parseRepo(rest); ok {
			act.Repo = r
		}
	} else if org, ok := ghOrgOf(ep); ok {
		act.Org = org
	} else {
		// graphql, user/..., gists: no repo picks the destination.
		act.Repo, act.NoRepo = "", true
	}
	return act
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

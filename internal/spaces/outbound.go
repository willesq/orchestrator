package spaces

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Outbound actions (spaces plan SP-044): the commands whose text leaves
// the machine for a destination, so the outbound terms check can hold back
// another space's terms. They are read from the shell command a tool call
// runs: git commit, push and tag, gh writes (every gh command but a known
// read-only one), and gh api writes. What cannot be followed (a variable,
// eval, an alias, another repository named through the environment) makes
// the call unknown, which the check refuses while terms apply.

// Outbound is one outbound action in a tool call.
type Outbound struct {
	// Kind names the action, e.g. "git commit", "gh pr create".
	Kind string
	// Dir is the directory it runs in; "" when a cd before it cannot be
	// followed.
	Dir string
	// Repo is the destination repo as "owner/repo" when the command names
	// it (-R, GH_REPO, a URL, an API endpoint), and Org an org it writes
	// to. With neither, a git command sends to the checkout's push remote
	// and a gh command (GHDefault) to the repo gh itself picks.
	Repo, Org string
	GHDefault bool
	// GHEnv holds the variables the command sets that change which repo
	// gh picks (GH_HOST, GH_CONFIG_DIR), read before the hook's own.
	GHEnv map[string]string
	// GHUnmarked is set when gh may pick its first remote even though
	// gh repo set-default marked another (gh api's {owner}/{repo}).
	GHUnmarked bool
	// NoRepo is set when the destination is no repo or org (a gist, a
	// GraphQL call, the signed-in user): it holds no space.
	NoRepo bool
	// Files are files whose content leaves: a message or body file, an
	// API input, gist files, release assets.
	Files []string
	// Staged is set for a commit: the staged changes leave, and with All
	// (commit -a) the tracked changes too.
	Staged, All bool
	// Push is set when commits leave: Remote is the remote a push names,
	// "" for the default; Refs are the local refs it pushes (none: the
	// checkout's default); Tags are tag names whose messages also leave,
	// "*" for every tag.
	Push   bool
	Remote string
	Refs   []string
	Tags   []string
	// Alias is a git subcommand that is not a known one: the check
	// refuses it when the checkout defines it as an alias.
	Alias string
	// Pending is set when an earlier command of the same call changed
	// what is staged or committed (git add, commit): the work tree's
	// changes and untracked files may then leave too.
	Pending bool
	// ExcludeAll is set for a push to a URL: no remote's tracking refs
	// are known to be its destination's.
	ExcludeAll bool
	// Have are commits the destination already holds, which a push does
	// not send again: the remote objects a pre-push hook is given (SP-045).
	Have []string
	// HaveRemotes are remotes whose tracking refs a push need not send
	// again, read with ExcludeAll (SP-045's pre-push hook).
	HaveRemotes []string
}

// OutboundActions lists the outbound actions of a tool call. unknown is set
// when the call holds a command that may send text out but cannot be read
// in full: the check then cannot vouch for it.
func OutboundActions(tool string, input []byte, cwd string) (acts []Outbound, unknown bool) {
	home, _ := os.UserHomeDir()
	return OutboundActionsIn(tool, input, cwd, home)
}

// OutboundActionsIn is OutboundActions with the home "~" expands to.
func OutboundActionsIn(tool string, input []byte, cwd, home string) (acts []Outbound, unknown bool) {
	acts, unknown, _ = OutboundRead(tool, input, cwd, home)
	return acts, unknown
}

// OutboundRead is OutboundActionsIn that also returns the static words of
// every command, decoded as the shell would pass them, for matching terms
// split by quoting.
func OutboundRead(tool string, input []byte, cwd, home string) (acts []Outbound, unknown bool, words []string) {
	return readOutbound(tool, input, cwd, home, nil)
}

// readOutbound is OutboundRead under the operator's rules (SP-075); nil
// is the baseline.
func readOutbound(tool string, input []byte, cwd, home string, rules *ruleSet) (acts []Outbound, unknown bool, words []string) {
	cmd, workdir, ok := shellCommand(tool, input)
	if !ok || !mayBeOutbound(cmd) && !rules.mentionsCommand(cmd) {
		return nil, false, nil
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil, true, nil
	}
	w := &outboundWalker{home: home, user: os.Getenv("USER"), env: map[string]string{}, vars: map[string]string{},
		rules: rules}
	dir := cwd
	if workdir != "" {
		// Reason: Codex runs the command in its workdir field.
		dir = w.join(cwd, workdir)
	}
	w.script(f, dir)
	return w.acts, w.unknown, w.words
}

// shellCommand is the shell command a tool call runs, and the directory
// it names to run in, if any.
func shellCommand(tool string, input []byte) (string, string, bool) {
	switch tool {
	case "Bash", "shell", "local_shell", "container.exec", "exec_command":
	default:
		return "", "", false
	}
	var obj map[string]any
	if json.Unmarshal(input, &obj) != nil {
		return "", "", false
	}
	workdir, _ := obj["workdir"].(string)
	cmd := obj["command"]
	if tool == "exec_command" {
		cmd = obj["cmd"]
	}
	switch c := cmd.(type) {
	case string:
		return c, workdir, true
	case []any:
		var parts []string
		for _, a := range c {
			s, ok := a.(string)
			if !ok {
				return "", "", false
			}
			parts = append(parts, syntaxQuote(s))
		}
		return strings.Join(parts, " "), workdir, true
	}
	return "", "", false
}

// syntaxQuote quotes s for the shell parser.
func syntaxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unquote drops the characters quoting can split a word with, so the
// cheap test below sees g'i't as git.
var unquote = strings.NewReplacer(`'`, "", `"`, "", `\`, "")

// mayBeOutbound is a cheap test that a command could hold an outbound
// action, so most calls skip parsing. A $ or ` may build a command name
// out of pieces ($'\x67it', g$()it), so those are parsed too.
func mayBeOutbound(cmd string) bool {
	c := strings.ToLower(unquote.Replace(cmd))
	return strings.Contains(c, "git") || strings.Contains(c, "gh") || strings.ContainsAny(cmd, "$`")
}

// wrapperValued are each wrapper's options that take a separate value.
var wrapperValued = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-h": true, "-p": true, "-U": true, "-C": true, "-D": true, "-r": true, "-t": true},
	"doas":    {"-u": true, "-C": true},
	"nice":    {"-n": true},
	"ionice":  {"-c": true, "-n": true, "-p": true, "-P": true, "-u": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"exec":    {"-a": true},
}

// outboundWrappers are commands that run the rest of their line.
var outboundWrappers = flagSet("command", "sudo", "doas", "time", "nice", "nohup", "rtk", "exec", "timeout",
	"stdbuf", "ionice", "caffeinate")

// unwrap drops wrappers that run the rest of the line, keeping env's
// NAME=value words in env and following env -C.
func (w *outboundWalker) unwrap(argv []string, dir string, env map[string]string) ([]string, string) {
	for len(argv) > 0 {
		name := filepath.Base(argv[0])
		switch {
		case name == "env":
			argv, dir = w.unwrapEnv(argv[1:], dir, env)
		case outboundWrappers[name]:
			valued := wrapperValued[name]
			argv = argv[1:]
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				if valued[argv[0]] && len(argv) > 1 {
					argv = argv[1:]
				}
				argv = argv[1:]
			}
			if name == "timeout" && len(argv) > 0 {
				// timeout's first operand is its duration.
				argv = argv[1:]
			}
		default:
			return argv, dir
		}
	}
	return argv, dir
}

// unwrapEnv reads env's options and NAME=value words.
func (w *outboundWalker) unwrapEnv(argv []string, dir string, env map[string]string) ([]string, string) {
	for len(argv) > 0 {
		a := argv[0]
		switch {
		case a == "-C" || a == "--chdir":
			if len(argv) < 2 {
				return nil, dir
			}
			dir, argv = w.join(dir, argv[1]), argv[2:]
			continue
		case strings.HasPrefix(a, "--chdir="):
			dir = w.join(dir, strings.TrimPrefix(a, "--chdir="))
		case a == "-S" || strings.HasPrefix(a, "--split-string"):
			w.unknown = true
		case a == "-u" || a == "--unset" || a == "-P":
			// Options with a separate value: a variable to remove, or
			// where to find the command.
			if len(argv) < 2 {
				return nil, dir
			}
			if a != "-P" {
				env[argv[1]] = ""
			}
			argv = argv[2:]
			continue
		case strings.HasPrefix(a, "--unset="):
			env[strings.TrimPrefix(a, "--unset=")] = ""
		case strings.HasPrefix(a, "-u") && len(a) > 2:
			env[a[2:]] = ""
		case a == "-i" || a == "-" || a == "--ignore-environment":
			// The command starts with no environment: no GH_REPO, and no
			// HOME for git's global config.
			env["GH_REPO"], env["HOME"] = "", ""
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "="):
			k, v, _ := strings.Cut(a, "=")
			env[k] = v
		default:
			return argv, dir
		}
		argv = argv[1:]
	}
	return argv, dir
}

// unknownValue stands for a variable whose value is not known.
const unknownValue = "\x00unknown"

// staticWord is a word's value when the text it adds is all in the
// command: literals and quotes, and a $(cat <<'EOF' ...) here-document.
func staticWord(word *syntax.Word) (string, bool) {
	return staticWordWith(word, nil)
}

// staticWordWith is staticWord that also expands a plain $NAME or ${NAME}
// lookup knows, told whether it is inside double quotes; nil knows none.
func staticWordWith(word *syntax.Word, lookup func(name string, quoted bool) (string, bool)) (string, bool) {
	var b strings.Builder
	ok := true
	var walk func(parts []syntax.WordPart, quoted bool)
	walk = func(parts []syntax.WordPart, quoted bool) {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(unescape(p.Value, quoted))
			case *syntax.SglQuoted:
				if p.Dollar {
					// $'...' decodes escapes, so its text is not what is
					// written.
					ok = false
				}
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				if p.Dollar {
					ok = false
				}
				walk(p.Parts, true)
			case *syntax.ParamExp:
				if lookup != nil && plainParam(p) {
					if v, known := lookup(p.Param.Value, quoted); known {
						b.WriteString(v)
						continue
					}
				}
				ok = false
			case *syntax.CmdSubst:
				// The usual way agents pass a multi-line body: its text is
				// in the command.
				body, isCat := catHeredoc(p)
				if !isCat {
					ok = false
				}
				b.WriteString(body)
			default:
				ok = false
			}
		}
	}
	walk(word.Parts, false)
	return b.String(), ok
}

// plainParam reports whether p is $NAME or ${NAME} with nothing else.
func plainParam(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Split == syntax.OptUnset && p.GlobSubst == syntax.OptUnset && p.RcExpand == syntax.OptUnset &&
		p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 && p.Slice == nil &&
		p.Repl == nil && p.Names == 0 && p.Exp == nil
}

// unescape removes the backslashes the shell removes from a literal:
// before any character outside quotes, and inside double quotes before $,
// `, ", \ and a newline.
func unescape(s string, quoted bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (!quoted || strings.IndexByte("$`\"\\\n", s[i+1]) >= 0) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

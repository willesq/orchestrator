package spaces

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The walk over a shell command that finds its outbound actions (SP-044).

// maxOutboundDepth bounds nested shells (bash -c "...").
const maxOutboundDepth = 4

type outboundWalker struct {
	acts    []Outbound
	unknown bool
	depth   int
	// home and user are $HOME and $USER (baseline rule plain_variables).
	home, user string
	// env holds variables the script exports or sets, which later
	// commands see (GH_REPO, GIT_DIR); unknownValue for one whose value
	// or export is not known.
	env map[string]string
	// vars holds the shell variables the script set, unknownValue for one
	// it may have set to anything (outbound_vars.go). uncertain counts
	// the contexts being walked whose assignments may not run or last
	// (pipelines, subshells, the right side of && and ||, compound
	// commands); noVars is set once no variable can be trusted again.
	vars      map[string]string
	uncertain int
	noVars    bool
	// words are every static word of every command, decoded.
	words []string
	// staged is set once a command changed what is staged or committed;
	// redirected once one changed where git sends or what it runs (a
	// remote, a config key, an alias).
	staged, redirected bool
	// written are files earlier commands wrote ("" for a name not known).
	written []string
}

func (w *outboundWalker) script(f *syntax.File, dir string) {
	for _, s := range f.Stmts {
		dir = w.stmt(s, dir, false)
	}
}

// stmt walks one statement run in dir, piped when it reads a pipe, and
// returns the directory the next statement runs in.
func (w *outboundWalker) stmt(s *syntax.Stmt, dir string, piped bool) string {
	if s == nil {
		return dir
	}
	// Reason: commands in $(...), <(...) and redirections run too.
	for _, r := range s.Redirs {
		w.substs(r, dir)
	}
	defer w.redirWrites(s, dir)
	w.scanVars(s)
	if s.Background || s.Coprocess {
		// Reason: a background job runs in a subshell, maybe after what
		// follows it.
		w.uncertain++
		defer func() { w.uncertain-- }()
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		return w.call(s, c, dir, piped)
	case *syntax.DeclClause:
		w.substs(c, dir)
		w.decl(c, dir)
		return dir
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			w.uncertain++
			w.stmt(c.X, dir, piped)
			w.stmt(c.Y, dir, true)
			w.uncertain--
			return dir
		}
		next := w.stmt(c.X, dir, piped)
		// Reason: the right side of && or || may not run.
		w.uncertain++
		defer func() { w.uncertain-- }()
		return w.stmt(c.Y, next, piped)
	case *syntax.Subshell:
		w.uncertain++
		defer func() { w.uncertain-- }()
		d := dir
		for _, x := range c.Stmts {
			d = w.stmt(x, d, piped)
		}
		return dir
	case *syntax.Block:
		d := dir
		for _, x := range c.Stmts {
			d = w.stmt(x, d, piped)
		}
		return d
	default:
		// Loops, conditionals, functions: walk what they run; a cd inside
		// may or may not run, so the directory after them is unknown.
		w.uncertain++
		defer func() { w.uncertain-- }()
		before := len(w.acts)
		syntax.Walk(s, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.Stmt:
				if n != s {
					w.stmt(n, dir, true)
					return false
				}
			}
			return true
		})
		if len(w.acts) > before {
			w.unknown = true
		}
		if movesDir(s) {
			return ""
		}
		return dir
	}
}

// movesDir reports whether a compound statement may run cd or pushd, so
// the directory after it is unknown.
func movesDir(s *syntax.Stmt) bool {
	moves := false
	syntax.Walk(s, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			switch c.Args[0].Lit() {
			case "cd", "pushd", "popd":
				moves = true
			}
		}
		return !moves
	})
	return moves
}

// substs walks the command and process substitutions inside node. What
// they run sends text out like any command; its output is then used where
// the walker cannot follow it, so an action found there is unknown.
func (w *outboundWalker) substs(node syntax.Node, dir string) {
	w.uncertain++
	defer func() { w.uncertain-- }()
	before := len(w.acts)
	syntax.Walk(node, func(n syntax.Node) bool {
		var stmts []*syntax.Stmt
		switch n := n.(type) {
		case *syntax.CmdSubst:
			if _, ok := catHeredoc(n); ok {
				return false
			}
			stmts = n.Stmts
		case *syntax.ProcSubst:
			stmts = n.Stmts
		default:
			return true
		}
		for _, s := range stmts {
			w.stmt(s, dir, true)
		}
		return false
	})
	if len(w.acts) > before {
		w.unknown = true
	}
}

// call reads one simple command run in dir and returns the directory after
// it: cd and pushd move it.
func (w *outboundWalker) call(s *syntax.Stmt, c *syntax.CallExpr, dir string, piped bool) string {
	w.substs(c, dir)
	var argv []string
	static := true
	extglob := false
	for _, a := range c.Args {
		v, ok := w.word(a, dir)
		if !ok {
			static = false
		}
		argv = append(argv, v)
		w.words = append(w.words, braceWords(a)...)
		if hasExtGlob(a) {
			extglob = true
		}
	}
	w.words = append(w.words, argv...)
	env := map[string]string{}
	for k, v := range w.env {
		env[k] = v
	}
	for _, as := range c.Assigns {
		v := w.assignValue(as, dir)
		env[as.Name.Value] = v
		if len(c.Args) == 0 {
			// NAME=value alone sets a shell variable, which reaches later
			// commands' environment only if it is exported.
			w.setVar(as.Name.Value, v)
			w.setEnv(as.Name.Value, v, false)
		} else {
			// Reason: before a special builtin or a function the
			// assignment lasts (POSIX), so the variable is unknown after.
			w.vars[as.Name.Value] = unknownValue
		}
	}
	nameStatic := len(c.Args) > 0
	if nameStatic {
		_, nameStatic = w.word(c.Args[0], dir)
	}
	argv, dir = w.unwrap(argv, dir, env)
	if len(argv) == 0 {
		return dir
	}
	if !nameStatic || setsVars(strings.ToLower(filepath.Base(argv[0])), argv[1:]) {
		// Reason: forgotten after the command, which still reads the
		// values it was given.
		defer w.forgetVars()
	}
	// Reason: macOS names ignore case (GIT runs git), and zsh's =git
	// runs git found on the path.
	name := strings.ToLower(filepath.Base(strings.TrimPrefix(argv[0], "=")))
	if name == "unset" {
		// Reason: an unset variable leaves the environment, so a GH_REPO
		// the hook sees no longer picks gh's repo.
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				w.env[a] = ""
			}
		}
	}
	switch name {
	case "cd", "pushd":
		// cd sets PWD to where it moves.
		delete(w.vars, "PWD")
		if len(argv) == 2 && static && dir != "" {
			return w.join(dir, argv[1])
		}
		return ""
	case "popd":
		delete(w.vars, "PWD")
		return ""
	}
	in, literal := w.stdin(s)
	stdin := piped || in
	var act *Outbound
	// Reason: a zsh glob qualifier (*(e:...:)) runs code the bash parser
	// reads as a pattern.
	unknown := extglob
	switch {
	case name == "git":
		act, unknown = gitOutbound(argv[1:], dir, env)
		w.gitState(argv[1:], act)
	case name == "gh":
		act, unknown = ghOutbound(argv[1:], dir, env)
		// Reason: what gh reads from stdin (a gist, a variable's value)
		// is not in the command.
		if act != nil && stdin {
			unknown = true
		}
		if ghSetsDefault(argv[1:]) {
			// Reason: it changes the repo later gh commands write to
			// (gh_default_repo), which the check reads before any runs.
			w.redirected = true
		}
	case strings.HasPrefix(name, "git-"):
		// A git helper run directly (git-push).
		unknown = true
	case shellNames[name]:
		w.nested(s, argv[1:], dir, stdin, true)
	case name == "source" || name == ".":
		w.sourced(s, argv[1:], dir, stdin)
	case name == "alias":
		// zsh expands aliases in scripts: the value runs where the alias
		// is used.
		for _, a := range argv[1:] {
			if _, v, ok := strings.Cut(a, "="); ok && mentionsGit([]string{v}) {
				unknown = true
			}
		}
	default:
		// Reason: a program that runs a command from its arguments (eval,
		// xargs, watch, find -exec) given git or gh, or a command whose
		// name is a variable, sends what this walker cannot read.
		if !nameStatic || commandRunners[name] && mentionsGit(argv[1:]) || name == "find" && findRunsGit(argv[1:]) {
			unknown = true
		}
		w.writes(s, name, argv[1:], dir)
	}
	if !nameStatic {
		unknown = true
	}
	if unknown {
		w.unknown = true
	}
	if act == nil {
		return dir
	}
	if in && literal && !piped {
		// Reason: -F - reads the here-document, whose text is kept as
		// words and checked with the command.
		act.Files = dropStdin(act.Files)
	}
	if !static || act.Dir == "" || w.redirected || w.touched(act) {
		w.unknown = true
	}
	if w.staged && (act.Push || act.Staged) {
		act.Pending = true
		if act.Push {
			act.Refs = append(act.Refs, "--branches", "HEAD")
		}
	}
	w.acts = append(w.acts, *act)
	if act.Staged || act.Push || act.Kind == "git tag" {
		w.staged = true
	}
	return dir
}

// shellNames are shells whose -c script, or here-document, is walked.
var shellNames = flagSet("bash", "sh", "zsh", "dash", "ksh")

// gitReadOnly are git subcommands that change neither what is staged or
// committed nor where git sends.
var gitReadOnly = flagSet("status", "diff", "log", "show", "rev-parse", "ls-files", "ls-tree", "ls-remote",
	"grep", "blame", "describe", "shortlog", "cat-file", "for-each-ref", "show-ref", "rev-list",
	"merge-base", "check-ignore", "count-objects", "help", "version", "var", "whatchanged", "range-diff",
	"push", "fetch", "tag", "notes", "format-patch", "archive", "bundle", "fsck", "gc", "maintenance", "reflog")

// gitRemoteChanges are git remote subcommands that change a remote.
var gitRemoteChanges = flagSet("add", "set-url", "rename", "remove", "rm", "set-branches", "set-head")

// gitConfigWrites are git config options that write.
var gitConfigWrites = flagSet("--add", "--replace-all", "--unset", "--unset-all", "--rename-section",
	"--remove-section", "-e", "--edit", "set", "unset", "rename-section", "remove-section", "edit")

// gitState records what a git command changes for the commands after it:
// where git sends or what it runs (a remote, a config key, an alias), or
// what is staged or committed.
func (w *outboundWalker) gitState(args []string, act *Outbound) {
	var operands []string
	sub, i := "", 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "-C" || a == "-c" || a == "--config-env" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	writes := false
	if i < len(args) {
		for _, a := range args[i+1:] {
			if gitConfigWrites[a] {
				writes = true
			}
			if !strings.HasPrefix(a, "-") {
				operands = append(operands, a)
			}
		}
	}
	switch {
	case sub == "":
	case sub == "remote":
		if len(operands) > 0 && gitRemoteChanges[operands[0]] {
			w.redirected = true
		}
	case sub == "config":
		if writes || len(operands) >= 2 {
			w.redirected = true
		}
	case sub == "symbolic-ref" && len(operands) >= 2:
		w.staged = true
	case gitReadOnly[sub] || sub == "submodule" || sub == "worktree" || sub == "remote" || act != nil:
	default:
		// add, rm, mv, checkout, reset, merge, rebase, stash, apply…
		w.staged = true
	}
}

// dropStdin removes "-" (stdin) from files.
func dropStdin(files []string) []string {
	var out []string
	for _, f := range files {
		if f != "-" {
			out = append(out, f)
		}
	}
	return out
}

// stdin reports whether a statement feeds its command from a redirect, a
// here-document or a here-string, and whether all of that is literal
// text, which is then kept as words.
func (w *outboundWalker) stdin(s *syntax.Stmt) (in, literal bool) {
	literal = true
	for _, r := range s.Redirs {
		switch r.Op {
		case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn:
			in, literal = true, false
		case syntax.Hdoc, syntax.DashHdoc:
			in = true
			if r.Hdoc == nil {
				continue
			}
			body, ok := staticWord(r.Hdoc)
			if !ok {
				literal = false
				body = litText(r.Hdoc)
			}
			w.words = append(w.words, body)
		case syntax.WordHdoc:
			in = true
			v, ok := staticWord(r.Word)
			if !ok {
				literal = false
			}
			w.words = append(w.words, v)
		}
	}
	return in, literal && in
}

// join resolves a cd destination against dir, expanding ~ as $HOME is
// known; "" when it is not.
func (w *outboundWalker) join(dir, p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, ok := w.lookup("HOME", dir)
		if !ok || home == "" {
			return ""
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if p == "" || dir == "" && !filepath.IsAbs(p) {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// mentionsGit reports whether a word is, or runs, git or gh, in any case
// and after an = (alias p=git, =git).
func mentionsGit(args []string) bool {
	for _, a := range args {
		for _, f := range strings.FieldsFunc(strings.ToLower(unquote.Replace(a)), func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '=' || r == ':' || r == ';' || r == '&' || r == '|' || r == '(' || r == ')' || r == '`'
		}) {
			if b := filepath.Base(f); b == "git" || b == "gh" || strings.HasPrefix(b, "git-") {
				return true
			}
		}
	}
	return false
}

// commandRunners run a command named in their arguments.
var commandRunners = flagSet("eval", "xargs", "watch", "parallel", "ssh", "script", "xcrun", "arch", "trap",
	"fc", "su", "runuser", "chroot", "unbuffer", "entr", "flock", "setsid", "systemd-run", "launchctl",
	"osascript", "open", "screen", "tmux", "expect", "builtin", "noglob", "nocorrect", "coproc", "zsh-run")

// findRunsGit reports whether find runs git or gh through -exec/-ok.
func findRunsGit(args []string) bool {
	for i, a := range args {
		if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" {
			return mentionsGit(args[i+1:])
		}
	}
	return false
}

// nested walks the script a shell runs: its -c script (also in a flag
// bundle, as in bash -lc) or a literal here-document. A script read from
// a file or a pipe is not followed; one whose words name git is unknown.
func (w *outboundWalker) nested(s *syntax.Stmt, args []string, dir string, stdin, child bool) {
	script, found := "", false
	for i, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			if i+1 < len(args) {
				script, found = args[i+1], true
			}
			break
		}
	}
	if !found && s != nil {
		for _, r := range s.Redirs {
			if (r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc) && r.Hdoc != nil {
				if body, ok := staticWord(r.Hdoc); ok {
					script, found = body, true
				}
			}
			if r.Op == syntax.WordHdoc && r.Word != nil {
				if body, ok := staticWord(r.Word); ok {
					script, found = body, true
				}
			}
		}
	}
	if !found {
		if stdin || mentionsGit(args) {
			w.unknown = true
		}
		return
	}
	if w.depth >= maxOutboundDepth {
		w.unknown = true
		return
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		w.unknown = true
		return
	}
	// Reason: a script sees only exported variables. What a new shell
	// (child) sets or exports does not come back; what a sourced script
	// exports does, and its other variables are forgotten after it.
	outer, outerEnv := w.vars, w.env
	w.vars = map[string]string{}
	for k, v := range w.env {
		w.vars[k] = v
	}
	if child {
		w.env = map[string]string{}
		for k, v := range outerEnv {
			w.env[k] = v
		}
	}
	w.depth++
	w.script(f, dir)
	w.depth--
	w.vars, w.env = outer, outerEnv
}

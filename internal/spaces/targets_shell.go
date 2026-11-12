package spaces

import (
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// maxCwds bounds the directories a shell may be in before its working
// directory counts as unknown.
const maxCwds = 16

// shellState is what one shell knows while a command line runs. cwds holds
// every directory it may be in: a cd can fail, sit in a branch that does
// not run, or follow another that failed, so later relative paths are
// reported against each. unknown is set when a cd may have run any number
// of times (in a loop or a function); relative paths then make the call
// opaque. A subshell, pipeline stage or background job works on a copy,
// so its cd does not leak, as in bash.
type shellState struct {
	cwds    []string
	unknown bool
	oldpwd  []string
	env     map[string]string
	dirs    [][]string
}

func newShellState(cwd, home string) *shellState {
	return &shellState{cwds: []string{cwd}, env: map[string]string{"HOME": home, "PWD": cwd}}
}

func (s *shellState) fork() *shellState {
	c := *s
	c.env = make(map[string]string, len(s.env))
	for k, v := range s.env {
		c.env[k] = v
	}
	c.cwds = slices.Clone(s.cwds)
	c.oldpwd = slices.Clone(s.oldpwd)
	c.dirs = slices.Clone(s.dirs)
	return &c
}

func (s *shellState) at() where { return where{dirs: s.cwds, unknown: s.unknown} }

// move records a cd to dirs.
func (s *shellState) move(dirs []string) {
	s.oldpwd = s.cwds
	s.setCwds(dirs)
}

func (s *shellState) setCwds(dirs []string) {
	dirs = sortedUnique(slices.Clone(dirs))
	if len(dirs) > maxCwds {
		s.unknown = true
		dirs = dirs[:maxCwds]
	}
	s.cwds = dirs
	setSingle(s.env, "PWD", dirs)
	setSingle(s.env, "OLDPWD", s.oldpwd)
}

// forgetVars drops the variables the line set, after a builtin such as
// read, printf -v or let may have changed any of them.
func (s *shellState) forgetVars() {
	for k := range s.env {
		if k != "HOME" && k != "PWD" && k != "OLDPWD" {
			delete(s.env, k)
		}
	}
}

func setSingle(env map[string]string, name string, v []string) {
	if len(v) == 1 {
		env[name] = v[0]
	} else {
		delete(env, name)
	}
}

// where is the set of directories a command may run in.
type where struct {
	dirs    []string
	unknown bool
}

// script parses a shell command line and walks it. Code the parser cannot
// read is flagged opaque, and the paths written in it are still kept.
func (e *extractor) script(src string, st *shellState, depth int) {
	if depth > maxShellDepth {
		e.opaque("shell scripts nested too deep")
		e.embedded(src, st.at(), true, true)
		return
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		e.opaque("shell syntax could not be parsed")
		e.scanAbsolute(src, st)
		return
	}
	for _, s := range f.Stmts {
		e.stmt(s, st, depth)
	}
}

var absToken = regexp.MustCompile(`(?:^|[\s"'=:(<>|;&])((?:~|/)[^\s"';|&<>()$` + "`" + `]*)`)

// scanAbsolute keeps every absolute or ~ path in text that could not be
// parsed, as a prefix, since its end is uncertain.
func (e *extractor) scanAbsolute(text string, st *shellState) {
	for _, m := range absToken.FindAllStringSubmatch(text, -1) {
		p := m[1]
		if p[0] == '~' && p != "~" && !strings.HasPrefix(p, "~/") {
			continue
		}
		p = e.abs(st.cwds[0], p)
		if strings.ContainsAny(p, "*?[{") {
			p = globPrefix(p)
		} else {
			p = prefixDir(p)
		}
		e.add(kindPrefix, p)
	}
	e.githubURLs(text)
}

func (e *extractor) stmt(s *syntax.Stmt, st *shellState, depth int) {
	if s == nil || e.expired() {
		return
	}
	if s.Background || s.Coprocess {
		st = st.fork()
	}
	before := st.cwds
	for _, r := range s.Redirs {
		e.redirect(r, st, depth)
	}
	switch c := s.Cmd.(type) {
	case nil:
	case *syntax.CallExpr:
		e.call(c, st, depth)
	case *syntax.DeclClause:
		e.decl(c, st, depth)
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			// bash runs every stage in a subshell, but zsh (and bash
			// with lastpipe) runs the last one in this shell.
			e.stmt(c.X, st.fork(), depth)
			e.stmt(c.Y, st, depth)
			break
		}
		e.stmt(c.X, st, depth)
		e.stmt(c.Y, st, depth)
	case *syntax.Subshell:
		sub := st.fork()
		for _, x := range c.Stmts {
			e.stmt(x, sub, depth)
		}
	case *syntax.FuncDecl:
		// A function may be called any number of times, and its cd
		// moves the caller.
		sub := st.fork()
		e.stmt(c.Body, sub, depth)
		if sub.unknown || !slices.Equal(sub.cwds, st.cwds) {
			st.unknown = true
		}
	case *syntax.WhileClause, *syntax.ForClause:
		if f, ok := c.(*syntax.ForClause); ok {
			if it, ok := f.Loop.(*syntax.WordIter); ok && it.Name != nil {
				delete(st.env, it.Name.Value)
			}
		}
		start := st.cwds
		e.compound(c, st, depth)
		if !slices.Equal(st.cwds, start) {
			st.unknown = true
		}
	case *syntax.ArithmCmd, *syntax.LetClause:
		st.forgetVars()
		e.compound(c, st, depth)
	default:
		e.compound(c, st, depth)
	}
	// A command can fail before its cd runs, so the shell may still be
	// in any directory it was in before.
	st.setCwds(append(slices.Clone(before), st.cwds...))
}

// compound walks if, case, blocks, [[ ]] and arithmetic in source order.
// Their bodies run in the same shell; their other words are arguments.
func (e *extractor) compound(n syntax.Node, st *shellState, depth int) {
	syntax.Walk(n, func(x syntax.Node) bool {
		switch x := x.(type) {
		case *syntax.Stmt:
			e.stmt(x, st, depth)
			return false
		case *syntax.Word:
			for _, w := range e.words(x, st, depth) {
				e.pathArg(w, st.at())
			}
			return false
		}
		return true
	})
}

func (e *extractor) redirect(r *syntax.Redirect, st *shellState, depth int) {
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		body := r.Hdoc
		if r.Op == syntax.WordHdoc {
			body = r.Word
		}
		if body != nil {
			e.substitutions(body, st, depth)
			e.input(litText(body), st.at())
		}
		return
	case syntax.DplIn, syntax.DplOut:
		if lit := r.Word.Lit(); lit == "-" || isDigits(lit) {
			return
		}
	}
	at := st.at()
	out := r.Op != syntax.RdrIn
	for _, w := range e.words(r.Word, st, depth) {
		if out {
			e.writing++
		}
		e.pathArg(w, at)
		if out {
			e.writing--
		}
		if r.Op != syntax.RdrIn && w.exact() {
			for _, p := range e.absAll(at, w.val) {
				e.written[e.fileKey(p)] = true
			}
		}
	}
}

// input handles text a command reads on stdin: a here-document or
// here-string. Paths in it are kept, and a Codex patch in it is read.
func (e *extractor) input(text string, at where) {
	if strings.Contains(text, "*** Begin Patch") {
		// A patch's files are written (SP-042): Codex's usual form is
		// apply_patch <<'EOF'.
		e.writing++
		e.patch(text, at)
		e.writing--
	}
	e.embedded(text, at, true, true)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (e *extractor) call(c *syntax.CallExpr, st *shellState, depth int) {
	target := st
	if len(c.Args) > 0 && len(c.Assigns) > 0 {
		// FOO=x cmd sets FOO for cmd only.
		target = st.fork()
	}
	for _, a := range c.Assigns {
		e.assign(a, target, depth, false)
	}
	var argv []word
	for _, w := range c.Args {
		argv = append(argv, e.words(w, target, depth)...)
	}
	if len(argv) == 0 {
		return
	}
	e.command(argv, target, depth)
	if target != st {
		st.cwds, st.oldpwd, st.dirs, st.unknown = target.cwds, target.oldpwd, target.dirs, target.unknown
		setSingle(st.env, "PWD", st.cwds)
		setSingle(st.env, "OLDPWD", st.oldpwd)
	}
}

func (e *extractor) decl(c *syntax.DeclClause, st *shellState, depth int) {
	if c.Variant != nil && c.Variant.Value == "nameref" {
		e.opaque("nameref variable")
	}
	for _, a := range c.Args {
		if a.Name == nil && a.Value != nil {
			if lit := a.Value.Lit(); strings.HasPrefix(lit, "-") && strings.Contains(lit, "n") {
				e.opaque("nameref variable")
			}
		}
		e.assign(a, st, depth, true)
	}
}

// assign records a variable the line sets so later words using it can be
// expanded. A value that cannot be known makes the variable unknown, and
// any later use of it opaque. The value is also kept as a path.
func (e *extractor) assign(a *syntax.Assign, st *shellState, depth int, decl bool) {
	if a.Name == nil {
		if a.Value != nil && !strings.HasPrefix(a.Value.Lit(), "-") {
			for _, w := range e.words(a.Value, st, depth) {
				e.pathArg(w, st.at())
			}
		}
		return
	}
	name := a.Name.Value
	if a.Array != nil || a.Index != nil {
		e.specialVar(name, nil, st.at())
	}
	if a.Value == nil && !a.Naked && a.Array == nil && a.Index == nil {
		e.specialVar(name, []word{{}}, st.at())
		st.env[name] = ""
		return
	}
	if a.Value == nil || a.Array != nil || a.Index != nil || a.Append {
		if !(decl && a.Naked) {
			delete(st.env, name)
		}
		if a.Array != nil {
			e.compound(a.Array, st, depth)
		}
		return
	}
	ws := e.words(a.Value, st, depth)
	for _, w := range ws {
		e.pathArg(w, st.at())
	}
	e.specialVar(name, ws, st.at())
	if len(ws) == 1 && ws[0].exact() && len(ws[0].val) <= maxVar {
		st.env[name] = ws[0].val
	} else if len(ws) == 0 {
		st.env[name] = ""
	} else {
		delete(st.env, name)
	}
}

// codeVars hold code or settings that change where a later command
// reaches, in ways Targets does not follow.
var codeVars = flagSet("CDPATH", "cdpath", "BASH_ENV", "ENV", "ZDOTDIR", "HOME", "PROMPT_COMMAND",
	"PS0", "PS1", "PS2", "PS4", "PROMPT", "EDITOR", "VISUAL", "PAGER", "MANPAGER", "LESSOPEN",
	"LESSCLOSE", "BROWSER", "GIT_EXEC_PATH", "GIT_PROXY_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS",
	"GIT_EXTERNAL_DIFF", "GIT_EDITOR", "GIT_PAGER", "GIT_SEQUENCE_EDITOR", "GIT_TEMPLATE_DIR",
	"GH_HOST", "GH_CONFIG_DIR", "GH_EDITOR", "GH_BROWSER", "GH_PAGER", "XDG_CONFIG_HOME",
	"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "SHELLOPTS", "BASHOPTS", "IFS", "FPATH", "fpath")

// specialVar handles variables that point git or gh at a target.
func (e *extractor) specialVar(name string, ws []word, at where) {
	switch {
	case codeVars[name] || strings.HasPrefix(name, "GIT_SSH") || strings.HasPrefix(name, "GIT_CONFIG") ||
		strings.HasSuffix(name, "_COMMAND") || strings.HasSuffix(name, "EDITOR") || strings.HasSuffix(name, "PAGER"):
		e.opaque("a variable that changes how commands run is set")
	case name == "GIT_DIR" || name == "GIT_WORK_TREE" || name == "GIT_COMMON_DIR":
		for _, w := range ws {
			if !w.exact() {
				e.opaque("git directory is not static")
				continue
			}
			for _, p := range e.absAll(at, w.val) {
				e.add(kindGit, p)
			}
		}
	case name == "GH_REPO":
		for _, w := range ws {
			e.ghRepoArg(w)
		}
	}
}

// substitutions walks the command and process substitutions in a word
// whose own value is not a path, such as a here-document body.
func (e *extractor) substitutions(w *syntax.Word, st *shellState, depth int) {
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst:
			e.opaque("command substitution")
			sub := st.fork()
			for _, s := range n.Stmts {
				e.stmt(s, sub, depth)
			}
			return false
		case *syntax.ProcSubst:
			e.opaque("process substitution")
			sub := st.fork()
			for _, s := range n.Stmts {
				e.stmt(s, sub, depth)
			}
			return false
		}
		return true
	})
}

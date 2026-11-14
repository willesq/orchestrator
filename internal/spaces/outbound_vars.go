package spaces

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Variables the outbound walk reads (SP-044, D-60). A command's words are
// static text, plus the shell variables whose value the walk knows: $PWD,
// $USER and $HOME as the shell has them (baseline rule plain_variables),
// and variables given a literal value earlier in the same command where
// that assignment always runs before the use (command_assignments). Any
// other expansion, or a known variable anything may have changed since
// (read, eval, a loop, arithmetic, a function), is unknown, and the check
// fails closed as before.

// shellVarSetters are commands that may set a shell variable the walk cannot
// follow; after one, no variable is known.
var shellVarSetters = flagSet("read", "mapfile", "readarray", "getopts", "unset", "let", "eval", "source", ".",
	"wait", "builtin", "coproc", "typeset", "export", "declare", "local", "readonly", "vared", "zparseopts",
	"zstyle", "set", "shift")

// setsVars reports whether command name, given args, may set a shell
// variable: a varSetters command, or printf -v and zsh's print -v.
func setsVars(name string, args []string) bool {
	if shellVarSetters[name] {
		return true
	}
	if name == "printf" || name == "print" {
		for _, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "v") {
				return true
			}
		}
	}
	return false
}

// plainVars are the variables read from the hook's view of the session
// when the command has not set them.
var plainVars = flagSet("PWD", "USER", "HOME")

// lookup is the value of variable name in a command running in dir.
func (w *outboundWalker) lookup(name, dir string) (string, bool) {
	if w.noVars {
		return "", false
	}
	if v, ok := w.vars[name]; ok {
		// Baseline rule command_assignments.
		return v, v != unknownValue
	}
	// Baseline rule plain_variables.
	switch name {
	case "HOME":
		return w.home, w.home != ""
	case "PWD":
		return dir, dir != ""
	case "USER":
		return w.user, w.user != ""
	}
	return "", false
}

// word is a word's value in a command running in dir, with the variables
// the walk knows expanded.
func (w *outboundWalker) word(word *syntax.Word, dir string) (string, bool) {
	return staticWordWith(word, func(name string, quoted bool) (string, bool) {
		v, ok := w.lookup(name, dir)
		// Reason: unquoted, the shell splits the value on blanks and
		// expands globs in it (and drops it when empty); only a value
		// those leave alone is one word as read here.
		if !ok || !quoted && (v == "" || strings.ContainsAny(v, " \t\n*?[")) {
			return "", false
		}
		return v, true
	})
}

// setVar records a shell variable's value, unknown where the assignment
// may not run before a later use or may not last.
func (w *outboundWalker) setVar(name, v string) {
	if w.uncertain > 0 {
		v = unknownValue
	}
	w.vars[name] = v
}

// forgetVars makes every variable unknown, the plain ones too, and the
// value of every variable the command exported.
func (w *outboundWalker) forgetVars() {
	for k := range w.vars {
		w.vars[k] = unknownValue
	}
	for k := range plainVars {
		w.vars[k] = unknownValue
	}
	for k, v := range w.env {
		if v != "" {
			w.env[k] = unknownValue
		}
	}
}

// distrust gives up on variables for the rest of the command: after a
// function, alias, trap, name reference or IFS change, any later word may
// expand to anything.
func (w *outboundWalker) distrust() {
	w.noVars = true
	w.forgetVars()
}

// assignValue is a bare or declared assignment's value: unknown for an
// append, an array or an element.
func (w *outboundWalker) assignValue(as *syntax.Assign, dir string) string {
	if as.Append || as.Array != nil || as.Index != nil {
		return unknownValue
	}
	if as.Value == nil {
		return ""
	}
	v, ok := w.word(as.Value, dir)
	if !ok {
		return unknownValue
	}
	return v
}

// scanVars marks what a statement may set without a plain assignment:
// ${NAME=...} and ${NAME:=...} set NAME, arithmetic may set anything, a
// for or select loop sets its variable, and a function or a trap may run
// anywhere later, so after one no variable is known again.
func (w *outboundWalker) scanVars(node syntax.Node) {
	syntax.Walk(node, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.ParamExp:
			if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) && n.Param != nil {
				w.vars[n.Param.Value] = unknownValue
			}
		case *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.CStyleLoop, *syntax.LetClause:
			w.forgetVars()
		case *syntax.WordIter:
			if n.Name != nil {
				w.vars[n.Name.Value] = unknownValue
				if _, ok := w.env[n.Name.Value]; ok {
					w.env[n.Name.Value] = unknownValue
				}
			}
		case *syntax.FuncDecl:
			w.distrust()
		case *syntax.Assign:
			if n.Name != nil && n.Name.Value == "IFS" {
				w.distrust()
			}
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				switch n.Args[0].Lit() {
				case "trap", "alias", "function":
					w.distrust()
				}
			}
		}
		return true
	})
}

// decl reads export, declare, local, readonly and typeset: the shell
// variables they set, and the environment of later commands for those they
// export (export, or the -x option). Options other than -x, -r and -g (an
// integer, a name reference, an array, case folding) change the value,
// which is then unknown. A plain export NAME exports the value it has.
func (w *outboundWalker) decl(d *syntax.DeclClause, dir string) {
	plain, exports := true, d.Variant != nil && d.Variant.Value == "export"
	for _, a := range d.Args {
		if a.Name == nil && a.Value != nil {
			f, ok := staticWord(a.Value)
			if !ok || strings.Trim(f, "-+xrg") != "" {
				plain = false
			}
			if !ok || strings.HasPrefix(f, "-") && strings.Contains(f, "n") {
				// A name reference makes one name set another.
				w.distrust()
			}
			if strings.HasPrefix(f, "-") && strings.Contains(f, "x") {
				exports = true
			}
		}
	}
	for _, a := range d.Args {
		if a.Name == nil {
			continue
		}
		name := a.Name.Value
		if a.Naked {
			// Reason: export NAME exports what NAME holds; an unknown
			// value counts as set, as a variable from the hook's own
			// environment might.
			if v, ok := w.vars[name]; ok && exports {
				w.env[name] = v
			} else if exports {
				w.env[name] = unknownValue
			}
			if !plain {
				w.vars[name] = unknownValue
			}
			continue
		}
		v := w.assignValue(a, dir)
		if !plain {
			v = unknownValue
		}
		w.setVar(name, v)
		w.setEnv(name, v, exports)
	}
}

// setEnv records name's value in the environment of later commands: as v
// when the assignment exports it or name is already exported, and as set
// to an unknown value otherwise, since the walk cannot tell whether the
// hook's environment exports name.
func (w *outboundWalker) setEnv(name, v string, exports bool) {
	if _, exported := w.env[name]; exports || exported && w.env[name] != unknownValue {
		if w.uncertain > 0 {
			v = unknownValue
		}
		w.env[name] = v
		return
	}
	w.env[name] = unknownValue
}

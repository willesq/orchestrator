package spaces

import (
	"bytes"
	"path/filepath"
	"strings"
)

// shell handles sh, bash, zsh and the like: a -c script is parsed and
// walked in a new shell, a script file is read and walked, and a shell
// reading its script from stdin cannot be analysed.
func (e *extractor) shell(name string, args []word, at where, st *shellState, depth int) {
	inline, stdin := false, false
	i := 0
	for ; i < len(args); i++ {
		v := args[i].val
		if v == "--" || v == "-" {
			stdin = stdin || v == "-"
			i++
			break
		}
		if !strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "+") {
			break
		}
		switch {
		case v == "-o" || v == "+o" || v == "-O" || v == "+O":
			i++
		case v == "--rcfile" || v == "--init-file":
			if i+1 < len(args) {
				i++
				e.runScript(args[i], at, st, depth, false)
			}
		case v == "--command" || name == "fish" && (v == "-C" || v == "--init-command"):
			inline = true
		case !strings.HasPrefix(v, "--"):
			inline = inline || strings.Contains(v, "c")
			stdin = stdin || strings.Contains(v, "s")
		}
	}
	// A trailing -o or --rcfile may skip past the last argument.
	i = min(i, len(args))
	switch {
	case inline && i < len(args):
		script := args[i]
		if !script.exact() {
			e.opaque("shell -c script is not static")
			e.pathArg(script, at)
			return
		}
		sub := st.fork()
		sub.cwds, sub.unknown = at.dirs, at.unknown
		e.script(script.val, sub, depth+1)
		e.genericArgs(args[i+1:], at)
	case inline:
		e.opaque("shell -c without a script")
	case stdin || i >= len(args):
		e.opaque("shell reads its script from stdin")
		e.genericArgs(args[i:], at)
	default:
		e.runScript(args[i], at, st, depth, false)
		e.genericArgs(args[i+1:], at)
	}
}

func isStdinPath(p string) bool {
	return p == "-" || p == "/dev/stdin" || strings.HasPrefix(p, "/dev/fd/") ||
		strings.HasPrefix(p, "/proc/") && strings.Contains(p, "/fd/")
}

// runScript records a script the call runs and walks its content: in this
// shell for source, in a new one otherwise. A script read from stdin or
// written by the call itself cannot be read ahead and is opaque.
func (e *extractor) runScript(w word, at where, st *shellState, depth int, same bool) {
	e.pathArg(w, at)
	if !w.exact() {
		e.opaque("script path is not static")
		return
	}
	if isStdinPath(w.val) {
		e.opaque("script read from stdin")
		return
	}
	for _, p := range e.absAll(at, w.val) {
		if e.written[e.fileKey(p)] {
			e.opaque("runs a script the call writes")
			continue
		}
		sub := st
		if !same {
			sub = st.fork()
			sub.cwds, sub.unknown = at.dirs, at.unknown
		}
		e.scriptFile(p, sub, depth)
	}
}

// scriptFile reads a script and walks it as shell code when it is one. A
// compiled program is not a script. A script in another language is
// searched for the paths written in it.
func (e *extractor) scriptFile(p string, st *shellState, depth int) {
	if e.scriptReads >= maxScriptReads {
		e.opaque("too many scripts to read")
		return
	}
	e.scriptReads++
	data, err := readSmall(e.res.resolve(p).path, maxScript+1)
	if err != nil {
		e.opaque("script could not be read")
		return
	}
	if isBinary(data) {
		return
	}
	data = bytes.ReplaceAll(data, []byte{0}, []byte{' '})
	if len(data) > maxScript || e.scriptBytes+len(data) > maxScriptBytes {
		e.opaque("script too large to read")
		return
	}
	e.scriptBytes += len(data)
	text := string(data)
	if strings.HasPrefix(text, "#!") && !posixShebang(text) {
		e.embedded(text, st.at(), true, true)
		return
	}
	e.script(text, st, depth+1)
}

// isBinary reports a compiled program by its magic number: ELF, Mach-O
// or a universal binary. Anything else may be run as a script, even with
// NUL bytes in it.
func isBinary(data []byte) bool {
	for _, m := range [][]byte{{0x7f, 'E', 'L', 'F'}, {0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
		{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.HasPrefix(data, m) {
			return true
		}
	}
	return false
}

// posixShebang reports whether a script's #! line names a shell the bash
// parser reads.
func posixShebang(text string) bool {
	line, _, _ := strings.Cut(text[2:], "\n")
	f := strings.Fields(line)
	if len(f) == 0 {
		return true
	}
	prog := filepath.Base(f[0])
	if prog == "env" {
		prog = ""
		for _, a := range f[1:] {
			if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
				prog = filepath.Base(a)
				break
			}
		}
	}
	switch prog {
	case "sh", "bash", "zsh", "dash", "ksh", "mksh", "ash":
		return true
	}
	return false
}

// interpreter flags inline code and code read from stdin as opaque; a
// script file is a path whose content is searched for paths. Flags after
// the script or "-m module" belong to the program, not the interpreter.
func (e *extractor) interpreter(args []word, at where) {
	script, module, info := -1, false, false
	for i, a := range args {
		v := a.val
		if script >= 0 || module {
			break
		}
		switch {
		case v == "--version" || v == "-V" || v == "-v" || v == "--help" || v == "-h":
			info = true
		case v == "-m":
			module = true
		case i == 0 && (v == "eval" || v == "repl"):
			e.opaque("inline interpreter code")
		case v == "-":
			e.opaque("interpreter reads code from stdin")
		case strings.HasPrefix(v, "--eval") || strings.HasPrefix(v, "--print") || strings.HasPrefix(v, "--command") ||
			strings.HasPrefix(v, "--exec") || strings.HasPrefix(v, "--require") || strings.HasPrefix(v, "--import") ||
			strings.HasPrefix(v, "--loader"):
			e.opaque("inline interpreter code")
		case strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") && strings.ContainsAny(v[1:], "ceEprRBF"):
			e.opaque("inline interpreter code")
		case !strings.HasPrefix(v, "-"):
			script = i
		}
	}
	switch {
	case script >= 0 && args[script].exact() && !isStdinPath(args[script].val):
		for _, p := range e.absAll(at, args[script].val) {
			if e.written[e.fileKey(p)] {
				e.opaque("runs a script the call writes")
				continue
			}
			if data, err := readSmall(e.res.resolve(p).path, maxScript); err == nil {
				e.embedded(string(data), at, true, true)
			}
		}
	case script < 0 && !module && !info:
		e.opaque("interpreter reads code from stdin")
	}
	e.genericArgs(args, at)
}

// find runs the command after -exec, -execdir, -ok and -okdir, up to ";"
// or "+", for each file it finds.
func (e *extractor) find(args []word, at where, st *shellState, depth int) {
	var rest []word
	for i := 0; i < len(args); i++ {
		v := args[i].val
		if v != "-exec" && v != "-execdir" && v != "-ok" && v != "-okdir" {
			rest = append(rest, args[i])
			continue
		}
		j := i + 1
		for j < len(args) && args[j].val != ";" && args[j].val != "+" {
			j++
		}
		sub := st.fork()
		sub.cwds, sub.unknown = at.dirs, at.unknown || strings.HasSuffix(v, "dir")
		if depth < maxShellDepth {
			e.command(args[i+1:j], sub, depth+1)
		} else {
			e.opaque("shell scripts nested too deep")
		}
		i = j
	}
	e.genericArgs(rest, at)
}

// su runs -c's script as another user; without -c it starts a shell that
// reads stdin.
func (e *extractor) su(args []word, at where, st *shellState, depth int) {
	for i := 0; i < len(args); i++ {
		v := args[i].val
		if (v == "-c" || v == "--command") && i+1 < len(args) {
			e.shell("sh", []word{{val: "-c"}, args[i+1]}, at, st, depth)
			return
		}
		if c, ok := strings.CutPrefix(v, "--command="); ok {
			e.shell("sh", []word{{val: "-c"}, {val: c, partial: args[i].partial}}, at, st, depth)
			return
		}
	}
	e.opaque("su starts a shell")
	e.genericArgs(args, at)
}

// watch joins its operands into a shell command, as it does when it runs
// them with sh -c.
func (e *extractor) watch(args []word, at where, st *shellState, depth int) {
	i, argv := 0, false
	for i < len(args) && strings.HasPrefix(args[i].val, "-") {
		switch v := args[i].val; v {
		case "-n", "--interval":
			i++
		case "-x", "--exec":
			argv = true
		}
		i++
	}
	if i >= len(args) {
		return
	}
	if argv {
		sub := st.fork()
		sub.cwds, sub.unknown = at.dirs, at.unknown
		e.command(args[i:], sub, depth+1)
		return
	}
	if !allExact(args[i:]) {
		e.opaque("watch command is not static")
		e.genericArgs(args[i:], at)
		return
	}
	e.shell("sh", []word{{val: "-c"}, {val: joinWords(args[i:])}}, at, st, depth)
}

// sshValued are ssh options that take a separate value.
var sshValued = flagSet("-p", "-i", "-l", "-o", "-F", "-J", "-L", "-R", "-D", "-b", "-c", "-E", "-e", "-m",
	"-O", "-Q", "-S", "-W", "-w", "-B", "-I")

// ssh runs its command in the remote account's home. The host may be this
// machine, so the command is walked as if run in $HOME here, as a guess.
func (e *extractor) ssh(args []word, st *shellState, depth int) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i].val, "-") {
		if sshValued[args[i].val] {
			i++
		}
		i++
	}
	if i+1 < len(args) && depth < maxShellDepth && allExact(args[i+1:]) {
		sub := st.fork()
		sub.cwds, sub.unknown = []string{st.env["HOME"]}, false
		e.spec++
		e.script(joinWords(args[i+1:]), sub, depth+1)
		e.spec--
	}
	e.genericArgs(args, st.at())
}

// argv handles a command given as an argument vector, as Codex's shell
// tool may send it.
func (e *extractor) argv(argv []string, st *shellState, depth int) {
	ws := make([]word, len(argv))
	for i, a := range argv {
		ws[i] = word{val: a}
	}
	if len(ws) > 0 {
		e.command(ws, st, depth)
	}
}

func hasFlag(args []word, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a.val == f {
				return true
			}
		}
		if len(a.val) > 1 && a.val[0] == '-' && a.val[1] != '-' {
			for _, f := range flags {
				if len(f) == 2 && f[0] == '-' && strings.ContainsRune(a.val[1:], rune(f[1])) {
					return true
				}
			}
		}
	}
	return false
}

func allExact(ws []word) bool {
	for _, w := range ws {
		if !w.exact() {
			return false
		}
	}
	return true
}

func joinWords(ws []word) string {
	s := make([]string, len(ws))
	for i, w := range ws {
		s[i] = w.val
	}
	return strings.Join(s, " ")
}

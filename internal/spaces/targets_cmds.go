package spaces

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	// maxScript bounds one script file Targets reads; maxScriptBytes all
	// the script files of one call.
	maxScript      = 64 << 10
	maxScriptBytes = 256 << 10
)

// wrapper describes a command that runs another command: its flags that
// take a value, the flags among them that change directory, and how many
// operands come before the command (timeout's duration, chroot's root).
type wrapper struct {
	valued      map[string]bool
	chdir       map[string]bool
	positionals int
}

func flagSet(fs ...string) map[string]bool {
	m := make(map[string]bool, len(fs))
	for _, f := range fs {
		m[f] = true
	}
	return m
}

var wrappers = map[string]wrapper{
	"sudo": {valued: flagSet("-u", "-g", "-C", "-h", "-p", "-r", "-t", "-T", "-U", "-D", "--user", "--group", "--command-timeout",
		"--close-from", "--host", "--prompt", "--role", "--type", "--other-user", "--chdir"),
		chdir: flagSet("-D", "--chdir")},
	"doas":       {valued: flagSet("-u", "-C")},
	"env":        {valued: flagSet("-u", "--unset", "-C", "--chdir", "-S", "--split-string", "-P"), chdir: flagSet("-C", "--chdir")},
	"command":    {},
	"builtin":    {},
	"nohup":      {},
	"setsid":     {},
	"unbuffer":   {},
	"busybox":    {},
	"exec":       {valued: flagSet("-a")},
	"time":       {valued: flagSet("-f", "-o", "--format", "--output")},
	"nice":       {valued: flagSet("-n", "--adjustment")},
	"ionice":     {valued: flagSet("-c", "-n", "-p", "--class", "--classdata")},
	"timeout":    {valued: flagSet("-s", "-k", "--signal", "--kill-after"), positionals: 1},
	"gtimeout":   {valued: flagSet("-s", "-k", "--signal", "--kill-after"), positionals: 1},
	"chroot":     {valued: flagSet("--userspec", "--groups"), positionals: 1},
	"stdbuf":     {valued: flagSet("-i", "-o", "-e")},
	"caffeinate": {valued: flagSet("-t", "-w")},
	"xargs": {valued: flagSet("-I", "-i", "-n", "-P", "-L", "-l", "-d", "-E", "-e", "-s", "-a",
		"--arg-file", "--delimiter", "--max-args", "--max-procs", "--max-lines", "--replace")},
	"rtk": {},
}

var (
	shells = flagSet("sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "fish", "csh", "tcsh")
	// otherShells run code in a language the bash parser cannot read.
	otherShells  = flagSet("pwsh", "powershell", "nu", "xonsh", "elvish", "rc", "es")
	interpreters = flagSet("node", "perl", "ruby", "php", "osascript", "deno", "bun", "lua", "Rscript",
		"tclsh", "jshell", "irb", "groovy", "julia", "R", "swift", "kotlin", "scala", "elixir", "erl", "guile")
	awks = flagSet("awk", "gawk", "mawk", "nawk")
	// linkMakers can create symlinks whose targets come from content, or
	// move a relative link so it points somewhere new.
	linkMakers = flagSet("ln", "link", "unzip", "cpio", "7z", "7za", "ditto", "cp", "mv", "rsync", "install")
	// writers change files named in their arguments, so a script among
	// them cannot be read ahead of the call.
	writers = flagSet("cp", "mv", "tee", "install", "rsync", "ditto", "dd", "ln", "link", "touch", "patch",
		"curl", "wget", "truncate", "chmod", "unzip", "tar", "cpio")
	// varSetters are builtins that set variables from input or arguments.
	varSetters = flagSet("read", "printf", "mapfile", "readarray", "getopts", "unset", "export", "declare",
		"typeset", "local", "readonly", "vared", "zparseopts")
	// knownCommands start a nested command inside an unknown program's
	// arguments, as in "arch -arm64 sh -c ..." or "uv run git -C ...".
	knownCommands = flagSet("cd", "git", "gh", "make", "env", "sudo", "eval", "source", "xargs", "find")
)

// command handles one simple command after its words are expanded.
func (e *extractor) command(argv []word, st *shellState, depth int) {
	at := st.at()
	argv, at = e.unwrap(argv, at)
	if len(argv) == 0 {
		return
	}
	if !argv[0].exact() {
		e.opaque("command name is not static")
		e.genericArgs(argv, at)
		return
	}
	cmd := argv[0].val
	name := filepath.Base(cmd)
	args := argv[1:]
	if strings.Contains(cmd, "/") {
		e.runScript(argv[0], at, st, depth, false)
	}
	if writers[name] || (name == "sed" || name == "gsed") && hasFlag(args, "-i", "--in-place") {
		e.writes(args, at)
	}
	if varSetters[name] {
		st.forgetVars()
	}
	switch {
	case name == "cd":
		e.cd(args, st)
	case name == "pushd":
		e.pushd(args, st)
	case name == "popd":
		e.popd(args, st)
	case name == "git":
		e.git(args, at, st, depth)
	case name == "make" || name == "gmake":
		e.make(args, at)
	case name == "gh":
		e.gh(args, at)
	case shells[name]:
		e.shell(name, args, at, st, depth)
	case otherShells[name]:
		e.opaque("shell in a language Targets cannot read")
		e.genericArgs(args, at)
	case name == "source" || name == ".":
		if len(args) > 0 {
			e.runScript(args[0], at, st, depth, true)
			e.genericArgs(args[1:], at)
		}
	case name == "eval":
		e.opaque("eval")
		if allExact(args) {
			e.script(joinWords(args), st, depth+1)
		} else {
			e.genericArgs(args, at)
		}
	case name == "trap":
		// trap's first operand is code run later in this shell.
		code := args
		if len(code) > 0 && code[0].val == "--" {
			code = code[1:]
		}
		if len(code) > 1 && code[0].exact() && !strings.HasPrefix(code[0].val, "-") {
			e.script(code[0].val, st.fork(), depth+1)
		} else if len(code) > 1 && !code[0].exact() {
			e.opaque("trap code is not static")
		}
		e.genericArgs(args, at)
	case name == "alias" || name == "shopt" || name == "setopt" || name == "enable":
		e.opaque("changes how later commands run")
		e.genericArgs(args, at)
	case name == "set" && slices.ContainsFunc(args, func(a word) bool { return a.val == "-o" || a.val == "+o" }):
		e.opaque("changes how later commands run")
		e.genericArgs(args, at)
	case name == "ssh":
		e.ssh(args, st, depth)
	case name == "find":
		e.find(args, at, st, depth)
	case name == "su":
		e.su(args, at, st, depth)
	case name == "watch":
		e.watch(args, at, st, depth)
	case name == "flock":
		e.opaque("flock runs a command")
		e.genericArgs(args, at)
	case name == "apply_patch" || name == "applypatch":
		e.patch(joinWords(args), at)
	case linkMakers[name]:
		if name == "ln" || name == "link" || name == "cp" && hasFlag(args, "-s", "--symbolic-link") {
			e.opaque("creates links")
		}
		e.genericArgs(args, at)
		e.creates()
	case name == "tar" || name == "bsdtar":
		e.genericArgs(args, at)
		if len(args) > 0 && (hasFlag(args, "-x", "--extract", "--get") ||
			!strings.HasPrefix(args[0].val, "-") && strings.Contains(args[0].val, "x")) {
			e.creates()
		}
	case awks[name]:
		for _, a := range args {
			if strings.Contains(a.val, "system") || strings.Contains(a.val, "getline") || strings.Contains(a.val, "|") {
				e.opaque("awk program runs commands or reads files")
			}
		}
		e.genericArgs(args, at)
	case name == "sed" || name == "gsed":
		for _, a := range args {
			if sedExec(a.val) {
				e.opaque("sed program runs commands")
			}
		}
		e.genericArgs(args, at)
	case name == "sqlite3":
		for _, a := range args {
			if strings.Contains(a.val, ".shell") || strings.Contains(a.val, ".system") {
				e.opaque("sqlite3 runs commands")
			}
		}
		e.genericArgs(args, at)
	case name == "base64" && hasFlag(args, "-d", "-D", "--decode"):
		e.opaque("base64 decoding")
		e.genericArgs(args, at)
	case name == "xxd" && hasFlag(args, "-r", "--revert"):
		e.opaque("hex decoding")
		e.genericArgs(args, at)
	case interpreters[name] || strings.HasPrefix(name, "python"):
		e.interpreter(args, at)
	default:
		e.nested(args, at, st, depth)
		e.genericArgs(args, at)
	}
}

// sedECommand matches sed's e command (GNU sed), which runs a command.
var sedECommand = regexp.MustCompile(`(?:^|[;\n{}])\s*(?:[0-9$]+(?:,[0-9$]+)?)?\s*e(?:\s|;|$)`)

// sedExec reports whether a sed program runs a command: the e command, or
// s/// with the e flag.
func sedExec(prog string) bool {
	if sedECommand.MatchString(prog) {
		return true
	}
	for i := 0; i+1 < len(prog); i++ {
		if prog[i] != 's' || (i > 0 && isWordByte(prog[i-1])) {
			continue
		}
		d := prog[i+1]
		if isWordByte(d) || d == '\\' || d == '\n' || d == ' ' {
			continue
		}
		j, parts := i+2, 0
		for j < len(prog) && parts < 2 {
			switch prog[j] {
			case '\\':
				j++
			case d:
				parts++
			}
			j++
		}
		for parts == 2 && j < len(prog) && isWordByte(prog[j]) {
			if prog[j] == 'e' {
				return true
			}
			j++
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// writes marks every path argument of a command that writes files, so a
// script it may have written is not read ahead of the call.
func (e *extractor) writes(args []word, at where) {
	for _, a := range args {
		v := a.val
		if _, val, ok := strings.Cut(v, "="); ok && strings.HasPrefix(v, "of=") {
			v = val
		} else if strings.HasPrefix(v, "-") {
			continue
		}
		if a.exact() {
			for _, p := range e.absAll(at, v) {
				e.written[e.fileKey(p)] = true
			}
		}
	}
}

// nested walks what an unknown program may run: a shell, git or other
// known command among its arguments ("arch -arm64 sh -c ...", "uv run
// git ..."), and any argument that reads as a shell command line ("ssh
// host 'cd x; ...'", "tmux new 'cmd'"). Both are guesses (the word may
// be data), so what they cannot read does not make the call opaque.
func (e *extractor) nested(args []word, at where, st *shellState, depth int) {
	if depth >= maxShellDepth {
		return
	}
	e.spec++
	defer func() { e.spec-- }()
	for i, a := range args {
		name := filepath.Base(a.val)
		_, wrap := wrappers[name]
		if a.exact() && (shells[name] || knownCommands[name] || interpreters[name] || wrap) {
			sub := st.fork()
			sub.cwds, sub.unknown = at.dirs, at.unknown
			e.command(args[i:], sub, depth+1)
			break
		}
	}
	for _, a := range args {
		if !a.exact() || !strings.ContainsAny(a.val, " \t\n;&|") || len(a.val) > maxScript {
			continue
		}
		sub := st.fork()
		sub.cwds, sub.unknown = at.dirs, at.unknown
		e.script(a.val, sub, depth+1)
	}
}

// unwrap strips wrappers such as sudo, env and timeout, returning the
// command they run and the directories it runs in.
func (e *extractor) unwrap(argv []word, at where) ([]word, where) {
	for len(argv) > 0 && argv[0].exact() {
		name := filepath.Base(argv[0].val)
		w, ok := wrappers[name]
		if !ok {
			return argv, at
		}
		if name == "xargs" {
			e.opaque("xargs takes arguments from stdin")
		}
		i := 1
		for i < len(argv) {
			a := argv[i].val
			if a == "--" {
				i++
				break
			}
			if name == "env" && !strings.HasPrefix(a, "-") && strings.Contains(a, "=") {
				k, v, _ := strings.Cut(a, "=")
				vw := word{val: v, partial: argv[i].partial, glob: argv[i].glob, lit: argv[i].lit}
				e.pathArg(vw, at)
				e.specialVar(k, []word{vw}, at)
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") || a == "-" {
				break
			}
			flag, val, hasVal := strings.Cut(a, "=")
			if !w.valued[flag] && !strings.HasPrefix(a, "--") && len(a) > 2 && w.valued[a[:2]] {
				// An attached value: -D/dir, -C/dir.
				flag, val, hasVal = a[:2], a[2:], true
			}
			i++
			if w.valued[flag] && !hasVal && i < len(argv) {
				val, hasVal = argv[i].val, true
				if !argv[i].exact() {
					e.opaque("wrapper option is not static")
				}
				i++
			}
			if name == "env" && (flag == "-S" || flag == "--split-string") {
				e.opaque("env -S splits a string into a command")
			}
			if w.chdir[flag] && hasVal {
				at = e.moveAll(at, val)
				for _, d := range at.dirs {
					e.add(kindCd, d)
				}
			} else if hasVal && w.valued[flag] {
				e.pathArg(word{val: val}, at)
			}
		}
		for n := 0; n < w.positionals && i < len(argv); n++ {
			e.pathArg(argv[i], at)
			i++
		}
		if name == "rtk" && i < len(argv) && argv[i].val == "proxy" {
			i++
		}
		argv = argv[i:]
	}
	return argv, at
}

// genericArgs records every argument as a path relative to the command's
// directory: operands in full, the value of --opt=value, and the value of
// -Xvalue when it looks like a path. Paths written inside an argument are
// kept too. Words that are not paths resolve under the command's
// directory, which the caller's own cwd already covers.
func (e *extractor) genericArgs(args []word, at where) {
	flags := true
	for _, a := range args {
		v := a.val
		switch {
		case flags && v == "--":
			flags = false
		case flags && strings.HasPrefix(v, "--"):
			e.embedded(v, at, true, false)
			if _, val, ok := strings.Cut(v, "="); ok {
				e.pathArg(word{val: val, partial: a.partial, glob: a.glob, lit: a.lit}, at)
			}
		case flags && strings.HasPrefix(v, "-") && len(v) > 1:
			e.embedded(v, at, true, false)
			if val := v[2:]; strings.ContainsAny(val, "/~") || strings.HasPrefix(val, ".") {
				e.pathArg(word{val: val, partial: a.partial, glob: a.glob, lit: a.lit}, at)
			}
		default:
			e.pathArg(a, at)
		}
	}
}

// cd moves the shell. zsh reads "cd old new" as a substitution in the
// cwd, which bash does not have, so more than one operand is opaque.
func (e *extractor) cd(args []word, st *shellState) {
	physical := false
	var ops []word
	for _, a := range args {
		if len(ops) == 0 && a.exact() && strings.HasPrefix(a.val, "-") && a.val != "-" {
			physical = physical || strings.Contains(a.val, "P")
			continue
		}
		ops = append(ops, a)
	}
	switch {
	case len(ops) > 1:
		e.opaque("cd with more than one operand")
		for _, o := range ops {
			e.pathArg(o, st.at())
		}
		st.unknown = true
	case len(ops) == 0:
		e.chdir(st, []string{st.env["HOME"]}, physical)
	case ops[0].exact() && ops[0].val == "-":
		if len(st.oldpwd) == 0 {
			e.opaque("cd - to a directory set before this call")
			st.unknown = true
			return
		}
		e.chdir(st, st.oldpwd, physical)
	case !ops[0].exact():
		e.opaque("cd to a directory that is not static")
		e.pathArg(ops[0], st.at())
		st.unknown = true
	default:
		if !strings.HasPrefix(ops[0].val, "/") && st.unknown {
			e.opaque("relative path after a cd that may have run any number of times")
		}
		e.chdir(st, e.absAll(st.at(), ops[0].val), physical)
	}
}

func (e *extractor) chdir(st *shellState, dirs []string, physical bool) {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		if !filepath.IsAbs(d) {
			// An empty HOME or OLDPWD: the shell's real directory is unknown.
			e.opaque("cd to a directory that is not static")
			st.unknown = true
			return
		}
		if physical {
			out[i] = e.res.resolve(d).path
		} else {
			out[i] = filepath.Clean(d)
		}
		e.add(kindCd, out[i])
	}
	st.move(out)
}

func (e *extractor) pushd(args []word, st *shellState) {
	switch {
	case len(args) == 0:
		if len(st.dirs) == 0 {
			e.opaque("pushd to a directory set before this call")
			st.unknown = true
			return
		}
		top := st.dirs[len(st.dirs)-1]
		st.dirs[len(st.dirs)-1] = st.cwds
		e.chdir(st, top, false)
	case len(args) > 1 || !args[0].exact():
		e.opaque("pushd to a directory that is not static")
		for _, a := range args {
			e.pathArg(a, st.at())
		}
		st.unknown = true
	case strings.HasPrefix(args[0].val, "+") || strings.HasPrefix(args[0].val, "-"):
		e.opaque("pushd rotates the directory stack")
		st.unknown = true
	default:
		prev := st.cwds
		e.chdir(st, e.absAll(st.at(), args[0].val), false)
		st.dirs = append(st.dirs, prev)
	}
}

func (e *extractor) popd(args []word, st *shellState) {
	if len(args) > 0 || len(st.dirs) == 0 {
		e.opaque("popd to a directory set before this call")
		st.unknown = true
		return
	}
	top := st.dirs[len(st.dirs)-1]
	st.dirs = st.dirs[:len(st.dirs)-1]
	e.chdir(st, top, false)
}

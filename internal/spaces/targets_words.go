package spaces

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// maxBraces bounds the brace groups a word may hold before it is left
// unexpanded: each group can double the number of fields.
const maxBraces = 8

// userHomeSentinel stands in for ~user so expand never looks a user up.
const userHomeSentinel = "\x00~user"

// word is one field of an expanded shell word. partial means only val's
// prefix is known (a variable or substitution follows it); glob means val
// holds unquoted glob characters. Either way the word may name any path
// under its prefix. lit is all the literal text of a partial word.
type word struct {
	val     string
	partial bool
	glob    bool
	lit     string
}

func (w word) exact() bool { return !w.partial && !w.glob }

// words expands one shell word into its fields: quotes removed, braces
// and ~ expanded, and variables the line set filled in. A word whose value
// cannot be known keeps its literal prefix as a partial field and flags
// the call opaque. Command and process substitutions inside it are walked
// as commands in a subshell.
func (e *extractor) words(w *syntax.Word, st *shellState, depth int) []word {
	if w == nil {
		return nil
	}
	known, glob := e.inspect(w, st, depth)
	if known && countBraces(w) > maxBraces {
		e.opaque("too many brace expansions")
		known = false
	}
	if known {
		cfg := &expand.Config{Env: mapEnv(st.env), NoUnset: true, CmdSubst: func(out io.Writer, cs *syntax.CmdSubst) error {
			body, ok := catHeredoc(cs)
			if !ok {
				return errNotStatic
			}
			_, err := io.WriteString(out, body)
			return err
		}}
		fields, err := expand.Fields(cfg, w)
		if err == nil {
			out := make([]word, 0, len(fields))
			for _, f := range fields {
				if i := strings.Index(f, userHomeSentinel); i >= 0 {
					e.opaque("~user path")
					out = append(out, word{val: f[:i], partial: true, lit: f[:i]})
					continue
				}
				out = append(out, word{val: f, glob: glob})
			}
			return out
		}
		e.opaque("a word could not be expanded")
	}
	return []word{{val: literalPrefix(w), partial: true, lit: litText(w)}}
}

var errNotStatic = errors.New("not static")

// catHeredoc reports whether cs is "cat <<'EOF' ... EOF" with a quoted
// delimiter, whose output is the body exactly, and returns the body.
func catHeredoc(cs *syntax.CmdSubst) (string, bool) {
	if len(cs.Stmts) != 1 {
		return "", false
	}
	s := cs.Stmts[0]
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) != 1 || call.Args[0].Lit() != "cat" ||
		len(s.Redirs) != 1 || s.Negated || s.Background {
		return "", false
	}
	r := s.Redirs[0]
	if (r.Op != syntax.Hdoc && r.Op != syntax.DashHdoc) || r.Hdoc == nil || r.Word.Lit() != "" {
		// A delimiter that is a plain literal leaves the body expanded.
		return "", false
	}
	body := r.Hdoc.Lit()
	if body == "" && len(r.Hdoc.Parts) > 0 {
		return "", false
	}
	return strings.TrimSuffix(body, "\n"), true
}

func countBraces(w *syntax.Word) int {
	n := 0
	for _, p := range w.Parts {
		if lit, ok := p.(*syntax.Lit); ok {
			n += strings.Count(lit.Value, "{")
		}
	}
	return n
}

// inspect reports whether w can be expanded statically and whether it holds
// unquoted glob characters. It walks substitutions as it finds them.
func (e *extractor) inspect(w *syntax.Word, st *shellState, depth int) (known, glob bool) {
	known = true
	size := 0
	if lit, ok := w.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
		user, _, _ := strings.Cut(lit.Value[1:], "/")
		if user != "" {
			e.opaque("~user path")
			known = false
		}
	}
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst:
			sub := st.fork()
			if _, ok := catHeredoc(n); ok {
				// "$(cat <<'EOF' ... EOF)" is literal text, as in a
				// commit message; its body is still searched.
				for _, s := range n.Stmts {
					e.stmt(s, sub, depth)
				}
				return false
			}
			e.opaque("command substitution")
			known = false
			for _, s := range n.Stmts {
				e.stmt(s, sub, depth)
			}
			return false
		case *syntax.ProcSubst:
			e.opaque("process substitution")
			known = false
			sub := st.fork()
			for _, s := range n.Stmts {
				e.stmt(s, sub, depth)
			}
			return false
		case *syntax.ArithmExp, *syntax.ExtGlob:
			e.opaque("arithmetic or extended glob")
			known = false
			return false
		case *syntax.ParamExp:
			if n.Param == nil || !simpleParam(n) {
				e.opaque("variable expansion")
				known = false
			} else if v, ok := st.env[n.Param.Value]; !ok {
				e.opaque("variable expansion")
				known = false
			} else if size += len(v); size > maxVar {
				e.forceOpaque("word too long to expand")
				known = false
			}
			return false
		case *syntax.SglQuoted:
			return false
		}
		return true
	})
	for i, p := range w.Parts[:len(w.Parts)-1] {
		// zsh reads $d[1,2] as a subscript, which bash does not have.
		if pe, ok := p.(*syntax.ParamExp); ok && pe.Short {
			if lit, ok := w.Parts[i+1].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "[") {
				e.opaque("zsh subscript")
				known = false
			}
		}
	}
	for _, p := range w.Parts {
		// Only unquoted glob characters glob, including those an
		// unquoted variable expands to.
		switch p := p.(type) {
		case *syntax.Lit:
			glob = glob || hasGlob(p.Value)
		case *syntax.ParamExp:
			if p.Param != nil {
				glob = glob || hasGlob(st.env[p.Param.Value])
			}
		}
	}
	return known, glob
}

// hasGlob reports glob characters. A lone '[' is the test command, not a
// bracket expression.
func hasGlob(s string) bool {
	if strings.ContainsAny(s, "*?") {
		return true
	}
	i := strings.IndexByte(s, '[')
	return i >= 0 && strings.IndexByte(s[i+1:], ']') >= 0
}

func simpleParam(p *syntax.ParamExp) bool {
	return !p.Excl && !p.Length && !p.Width && !p.IsSet && p.Flags == nil &&
		p.NestedParam == nil && p.Index == nil && p.Slice == nil &&
		p.Repl == nil && p.Names == 0 && p.Exp == nil && len(p.Modifiers) == 0
}

// literalPrefix returns the part of w before its first part whose value
// is not literal.
func literalPrefix(w *syntax.Word) string {
	var b strings.Builder
	var walk func(parts []syntax.WordPart) bool
	walk = func(parts []syntax.WordPart) bool {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				if b.Len() == 0 && strings.HasPrefix(p.Value, "~") {
					return false
				}
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				if !walk(p.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	walk(w.Parts)
	return b.String()
}

// litText returns all literal text in w, with a space where a part is not
// literal, so paths written anywhere in it can be found.
func litText(w *syntax.Word) string {
	var b strings.Builder
	var walk func(parts []syntax.WordPart)
	walk = func(parts []syntax.WordPart) {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				walk(p.Parts)
			default:
				b.WriteByte(' ')
			}
		}
	}
	walk(w.Parts)
	return b.String()
}

// mapEnv exposes the variables a line set, plus HOME and PWD, to expand.
// Every other variable is unset; words using one are never expanded. ~user
// gets a sentinel, so expand never looks a user up.
type mapEnv map[string]string

func (m mapEnv) Get(name string) expand.Variable {
	if strings.HasPrefix(name, "HOME ") {
		return expand.Variable{Set: true, Kind: expand.String, Str: userHomeSentinel}
	}
	v, ok := m[name]
	if !ok {
		return expand.Variable{}
	}
	return expand.Variable{Set: true, Kind: expand.String, Str: v}
}

func (m mapEnv) Each(fn func(string, expand.Variable) bool) {
	for k := range m {
		if !fn(k, m.Get(k)) {
			return
		}
	}
}

// pathArg records a word as a path in every directory the command may run
// in, plus any path written inside it. A partial or glob word records the
// directory it may reach anything under.
func (e *extractor) pathArg(w word, at where) {
	text := w.val
	if w.partial {
		text = w.lit
	}
	e.embedded(text, at, true, false)
	if w.val == "" {
		return
	}
	rel := !strings.HasPrefix(w.val, "/") && w.val != "~" && !strings.HasPrefix(w.val, "~/")
	if rel && at.unknown {
		e.opaque("relative path after a cd that may have run any number of times")
	}
	for _, p := range e.absAll(at, w.val) {
		if w.exact() {
			e.add(kindPath, p)
		} else {
			e.add(kindPrefix, globPrefix(p))
		}
	}
}

// embeddedToken finds path-like tokens inside a longer string: a script
// passed as an argument, a "-v src:dst" mount, a here-document.
var embeddedToken = regexp.MustCompile(`[^\s"'` + "`" + `;|&<>(){}$,:=]+`)

var fileURI = regexp.MustCompile(`file://(/[^\s"'<>]*)`)

// embedded records paths written inside s: absolute and ~ paths always,
// and, when rel is set, relative tokens holding a '/'. A token equal to
// the whole of s is kept only when whole is set; otherwise the caller
// records it.
func (e *extractor) embedded(s string, at where, rel, whole bool) {
	if s == "" {
		return
	}
	if len(s) > maxScan {
		e.forceOpaque("text too long to search for paths")
		s = s[:maxScan]
	}
	e.githubURLs(s)
	e.githubOrgURLs(s)
	// Paths found inside text are guesses: they add targets, but cannot
	// make the call opaque on their own.
	e.spec++
	defer func() { e.spec-- }()
	for _, m := range fileURI.FindAllStringSubmatch(s, -1) {
		e.add(kindPath, m[1])
	}
	if !strings.ContainsAny(s, "/~") {
		return
	}
	for i, t := range embeddedToken.FindAllString(s, -1) {
		if i%256 == 0 && time.Now().After(e.deadline) {
			e.forceOpaque("analysis took too long")
			return
		}
		if t == s && !whole {
			continue
		}
		// "x:///a/b" leaves "///a/b": read it as the path it names.
		if strings.HasPrefix(t, "//") {
			t = "/" + strings.TrimLeft(t, "/")
		}
		switch {
		case t == "~" || strings.HasPrefix(t, "~/") || strings.HasPrefix(t, "/"):
			e.add(kindPath, e.abs("/", t))
		case rel && strings.Contains(t, "/") && !strings.HasPrefix(t, "~"):
			if at.unknown {
				e.opaque("relative path after a cd that may have run any number of times")
			}
			for _, p := range e.absAll(at, t) {
				e.add(kindPath, p)
			}
		}
	}
}

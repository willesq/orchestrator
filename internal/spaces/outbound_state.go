package spaces

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// What one command of a call changes for the commands after it, and the
// shell forms the outbound walk (SP-044) reads beyond plain words.

// braceWords are a word's brace expansions as the shell makes them
// (fal{,}con is falcon), for matching terms; none without braces.
func braceWords(word *syntax.Word) []string {
	w := &syntax.Word{Parts: append([]syntax.WordPart(nil), word.Parts...)}
	if !syntax.SplitBraces(w) {
		return nil
	}
	var out []string
	for i, e := range expand.Braces(w) {
		if i >= 64 {
			break
		}
		if v, ok := staticWord(e); ok {
			out = append(out, v)
		}
	}
	return out
}

// hasExtGlob reports whether a word holds an extended glob, which zsh may
// read as a glob qualifier that runs code.
func hasExtGlob(word *syntax.Word) bool {
	found := false
	syntax.Walk(word, func(n syntax.Node) bool {
		if g, ok := n.(*syntax.ExtGlob); ok && g.Pattern != nil && mentionsGit([]string{g.Pattern.Value}) {
			found = true
		}
		return !found
	})
	return found
}

// sourced walks what source or . reads from stdin: a here-string or a
// literal here-document. Any other file it reads is not followed; stdin
// from elsewhere is unknown.
func (w *outboundWalker) sourced(s *syntax.Stmt, args []string, dir string, stdin bool) {
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "/dev/stdin", "/dev/fd/0", "-":
	default:
		if mentionsGit(args) {
			w.unknown = true
		}
		return
	}
	w.nested(s, nil, dir, stdin, false)
}

// gitConfigPaths are files whose change changes where git sends or what
// it runs, below the home.
var gitConfigPaths = []string{".gitconfig", ".config/git"}

// writes records the files a command writes: output redirections, and the
// operands of cp, mv, tee, sed -i, ln, install, rsync, dd of=, truncate
// and patch, so a later commit, push or file-reading action that may see
// them is unknown (SP-044 review).
func (w *outboundWalker) writes(s *syntax.Stmt, name string, args []string, dir string) {
	add := func(p string) {
		if p == "" || dir == "" && !filepath.IsAbs(p) {
			w.written = append(w.written, "")
			return
		}
		w.written = append(w.written, w.join(dir, p))
	}
	switch name {
	case "cp", "mv", "ln", "install", "rsync", "ditto", "tee", "truncate", "patch", "sed", "gsed", "perl", "dd":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				add(v)
			} else if !strings.HasPrefix(a, "-") {
				add(a)
			}
		}
	}
}

// redirWrites records the files a statement's output redirections write.
func (w *outboundWalker) redirWrites(s *syntax.Stmt, dir string) {
	for _, r := range s.Redirs {
		switch r.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.ClbOut, syntax.RdrInOut:
			v, ok := w.word(r.Word, dir)
			if !ok || dir == "" && !filepath.IsAbs(v) {
				w.written = append(w.written, "")
				continue
			}
			if v == "/dev/null" || v == "/dev/stderr" || v == "/dev/stdout" {
				continue
			}
			w.written = append(w.written, w.join(dir, v))
		}
	}
}

// touched reports whether an earlier command of the call wrote a file the
// action may read: one it sends, a file below its checkout (for a commit
// or push), git's config, or a file whose name is not known.
func (w *outboundWalker) touched(act *Outbound) bool {
	for _, p := range w.written {
		if p == "" {
			return true
		}
		if strings.Contains(p, "/.git/") || strings.HasSuffix(p, "/.git") {
			return true
		}
		for _, c := range gitConfigPaths {
			if w.home != "" && under(guardKey(p), guardKey(filepath.Join(w.home, c))) {
				return true
			}
		}
		for _, f := range act.Files {
			if guardKey(f) == guardKey(p) {
				return true
			}
		}
		if (act.Push || act.Staged || act.Pending) && act.Dir != "" && under(guardKey(p), guardKey(act.Dir)) {
			return true
		}
	}
	return false
}

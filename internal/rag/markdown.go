package rag

import (
	"regexp"
	"strings"
	"unicode"
)

// chunk is one piece of a document with the heading path it sits under.
type chunk struct {
	text    string
	heading string // "Install › Homebrew"; empty outside any heading
}

var (
	atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t#]*$`)
	fenceOpen  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
)

// maxTitle caps a heading title. A heading is repeated in every chunk under
// it, so an unbounded one would multiply a hostile document's size.
const maxTitle = 120

// pathSep joins heading paths. Titles cannot contain it (see cleanTitle), so a
// path cannot be faked by a title that looks like several.
const pathSep = " › "

// fence tracks an open fenced code block.
type fence struct {
	char byte
	n    int
}

// openFence reports the fence line opens, if any. A backtick fence's info
// string may not itself contain a backtick (CommonMark).
func openFence(line string) (fence, bool) {
	m := fenceOpen.FindStringSubmatch(line)
	if m == nil || (m[1][0] == '`' && strings.Contains(m[2], "`")) {
		return fence{}, false
	}
	return fence{char: m[1][0], n: len(m[1])}, true
}

// closes reports whether line closes f: the same character, at least as many,
// and nothing after but spaces.
func (f fence) closes(line string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	n := 0
	for n < len(t) && t[n] == f.char {
		n++
	}
	return n >= f.n && strings.TrimSpace(t[n:]) == ""
}

// cleanTitle makes a heading title safe to repeat: control and format
// characters dropped, the path separator replaced, length capped.
func cleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		case r == '›' || r == '>':
			return '-'
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxTitle {
		s = string(r[:maxTitle]) + "…"
	}
	return s
}

// isMarkdown reports whether text has at least one ATX heading outside a code
// fence: enough structure for section-aware chunking to beat word windows.
func isMarkdown(text string) bool {
	for _, s := range sections(text) {
		if s.level > 0 {
			return true
		}
	}
	return false
}

type section struct {
	level int      // heading level; 0 for text before the first heading
	path  []string // ancestor headings, outermost first, excluding this one
	title string
	lines []string // the heading line (normalized) and body, newlines preserved
}

func (s section) full() []string {
	if s.level == 0 {
		return nil
	}
	return append(append([]string{}, s.path...), s.title)
}

// sections splits markdown into heading-delimited sections. Lines inside a
// fenced code block are never read as headings.
func sections(text string) []section {
	var out []section
	cur := section{}
	type entry struct {
		level int
		title string
	}
	var stack []entry
	var open *fence
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if open != nil {
			cur.lines = append(cur.lines, line)
			if open.closes(line) {
				open = nil
			}
			continue
		}
		if f, ok := openFence(line); ok {
			open = &f
			cur.lines = append(cur.lines, line)
			continue
		}
		m := atxHeading.FindStringSubmatch(line)
		if m == nil {
			cur.lines = append(cur.lines, line)
			continue
		}
		if cur.level > 0 || strings.TrimSpace(strings.Join(cur.lines, "")) != "" {
			out = append(out, cur)
		}
		level, title := len(m[1]), cleanTitle(m[2])
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		path := make([]string, len(stack))
		for i, s := range stack {
			path[i] = s.title
		}
		stack = append(stack, entry{level, title})
		// The heading line is rewritten from the cleaned title, so what is
		// repeated is bounded too.
		cur = section{level: level, path: path, title: title, lines: []string{strings.Repeat("#", level) + " " + title}}
	}
	if cur.level > 0 || strings.TrimSpace(strings.Join(cur.lines, "")) != "" {
		out = append(out, cur)
	}
	return out
}

func words(s string) int { return len(strings.Fields(s)) }

// tidy trims trailing spaces and collapses runs of blank lines, leaving fenced
// code exactly as written.
func tidy(lines []string) string {
	var out []string
	blank := 0
	var open *fence
	for _, l := range lines {
		if open != nil {
			out = append(out, l)
			if open.closes(l) {
				open = nil
			}
			continue
		}
		if f, ok := openFence(l); ok {
			open = &f
			out = append(out, strings.TrimRight(l, " \t"))
			blank = 0
			continue
		}
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// blocks splits a section body into paragraphs, keeping each fenced code
// block whole.
func blocks(lines []string) [][]string {
	var out [][]string
	var cur []string
	var open *fence
	flush := func() {
		if strings.TrimSpace(strings.Join(cur, "")) != "" {
			out = append(out, cur)
		}
		cur = nil
	}
	for _, line := range lines {
		if open != nil {
			cur = append(cur, line)
			if open.closes(line) {
				open = nil
			}
			continue
		}
		if f, ok := openFence(line); ok {
			open = &f
			cur = append(cur, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// markdownChunks packs sections into chunks of about size words. Sections
// that fit together share a chunk when they sit in one subtree; a section too
// large for one is split at paragraph boundaries, and a single paragraph too
// large for one falls back to overlapping word windows. A chunk starts with the
// path of headings above its first section, counted against size, and records
// the deepest path all of its sections share.
func markdownChunks(text string, size, overlap int) []chunk {
	if size <= 0 {
		size = 400
	}
	var out []chunk
	var buf []string
	var first []string // full path of the chunk's first section
	var shared []string
	bufWords := 0
	inBuf := false
	flush := func() {
		body := tidy(buf)
		if strings.TrimSpace(body) != "" {
			out = append(out, chunk{text: withContext(parent(first), body), heading: strings.Join(shared, pathSep)})
		}
		buf, bufWords, inBuf = nil, 0, false
	}
	for _, sec := range sections(text) {
		full := sec.full()
		n := words(strings.Join(sec.lines, " "))
		if inBuf && !joins(first, sec) || inBuf && bufWords+n > budget(size, first) {
			flush()
		}
		if n <= budget(size, full) {
			if !inBuf {
				first, shared, inBuf = full, full, true
			} else {
				shared = commonPrefix(shared, full)
			}
			buf = append(append(buf, sec.lines...), "")
			bufWords += n
			continue
		}
		flush()
		for _, piece := range splitSection(sec.lines, budget(size, full), overlap) {
			out = append(out, chunk{text: withContext(parent(full), piece), heading: strings.Join(full, pathSep)})
		}
	}
	flush()
	return out
}

// joins reports whether sec may share a chunk whose first section has path
// first: it must sit in the same subtree. Text before the first heading stands
// alone, and a top-level section takes only its own descendants, so a chunk
// never spans unrelated parts of a document.
func joins(first []string, sec section) bool {
	switch len(first) {
	case 0:
		return false
	case 1:
		return hasPrefix(sec.path, first)
	}
	return hasPrefix(sec.path, parent(first))
}

// budget is the words a chunk body may use once its context line is counted.
func budget(size int, full []string) int {
	b := size - words(strings.Join(parent(full), " "))
	return max(b, size/2, 1)
}

// parent is a heading path minus its last heading.
func parent(path []string) []string {
	if len(path) == 0 {
		return nil
	}
	return path[:len(path)-1]
}

func hasPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

func commonPrefix(a, b []string) []string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// withContext prefixes body with the headings above it, marked so it cannot be
// mistaken for the document's own text.
func withContext(path []string, body string) string {
	if len(path) == 0 {
		return body
	}
	return "[section: " + strings.Join(path, pathSep) + "]\n\n" + body
}

// splitSection splits an oversized section at paragraph boundaries. The
// heading line repeats on every piece so each stays self-describing.
func splitSection(lines []string, size, overlap int) []string {
	heading := ""
	body := lines
	if len(lines) > 0 && atxHeading.MatchString(lines[0]) {
		heading, body = lines[0], lines[1:]
	}
	var out []string
	var cur []string
	curWords := 0
	emit := func() {
		if len(cur) == 0 {
			return
		}
		text := tidy(cur)
		if heading != "" {
			text = heading + "\n\n" + text
		}
		out = append(out, text)
		cur, curWords = nil, 0
	}
	for _, b := range blocks(body) {
		n := words(strings.Join(b, " "))
		if n > size {
			emit()
			for _, w := range chunkText(strings.Join(b, "\n"), size, overlap) {
				cur = []string{w}
				emit()
			}
			continue
		}
		if curWords+n > size {
			emit()
		}
		cur = append(append(cur, b...), "")
		curWords += n
	}
	emit()
	if len(out) == 0 {
		// Nothing but a heading too long for one chunk: keep its words.
		return chunkText(strings.Join(lines, "\n"), size, overlap)
	}
	return out
}

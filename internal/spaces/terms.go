package spaces

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Outbound terms (spaces plan SP-044). Each space may name a terms file:
// words and patterns that belong to that space and must not leave for a
// destination outside it (a commit, a push, a PR comment on another org's
// repo). A terms file holds one term per line: a literal term, or
// re:<regex>. Matching is case-insensitive and on word boundaries. Blank
// lines and lines starting with '#' are skipped.
//
// INV-07: a term never appears in logs, refusal messages, error messages,
// the UI or indexed content. A hit is named by the space, the line number
// in the terms file and a short hash of the line, never by its text; an
// error is named by its line number alone.

// Limits on a terms file, so a huge or hostile one cannot slow every
// outbound check.
const (
	maxTermsFile = 256 << 10
	maxTerms     = 4096
	maxTermLen   = 512
)

// TermHit names a term that matched, without its text.
type TermHit struct {
	Space string
	// Line is the term's line number in the space's terms file.
	Line int
	// Hash is the first 12 hex digits of the line's HMAC-SHA256, keyed
	// with the terms file's SHA-256.
	Hash string
}

// String names the hit as a refusal may: space, line and hash.
func (h TermHit) String() string {
	return fmt.Sprintf("space %q terms file line %d (hash %s)", h.Space, h.Line, h.Hash)
}

// term is one compiled term. re matches it with its word boundaries,
// captured as termb and terma.
type term struct {
	line int
	hash string
	re   *regexp.Regexp
}

// TermList is one space's terms.
type TermList struct {
	Space string
	terms []term
}

// Len is how many terms the list holds.
func (l TermList) Len() int { return len(l.terms) }

// termBoundary is what may stand on either side of a term: the start or
// end of the text, or a character that is not a letter, digit or '_'.
const (
	termBefore = `(?P<termb>^|[^\p{L}\p{N}_])`
	termAfter  = `(?P<terma>$|[^\p{L}\p{N}_])`
)

// ParseTerms reads a terms file's content for space. An error names the
// line it is about, never the term.
func ParseTerms(space string, data []byte) (TermList, error) {
	if len(data) > maxTermsFile {
		return TermList{}, fmt.Errorf("the terms file of space %q is over %d bytes", space, maxTermsFile)
	}
	if !utf8.Valid(data) {
		return TermList{}, fmt.Errorf("the terms file of space %q is not UTF-8", space)
	}
	out := TermList{Space: space}
	// Reason: the hash is keyed with the whole file, so a short term
	// cannot be found from its hash by trying words (SP-044 review); it
	// stays stable while the file is unchanged.
	key := sha256.Sum256(data)
	for i, raw := range strings.Split(string(data), "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > maxTermLen {
			return TermList{}, fmt.Errorf("the terms file of space %q: line %d is over %d bytes", space, n, maxTermLen)
		}
		if len(out.terms) >= maxTerms {
			return TermList{}, fmt.Errorf("the terms file of space %q has over %d terms", space, maxTerms)
		}
		pattern := regexp.QuoteMeta(line)
		if rest, ok := strings.CutPrefix(line, "re:"); ok {
			pattern = strings.TrimSpace(rest)
			if pattern == "" {
				return TermList{}, fmt.Errorf("the terms file of space %q: line %d has an empty pattern", space, n)
			}
			if _, err := regexp.Compile(pattern); err != nil {
				// Reason: the regexp error quotes the pattern, which is
				// the term itself.
				return TermList{}, fmt.Errorf("the terms file of space %q: line %d is not a valid regular expression", space, n)
			}
		}
		re, err := regexp.Compile(`(?i)` + termBefore + `(?:` + pattern + `)` + termAfter)
		if err != nil {
			return TermList{}, fmt.Errorf("the terms file of space %q: line %d is not a valid term", space, n)
		}
		if re.MatchString("") {
			return TermList{}, fmt.Errorf("the terms file of space %q: line %d matches empty text", space, n)
		}
		if countName(re, "termb") != 1 || countName(re, "terma") != 1 {
			// Reason: redaction keeps these groups; a pattern defining
			// its own would keep the term itself.
			return TermList{}, fmt.Errorf("the terms file of space %q: line %d uses a reserved group name", space, n)
		}
		out.terms = append(out.terms, term{line: n, hash: lineHash(key, line), re: re})
	}
	return out, nil
}

// countName counts re's groups named name.
func countName(re *regexp.Regexp, name string) int {
	n := 0
	for _, s := range re.SubexpNames() {
		if s == name {
			n++
		}
	}
	return n
}

// lineHash is the first 12 hex digits of HMAC-SHA256 of line, keyed with
// the terms file's own hash.
func lineHash(key [32]byte, line string) string {
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte(line))
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// ReadTerms reads and parses space's terms file at path.
func ReadTerms(space, path string) (TermList, error) {
	b, err := readSmall(path, maxTermsFile+1)
	if err != nil {
		return TermList{}, fmt.Errorf("the terms file of space %q cannot be read", space)
	}
	return ParseTerms(space, b)
}

// Match returns the first term of l found in text.
func (l TermList) Match(text string) (TermHit, bool) {
	for _, t := range l.terms {
		if t.re.MatchString(text) {
			return TermHit{Space: l.Space, Line: t.line, Hash: t.hash}, true
		}
	}
	return TermHit{}, false
}

// redactedTerm replaces a term in text kept for logs.
const redactedTerm = "[term]"

// maxRedactPasses bounds the passes Redact makes: a match consumes the
// boundary after it, so a term right after another needs one more pass.
const maxRedactPasses = 8

// Redact replaces every term of l in text with [term].
func (l TermList) Redact(text string) string {
	for _, t := range l.terms {
		for range maxRedactPasses {
			next := t.re.ReplaceAllString(text, "${termb}"+redactedTerm+"${terma}")
			if next == text {
				break
			}
			text = next
		}
	}
	return text
}

// RedactTerms replaces every term of every list in text.
func RedactTerms(text string, lists []TermList) string {
	for _, l := range lists {
		text = l.Redact(text)
	}
	return text
}

// SpaceTerms reads the terms file of every space of res that names one.
// A file that cannot be read or parsed is returned in errs, by space; the
// caller decides whether that fails closed.
func SpaceTerms(res *Resolved, home string) (lists []TermList, errs map[string]error) {
	if !res.Configured() {
		return nil, nil
	}
	env := Env{Home: home}
	for _, s := range res.Policy.Spaces {
		if s.TermsFile == "" {
			continue
		}
		path := env.expand(s.TermsFile)
		l, err := ReadTerms(s.Name, path)
		if err != nil {
			if errs == nil {
				errs = map[string]error{}
			}
			errs[s.Name] = err
			continue
		}
		lists = append(lists, l)
	}
	return lists, errs
}

// ReadApplied reads the applied policy from the orch.db at dbPath,
// read-only and verified as orchd verifies it at startup, for the hooks,
// which run whether or not orchd does. A missing store, or one that never
// applied a policy, has none (nil).
func ReadApplied(ctx context.Context, dbPath string, env Env) (*Resolved, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}).String()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	a, err := LoadApplied(ctx, conn, env)
	switch {
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return nil, nil
	case err != nil:
		return nil, err
	case a == nil:
		return nil, nil
	}
	return a.Resolved, nil
}

// TermsPaths are the terms files of res's spaces, by space, expanded as
// validation expands them.
func TermsPaths(res *Resolved, home string) map[string]string {
	if !res.Configured() {
		return nil
	}
	out := map[string]string{}
	env := Env{Home: home}
	for _, s := range res.Policy.Spaces {
		if s.TermsFile != "" {
			out[s.Name] = env.expand(s.TermsFile)
		}
	}
	return out
}

// RedactJSON replaces every term of lists in the string values of a JSON
// document, decoded first so a term after an escape (\n, \t) is found, and
// encoded again without HTML escaping. Text that is not JSON is redacted
// as it is.
func RedactJSON(doc []byte, lists []TermList) []byte {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return []byte(RedactTerms(string(doc), lists))
	}
	v = redactValue(v, lists)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(RedactTerms(string(doc), lists))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func redactValue(v any, lists []TermList) any {
	switch x := v.(type) {
	case string:
		return RedactTerms(x, lists)
	case []any:
		for i := range x {
			x[i] = redactValue(x[i], lists)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[RedactTerms(k, lists)] = redactValue(val, lists)
		}
		return out
	}
	return v
}

// RedactTargets replaces terms in every path and repo of t.
func RedactTargets(t ToolTargets, lists []TermList) ToolTargets {
	for _, l := range []*[]string{&t.Paths, &t.CdDest, &t.GitDirs, &t.GHRepos, &t.Prefixes, &t.OpaqueReasons} {
		for i := range *l {
			(*l)[i] = RedactTerms((*l)[i], lists)
		}
	}
	return t
}

// JSONText is the text of a tool input as a person reads it: its string
// values, decoded, one per line; the input itself when it is not JSON.
func JSONText(doc []byte) string {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return string(doc)
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			b.WriteString(x)
			b.WriteByte('\n')
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				b.WriteString(k)
				b.WriteByte('\n')
				walk(e)
			}
		}
	}
	walk(v)
	return b.String()
}

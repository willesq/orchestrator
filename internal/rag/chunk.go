package rag

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxChunkTokens is the most a chunk may hold. The embedding model's tokenizer
// truncates its input at 128 tokens, two of which are the special tokens added
// around every input; anything past that would never be embedded, so a chunk
// sized beyond it is silently searchable by only its start.
const maxChunkTokens = 126

// TokenCounter counts the model tokens in text, excluding the special tokens
// added around every input. ok is false when no tokenizer is loaded, and the
// chunker then estimates.
type TokenCounter interface {
	CountTokens(text string) (n int, ok bool)
}

// measure counts the tokens in one whitespace-separated word.
type measure func(word string) int

// tokens is the token count of s, summed word by word. The model's tokenizer
// splits on whitespace before anything else, so the sum matches counting s
// whole.
func tokens(m measure, s string) int {
	n := 0
	for _, w := range strings.Fields(s) {
		n += m(w)
	}
	return n
}

// runesPerToken floors every word's count at one token per this many
// characters. The model folds any word over 100 characters into a single
// unknown token, so a true count alone would let a megabyte "word" pass as
// one token; the floor keeps a chunk's bytes bounded too.
const runesPerToken = 20

// newMeasure counts with c when it can, else estimates, caching per word
// for one chunking pass.
func newMeasure(c TokenCounter) measure {
	cache := map[string]int{}
	return func(w string) int {
		if n, ok := cache[w]; ok {
			return n
		}
		n, ok := 0, false
		if c != nil {
			n, ok = c.CountTokens(w)
		}
		if !ok {
			n = approxTokens(w)
		}
		n = max(n, 1, (utf8.RuneCountInString(w)+runesPerToken-1)/runesPerToken)
		cache[w] = n
		return n
	}
}

// cleanText removes what the tokenizer would drop or stop at, so counts match
// what is embedded: invalid UTF-8, NUL (the tokenizer's C boundary ends a
// string there), and control and format characters other than tab and line
// breaks. Other Unicode spaces become a plain space.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r == utf8.RuneError:
			return -1
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// approxTokens estimates a word's WordPiece token count, erring high: each
// punctuation or symbol character is a token, a CJK character is a token, and
// a run of letters or digits splits into a piece per three characters. An
// overestimate only makes chunks smaller; an underestimate would let one run
// past what the model embeds.
func approxTokens(w string) int {
	n, run := 0, 0
	flush := func() {
		if run > 0 {
			n += 1 + (run-1)/3
			run = 0
		}
	}
	for _, r := range w {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			flush()
			n++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			run++
		default:
			flush()
			n++
		}
	}
	flush()
	return max(n, 1)
}

// chunkText splits text into overlapping windows of at most size tokens, with
// about overlap tokens shared between neighbours.
func chunkText(text string, size, overlap int, m measure) []string {
	if size <= 0 || size > maxChunkTokens {
		size = maxChunkTokens
	}
	// Overlap past a quarter of a window makes windows advance a word or two
	// at a time, multiplying the chunk count.
	overlap = min(max(overlap, 0), size/4)
	var fields []string
	for _, w := range strings.Fields(text) {
		fields = append(fields, splitWord(w, size, m)...)
	}
	if len(fields) == 0 {
		return nil
	}
	counts := make([]int, len(fields))
	for i, w := range fields {
		counts[i] = m(w)
	}
	var chunks []string
	for start := 0; start < len(fields); {
		end, used := start, 0
		for end < len(fields) && (end == start || used+counts[end] <= size) {
			used += counts[end]
			end++
		}
		chunks = append(chunks, strings.Join(fields[start:end], " "))
		if end == len(fields) {
			break
		}
		// Step back over about overlap tokens, but always move forward.
		next, back := end, 0
		for next-1 > start && back+counts[next-1] <= overlap {
			next--
			back += counts[next]
		}
		start = next
	}
	return chunks
}

// splitWord breaks a word too large for one chunk (a long rule, a table
// separator, a URL) into pieces that fit, so its tail is embedded rather than
// cut off by the model. A token covers at least one character, so a piece of
// size characters always fits; each cut grows from there by doubling, then
// narrows by binary search, keeping the cost near linear in the word's length.
func splitWord(w string, size int, m measure) []string {
	if m(w) <= size {
		return []string{w}
	}
	var out []string
	rs := []rune(w)
	for len(rs) > 0 {
		fits := func(n int) bool { return m(string(rs[:n])) <= size }
		lo := min(size, len(rs))
		hi := lo
		for hi < len(rs) && fits(min(2*hi, len(rs))) {
			hi = min(2*hi, len(rs))
		}
		lo, hi = hi, min(2*hi, len(rs))
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if fits(mid) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		out = append(out, string(rs[:lo]))
		rs = rs[lo:]
	}
	return out
}

// Package bm25 turns text into BM25-weighted sparse vectors for keyword
// search alongside the dense embeddings. It tokenizes for code as well as
// prose, so an exact identifier such as queue.worker.codex.enabled or
// ProcessActivity matches itself and its parts. The store applies inverse
// document frequency at query time (Qdrant's idf modifier), so a document
// vector carries only the term-frequency side of BM25 and a query vector
// weights each distinct term 1.
package bm25

import (
	"errors"
	"hash/fnv"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// TokenizerVersion identifies how text becomes term ids. Stored vectors are
// only comparable with queries tokenized the same way, so any change to
// tokenizing (the pattern, stopwords, limits, normalization) must bump it and
// re-index.
const TokenizerVersion = 1

// Params are BM25's tuning constants.
type Params struct {
	K1 float64 // term-frequency saturation (1.2 is the usual default)
	B  float64 // length normalization (0.75 is the usual default)
	// AvgLen is the average document length in terms. It should match the
	// corpus: chunks here are short, so the common default of 256 would
	// under-normalize them.
	AvgLen float64
}

// Validate rejects constants that would make weights NaN, infinite or
// negative.
func (p Params) Validate() error {
	finite := func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
	switch {
	case !finite(p.K1) || p.K1 <= 0:
		return errors.New("bm25: k1 must be positive")
	case !finite(p.B) || p.B < 0 || p.B > 1:
		return errors.New("bm25: b must be between 0 and 1")
	case !finite(p.AvgLen) || p.AvgLen <= 0:
		return errors.New("bm25: average length must be positive")
	}
	return nil
}

// DefaultParams suit this system's chunks: at most 110 model tokens is about
// 50 words, and this tokenizer yields about 1.09 terms per word of markdown
// and Go (measured 2026-10-14 across about 600k words of real repositories).
var DefaultParams = Params{K1: 1.2, B: 0.75, AvgLen: 55}

// Vector is a sparse vector: parallel, index-sorted term ids and weights.
type Vector struct {
	Indices []uint32  `json:"indices"`
	Values  []float32 `json:"values"`
}

// Limits that bound what one call may cost or store. Input is meant to be
// chunk-sized; these keep a hostile or mistaken caller from turning megabytes
// of text into gigabytes of allocations.
const (
	maxTerms    = 4096 // distinct terms kept in one vector
	maxTokens   = 8192 // tokens read from one text
	maxParts    = 64   // parts taken from one identifier
	maxTermByte = 64   // longest term indexed
)

// identifier matches a run of word characters, combining marks included,
// joined by the separators code uses inside names: dots, slashes, dashes and
// colons.
var identifier = regexp.MustCompile(`[\pL\pM\pN_]+(?:[./:\-][\pL\pM\pN_]+)*`)

// Tokens returns text's search terms, at most maxTokens of them. Text is
// NFC-normalized first, so composed and decomposed accents match. Each
// identifier yields itself, lowercased, and its parts: split at separators,
// underscores and camelCase boundaries. So "queue.worker.codex.enabled" also
// yields "queue", "worker", "codex", "enabled", and "ProcessActivity" yields
// "processactivity", "process", "activity". Scripts written without spaces
// (Han, kana, Hangul, Thai) yield overlapping character pairs, so a word
// inside a sentence still matches. Stopwords are dropped as parts but kept
// inside a whole identifier, and single characters are dropped.
func Tokens(text string) []string {
	var out []string
	add := func(t string) bool {
		out = append(out, t)
		return len(out) < maxTokens
	}
	for _, m := range identifier.FindAllString(norm.NFC.String(text), -1) {
		for _, seg := range scriptRuns(m) {
			if seg.unspaced {
				for _, bg := range bigrams(seg.text) {
					if !add(bg) {
						return out
					}
				}
				continue
			}
			parts := splitParts(seg.text)
			if len(seg.text) <= maxTermByte && (len(parts) > 1 || len(parts) == 1 && parts[0] != strings.ToLower(seg.text)) {
				if whole := strings.ToLower(seg.text); keep(whole) && !add(whole) {
					return out
				}
			}
			for _, p := range parts {
				if keep(p) && !stopwords[p] && !add(p) {
					return out
				}
			}
		}
	}
	return out
}

func keep(t string) bool { return len(t) <= maxTermByte && utf8.RuneCountInString(t) >= 2 }

// unspacedScript reports whether r belongs to a script written without spaces
// between words.
func unspacedScript(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Thai)
}

type run struct {
	text     string
	unspaced bool
}

// scriptRuns splits s into runs of unspaced-script characters and the rest.
func scriptRuns(s string) []run {
	var out []run
	start, cur := 0, false
	for i, r := range s {
		u := unspacedScript(r)
		if i > 0 && u != cur {
			out = append(out, run{s[start:i], cur})
			start = i
		}
		cur = u
	}
	if start < len(s) {
		out = append(out, run{s[start:], cur})
	}
	return out
}

// bigrams returns the overlapping two-character pieces of s (s itself when it
// is a single character, which keep then drops).
func bigrams(s string) []string {
	rs := []rune(s)
	if len(rs) < 2 {
		return []string{s}
	}
	out := make([]string, 0, len(rs)-1)
	for i := 0; i+1 < len(rs); i++ {
		out = append(out, string(rs[i:i+2]))
	}
	return out
}

// splitParts breaks an identifier at separators, underscores and camelCase
// boundaries, lowercasing each part.
func splitParts(s string) []string {
	var parts []string
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune("./:-_", r) }) {
		parts = append(parts, splitCamel(seg)...)
		if len(parts) >= maxParts {
			return parts[:maxParts]
		}
	}
	return parts
}

// splitCamel splits "ProcessActivity" into process, activity and
// "HTTPServer" into http, server; digits stay with the run before them.
func splitCamel(s string) []string {
	rs := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		boundary := unicode.IsLower(prev) && unicode.IsUpper(cur) ||
			unicode.IsUpper(prev) && unicode.IsUpper(cur) && unicode.IsLower(next)
		if boundary {
			parts = append(parts, strings.ToLower(string(rs[start:i])))
			start = i
		}
	}
	return append(parts, strings.ToLower(string(rs[start:])))
}

// TermID hashes a term to its sparse index. A 32-bit hash collides rarely at
// this vocabulary size (a few hundred thousand terms gives on the order of
// ten colliding pairs), and a collision only adds noise to one term's match.
func TermID(term string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(term))
	return h.Sum32()
}

// Document returns the BM25 term-frequency weights for a chunk-sized
// document: tf*(k1+1) / (tf + k1*(1 - b + b*len/avgLen)) per distinct term.
// Invalid constants fall back to DefaultParams. The length used for
// normalization is floored at a quarter of the average, so a tiny document
// cannot outweigh normal ones on its few terms.
func Document(text string, p Params) Vector {
	if p.Validate() != nil {
		p = DefaultParams
	}
	toks := Tokens(text)
	if len(toks) == 0 {
		return Vector{Indices: []uint32{}, Values: []float32{}}
	}
	tf := map[uint32]float64{}
	for _, t := range toks {
		tf[TermID(t)]++
	}
	length := math.Max(float64(len(toks)), p.AvgLen/4)
	norm := p.K1 * (1 - p.B + p.B*length/p.AvgLen)
	weights := make(map[uint32]float32, len(tf))
	for id, f := range tf {
		weights[id] = float32(f * (p.K1 + 1) / (f + norm))
	}
	return build(weights)
}

// Query returns a query's vector: each distinct term weighted 1, leaving the
// rest of the score to the store's IDF.
func Query(text string) Vector {
	weights := map[uint32]float32{}
	for _, t := range Tokens(text) {
		weights[TermID(t)] = 1
	}
	return build(weights)
}

// IsEmpty reports whether v has no terms; callers skip storing or searching
// an empty vector.
func (v Vector) IsEmpty() bool { return len(v.Indices) == 0 }

// build sorts by index and caps the term count, keeping the heaviest terms.
// It never returns nil slices, and drops any weight that is not a positive
// finite number, which the store would reject.
func build(weights map[uint32]float32) Vector {
	ids := make([]uint32, 0, len(weights))
	for id, w := range weights {
		if f := float64(w); f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f) {
			ids = append(ids, id)
		}
	}
	if len(ids) > maxTerms {
		sort.Slice(ids, func(i, j int) bool {
			if weights[ids[i]] != weights[ids[j]] {
				return weights[ids[i]] > weights[ids[j]]
			}
			return ids[i] < ids[j]
		})
		ids = ids[:maxTerms]
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	v := Vector{Indices: ids, Values: make([]float32, len(ids))}
	for i, id := range ids {
		v.Values[i] = weights[id]
	}
	return v
}

// stopwords are common English words too frequent to help a keyword match.
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an and are as at be but by can do does for from has have how i if in
		into is it its of on or so than that the their then there these they this to was we were what when
		where which who why will with you your`) {
		m[w] = true
	}
	return m
}()

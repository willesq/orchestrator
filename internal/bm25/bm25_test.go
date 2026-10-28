package bm25

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"queue.worker.codex.enabled", []string{"queue.worker.codex.enabled", "queue", "worker", "codex", "enabled"}},
		{"ProcessActivity", []string{"processactivity", "process", "activity"}},
		{"VALID_SOURCE_TYPES", []string{"valid_source_types", "valid", "source", "types"}},
		{"internal/testsupport fixtures", []string{"internal/testsupport", "internal", "testsupport", "fixtures"}},
		{"bump-formula.yml", []string{"bump-formula.yml", "bump", "formula", "yml"}},
		{"HTTPServer v2", []string{"httpserver", "http", "server", "v2"}},
		{"How does the sync work?", []string{"sync", "work"}},
		{"gh auth token", []string{"gh", "auth", "token"}},
		{"a I x", nil},
		{"Ünïcode café", []string{"ünïcode", "café"}},
		{"", nil},
	}
	for _, tt := range tests {
		if got := Tokens(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("Tokens(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// A term longer than 64 bytes is dropped rather than indexed.
	if got := Tokens(strings.Repeat("x", 65)); len(got) != 0 {
		t.Errorf("overlong term kept: %q", got)
	}
}

func TestDocumentWeights(t *testing.T) {
	p := Params{K1: 1.2, B: 0.75, AvgLen: 4}
	v := Document("sync sync outbox", p)
	if len(v.Indices) != 2 || !slices.IsSorted(v.Indices) || len(v.Values) != 2 {
		t.Fatalf("vector = %+v", v)
	}
	// Recompute by hand: len 3, avgLen 4.
	norm := 1.2 * (1 - 0.75 + 0.75*3.0/4)
	want := map[uint32]float64{TermID("sync"): 2 * 2.2 / (2 + norm), TermID("outbox"): 1 * 2.2 / (1 + norm)}
	for i, id := range v.Indices {
		if math.Abs(float64(v.Values[i])-want[id]) > 1e-6 {
			t.Errorf("term %d weight %v, want %v", id, v.Values[i], want[id])
		}
	}
	// More occurrences weigh more, with diminishing returns.
	one := Document("sync", p).Values[0]
	two := Document("sync sync", p).Values[0]
	if !(two > one) {
		t.Fatalf("tf not increasing: %v vs %v", one, two)
	}
	// The same term counts less in a longer document.
	short := Document("sync outbox", p)
	long := Document("sync outbox "+strings.Repeat("other words here ", 10), p)
	if weightOf(long, "sync") >= weightOf(short, "sync") {
		t.Fatal("length normalization missing")
	}
	if v := Document("the a of", p); len(v.Indices) != 0 {
		t.Fatalf("stopwords-only doc = %+v", v)
	}
}

func weightOf(v Vector, term string) float32 {
	id := TermID(term)
	for i, x := range v.Indices {
		if x == id {
			return v.Values[i]
		}
	}
	return 0
}

func TestQuery(t *testing.T) {
	v := Query("sync sync outbox table")
	if len(v.Indices) != 3 || !slices.IsSorted(v.Indices) {
		t.Fatalf("query = %+v", v)
	}
	for _, w := range v.Values {
		if w != 1 {
			t.Fatalf("query weights = %v", v.Values)
		}
	}
}

func TestTermCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxTerms+500; i++ {
		b.WriteString("term")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteByte(byte('a' + i%26))
		b.WriteString(" ")
		b.WriteString(string(rune('a' + i/26%26)))
		b.WriteString(string(rune('a'+i/676%26)) + "z ")
	}
	v := Document(b.String(), DefaultParams)
	if len(v.Indices) > maxTerms || !slices.IsSorted(v.Indices) {
		t.Fatalf("%d terms, sorted=%v", len(v.Indices), slices.IsSorted(v.Indices))
	}
}

func TestTermIDStable(t *testing.T) {
	// Indexed vectors persist, so the hash must never change: FNV-1a 32.
	if got := TermID("sync"); got != 0x97acf9a {
		t.Fatalf("TermID(sync) = %#x; changing the hash orphans every stored vector", got)
	}
	if TermID("sync") == TermID("outbox") {
		t.Fatal("collision on a trivial pair")
	}
}

func TestInvalidParamsFallBack(t *testing.T) {
	for name, p := range map[string]Params{
		"zero":         {},
		"avg zero":     {K1: 1.2, B: 0.75},
		"avg negative": {K1: 1.2, B: 0.75, AvgLen: -2},
		"b too big":    {K1: 1.2, B: 2, AvgLen: 55},
		"nan k1":       {K1: math.NaN(), B: 0.75, AvgLen: 55},
	} {
		if p.Validate() == nil {
			t.Errorf("%s: Validate accepted %+v", name, p)
		}
		v := Document("sync sync outbox", p)
		want := Document("sync sync outbox", DefaultParams)
		if !slices.Equal(v.Values, want.Values) {
			t.Errorf("%s: did not fall back to defaults: %v", name, v.Values)
		}
		for _, w := range v.Values {
			if math.IsNaN(float64(w)) || math.IsInf(float64(w), 0) || w <= 0 {
				t.Errorf("%s: weight %v", name, w)
			}
		}
	}
	if DefaultParams.Validate() != nil {
		t.Fatal("defaults invalid")
	}
}

func TestUnicode(t *testing.T) {
	// Composed and decomposed accents are the same term.
	if nfc, nfd := Tokens("caf\u00e9"), Tokens("cafe\u0301"); !slices.Equal(nfc, nfd) || !slices.Equal(nfc, []string{"caf\u00e9"}) {
		t.Fatalf("NFC %q vs NFD %q", nfc, nfd)
	}
	// Combining marks stay inside a word.
	if got := Tokens("नमस्ते दुनिया"); !slices.Equal(got, []string{"नमस्ते", "दुनिया"}) {
		t.Fatalf("hindi = %q", got)
	}
	// A word inside an unspaced sentence matches by its character pairs.
	doc := Document("東京都の天気予報は晴れです", DefaultParams)
	q := Query("天気")
	if q.IsEmpty() || !slices.Contains(doc.Indices, q.Indices[0]) {
		t.Fatalf("cjk query %v not in doc", q.Indices)
	}
	if got := Tokens("東京tower"); !slices.Equal(got, []string{"東京", "tower"}) {
		t.Fatalf("mixed script = %q", got)
	}
	if got := Tokens("ab\xffcd"); !slices.Equal(got, []string{"ab", "cd"}) {
		t.Fatalf("invalid utf-8 = %q", got)
	}
}

// Golden tokens for TokenizerVersion 2: if this changes, bump the version and
// re-index, since stored vectors no longer match queries.
func TestTokenizerVersionGolden(t *testing.T) {
	if TokenizerVersion != 2 {
		t.Fatal("update the golden list with the new version")
	}
	got := Tokens("Run queue.worker.codex.enabled via ProcessActivity — café 天気です " +
		"rag.search.keyword_weight internal/rag/search.go OAuth2Token parse_HTTPRequest")
	want := []string{"run", "queue.worker.codex.enabled", "queue", "worker", "codex", "enabled", "via",
		"processactivity", "process", "activity", "café", "天気", "気で", "です",
		"rag.search.keyword_weight", "keyword_weight", "rag", "search", "keyword", "weight",
		"internal/rag/search.go", "search.go", "internal", "rag", "search", "go",
		"oauth2token", "auth2", "token",
		"parse_httprequest", "httprequest", "parse", "http", "request"}
	if !slices.Equal(got, want) {
		t.Fatalf("tokens = %q", got)
	}
}

func TestCompoundPartsMatchAsWholes(t *testing.T) {
	cases := []struct{ doc, query string }{
		{"set rag.search.keyword_weight to 0.5", "keyword_weight"},
		{"see internal/rag/search.go", "search.go"},
		{"the my_ProcessActivity hook", "ProcessActivity"},
		{"refresh the OAuth2Token", "token"},
		{"an HTTP2Server", "server"},
		{"decode a utf8String", "string"},
	}
	for _, c := range cases {
		doc := Document(c.doc, DefaultParams)
		for _, id := range Query(c.query).Indices {
			if !slices.Contains(doc.Indices, id) {
				t.Errorf("%q: query %q term %d not in the document", c.doc, c.query, id)
			}
		}
	}
	if got := Tokens("x86_64 config"); !slices.Equal(got, []string{"x86_64", "x86", "64", "config"}) {
		t.Fatalf("digits = %q", got)
	}
	if got := Tokens("README"); !slices.Equal(got, []string{"readme"}) {
		t.Fatalf("plain word = %q", got)
	}
}

func TestInputBounds(t *testing.T) {
	if n := len(Tokens(strings.Repeat("word ", 3*maxTokens))); n != maxTokens {
		t.Fatalf("%d tokens, want the cap %d", n, maxTokens)
	}
	long := strings.TrimSuffix(strings.Repeat("ab.", 500), ".")
	if parts := splitParts(long); len(parts) != maxParts {
		t.Fatalf("%d parts, want %d", len(parts), maxParts)
	}
	huge := strings.Repeat("aA", 1<<20)
	allocs := testing.AllocsPerRun(1, func() { _ = Document(huge, DefaultParams) })
	if allocs > 5e6 {
		t.Fatalf("%v allocations for a 2 MB word", allocs)
	}
}

func TestEmptyVectorsAreNonNil(t *testing.T) {
	for _, v := range []Vector{Document("", DefaultParams), Document("the of", DefaultParams), Query("the"), Query("")} {
		if v.Indices == nil || v.Values == nil || !v.IsEmpty() {
			t.Fatalf("empty vector = %+v", v)
		}
	}
}

func TestTinyDocumentsDoNotDominate(t *testing.T) {
	tiny := weightOf(Document("secret", DefaultParams), "secret")
	normal := weightOf(Document("secret "+strings.Repeat("other words ", 27), DefaultParams), "secret")
	if tiny > 1.5*normal {
		t.Fatalf("one-term document weighs %v against %v", tiny, normal)
	}
}

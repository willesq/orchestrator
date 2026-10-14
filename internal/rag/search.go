package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/bm25"
)

// Hit is a search result surfaced to callers.
type Hit struct {
	Text       string
	SourceURI  string
	SourceType string
	Scope      string
	Tags       []string
	// Score is the hit's cosine similarity to the query, whichever search
	// found it, so a relevance floor keeps one meaning in every mode.
	Score float32
	// Keyword is the hit's BM25 score, or 0 when keyword search did not find it.
	Keyword float32
	// Exact reports that the hit contains, verbatim, a code identifier from
	// the query (a name with dots, slashes, underscores or camelCase).
	Exact bool
}

// Search modes.
const (
	ModeHybrid = "hybrid"
	ModeDense  = "dense"
)

// SearchOpts filters a search.
type SearchOpts struct {
	TopK       int
	Scope      string
	SourceType string
	Tags       []string
	// Mode overrides the service's SearchMode for this search ("" keeps it).
	Mode string
}

// ErrHybridUnavailable means a search asked explicitly for hybrid mode could
// not run as one.
var ErrHybridUnavailable = errors.New("hybrid search unavailable")

// Search bounds.
const (
	maxTopK       = 100
	candidatesPer = 4 // hits each side of a hybrid search offers, per hit asked for
	maxCandidates = 400
	perSourceCap  = 2 // keyword hits one source may offer the fusion
)

// Search embeds the query and returns the top matching chunks. In hybrid mode
// on a hybrid collection, dense and BM25 keyword searches run side by side and
// are merged by reciprocal rank fusion: each list contributes 1/(k+rank) for
// every hit it holds, so a chunk both find rises, and one found only by exact
// words still places. Otherwise it is a dense search.
//
// When hybrid mode comes from configuration and cannot run, the search answers
// by meaning alone and the returned note says why; when the caller asked for
// hybrid explicitly, it fails with ErrHybridUnavailable instead.
func (s *Service) Search(ctx context.Context, query string, opts SearchOpts) ([]Hit, string, error) {
	opts.TopK = min(max(opts.TopK, 0), maxTopK)
	if opts.TopK == 0 {
		opts.TopK = 5
	}
	vecs, err := s.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, "", err
	}
	if len(vecs) == 0 {
		return nil, "", nil
	}
	must := map[string]string{}
	if opts.Scope != "" {
		must["scope"] = opts.Scope
	}
	if opts.SourceType != "" {
		must["source_type"] = opts.SourceType
	}
	st := s.active()
	mode, explicit := opts.Mode, opts.Mode != ""
	if !explicit {
		mode = s.SearchMode
	}
	idents := queryIdentifiers(query)
	var note string
	if mode == ModeHybrid {
		l, lerr := st.Layout(ctx)
		switch {
		case lerr == nil && l == LayoutHybrid:
			hits, kerr, err := s.hybridSearch(ctx, st, query, vecs[0], opts, must, idents)
			if err != nil || kerr == nil {
				return hits, "", err
			}
			// Qdrant's error detail stays in the log, not in the reply to clients.
			s.fallbackLog.printf("rag: keyword search failed: %v", kerr)
			if explicit {
				return nil, "", fmt.Errorf("%w: keyword search failed (see orchd log)", ErrHybridUnavailable)
			}
			note = "keyword search failed; answered by meaning only"
		case lerr != nil:
			s.fallbackLog.printf("rag: reading the collection layout failed: %v", lerr)
			if explicit {
				return nil, "", fmt.Errorf("%w: collection layout unreadable (see orchd log)", ErrHybridUnavailable)
			}
			note = "collection layout unknown; answered by meaning only"
		default:
			if explicit {
				return nil, "", fmt.Errorf("%w: collection %q has no keyword index (run orch rag migrate-hybrid)", ErrHybridUnavailable, st.Collection())
			}
			note = "collection has no keyword index; answered by meaning only"
		}
	}
	raw, err := st.Search(ctx, vecs[0], opts.TopK, must, opts.Tags)
	if err != nil {
		return nil, "", err
	}
	hits := make([]Hit, 0, len(raw))
	for _, r := range raw {
		hits = append(hits, toHit(r, r.Score, 0, idents))
	}
	return hits, note, nil
}

// hybridSearch fuses dense and keyword results. A keyword failure is returned
// separately as kerr with the dense results, so the caller decides whether
// answering by meaning alone is acceptable.
func (s *Service) hybridSearch(ctx context.Context, st *QdrantStore, query string, vec []float32, opts SearchOpts, must map[string]string, idents []string) (hits []Hit, kerr, err error) {
	n := min(max(opts.TopK*candidatesPer, 20), maxCandidates)
	var dense, keyword []SearchHit
	var derr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); dense, derr = st.Search(ctx, vec, n, must, opts.Tags) }()
	go func() {
		defer wg.Done()
		keyword, kerr = st.KeywordSearch(ctx, bm25.Query(query), min(n*2, maxCandidates), must, opts.Tags)
	}()
	wg.Wait()
	if derr != nil {
		return nil, nil, derr
	}
	if kerr != nil {
		keyword = nil
	}
	k := float64(s.RRFK)
	if k <= 0 {
		k = 60
	}
	kw := s.KeywordWeight
	if kw <= 0 || math.IsNaN(kw) || math.IsInf(kw, 0) {
		kw = 1
	}
	type fused struct {
		hit     SearchHit
		rrf     float64
		cosine  float32
		keyword float32
		hasCos  bool
	}
	byID := map[string]*fused{}
	var order []string
	add := func(h SearchHit, rank int, weight float64) *fused {
		id := h.PointID()
		f, ok := byID[id]
		if !ok {
			f = &fused{hit: h}
			byID[id] = f
			order = append(order, id)
		}
		f.rrf += weight / (k + float64(rank))
		return f
	}
	for i, h := range dense {
		f := add(h, i+1, 1)
		f.cosine, f.hasCos = h.Score, true
	}
	for i, h := range capPerSource(keyword) {
		f := add(h, i+1, kw)
		f.keyword = h.Score
		if !f.hasCos {
			f.cosine, f.hasCos = cosineWith(vec, h.Vector[denseName]), true
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := byID[order[i]], byID[order[j]]
		if a.rrf != b.rrf {
			return a.rrf > b.rrf
		}
		return a.cosine > b.cosine
	})
	if len(order) > opts.TopK {
		order = order[:opts.TopK]
	}
	hits = make([]Hit, 0, len(order))
	for _, id := range order {
		f := byID[id]
		hits = append(hits, toHit(f.hit, f.cosine, f.keyword, idents))
	}
	return hits, kerr, nil
}

// capPerSource keeps at most perSourceCap hits from each source, in order, so
// one source repeating a rare term many times cannot fill the keyword list on
// its own. The keyword list is fetched twice as deep to leave room for others;
// dense results are not capped, so a search filtered to one source still fills.
func capPerSource(hits []SearchHit) []SearchHit {
	seen := map[string]int{}
	out := hits[:0:0]
	for _, h := range hits {
		uri := str(h.Payload["source_uri"])
		if seen[uri] >= perSourceCap {
			continue
		}
		seen[uri]++
		out = append(out, h)
	}
	return out
}

// rateLog logs at most once a minute, so a broken keyword index does not
// write a line for every prompt.
type rateLog struct {
	mu   sync.Mutex
	last time.Time
}

func (r *rateLog) printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.last) >= time.Minute {
		r.last = now
		log.Printf(format, args...)
	}
}

func toHit(r SearchHit, cosine, keyword float32, idents []string) Hit {
	text := str(r.Payload["text"])
	return Hit{
		Text:       text,
		SourceURI:  str(r.Payload["source_uri"]),
		SourceType: str(r.Payload["source_type"]),
		Scope:      str(r.Payload["scope"]),
		Tags:       strSlice(r.Payload["tags"]),
		Score:      cosine,
		Keyword:    keyword,
		Exact:      containsAny(text, idents),
	}
}

// cosineWith is the cosine similarity of q and a stored dense vector, 0 when
// the vector is missing or malformed.
func cosineWith(q []float32, raw json.RawMessage) float32 {
	var v []float32
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || len(v) != len(q) {
		return 0
	}
	var dot, nq, nv float64
	for i := range q {
		dot += float64(q[i]) * float64(v[i])
		nq += float64(q[i]) * float64(q[i])
		nv += float64(v[i]) * float64(v[i])
	}
	if nq == 0 || nv == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(nq) * math.Sqrt(nv)))
}

// Code identifiers are picked out of a query and matched whole in hit text.
// Only signals ordinary prose lacks count (see isIdentifier). Hyphens and
// colons never do (follow-up, 10:30), nor do numbers, versions, abbreviations,
// word pairs (and/or), domains (github.com) or bare file names (config.yaml).
var (
	identToken  = regexp.MustCompile(`[\pL\pN_]+(?:[./][\pL\pN_]+)*`)
	camelStep   = regexp.MustCompile(`\p{Ll}\p{Lu}`)
	numericLike = regexp.MustCompile(`^[vV]?[\pN._/]+$`)
	identStop   = map[string]bool{"e.g": true, "i.e": true, "a.m": true, "p.m": true, "a.k.a": true,
		"go.mod": true, "go.sum": true, "makefile": true, "frontend/backend": true, "backend/frontend": true}
	// Endings that make a two-part dotted name a domain or a bare file name.
	weakSuffix = map[string]bool{"com": true, "org": true, "net": true, "io": true, "dev": true, "ai": true,
		"co": true, "app": true, "md": true, "txt": true, "json": true, "yaml": true, "yml": true,
		"toml": true, "ini": true, "cfg": true, "conf": true, "csv": true, "log": true, "html": true,
		"pdf": true, "png": true, "jpg": true}
)

// minSlashPart is how long one part of a single-slash name must be: package
// paths (internal/testsupport) have one, word pairs (true/false) do not.
const minSlashPart = 8

// isIdentifier reports whether a query word looks like code rather than prose:
// camelCase or snake_case; a path with two slashes, or one slash and a long
// part; or a dotted name of three parts, or two parts that is not a domain or
// a bare file name.
func isIdentifier(w string) bool {
	if camelStep.MatchString(w) || strings.Contains(w, "_") {
		return true
	}
	lw := strings.ToLower(w)
	if strings.Contains(lw, "/") {
		parts := strings.Split(lw, "/")
		if len(parts) >= 3 {
			return true
		}
		for _, p := range parts {
			if len(p) >= minSlashPart && !strings.Contains(p, ".") {
				return true
			}
		}
		return false
	}
	parts := strings.Split(lw, ".")
	switch {
	case len(parts) < 2:
		return false
	case slices.ContainsFunc(parts, func(p string) bool { return len(p) < 2 }):
		return false
	}
	return !weakSuffix[parts[len(parts)-1]]
}

// Bounds on how much of a query identifier extraction looks at.
const (
	maxQueryIdents = 16
	maxIdentQuery  = 8 << 10
	minIdentLength = 5
	maxIdentLength = 128
)

// queryIdentifiers returns the code identifiers in a query, lowercased.
func queryIdentifiers(query string) []string {
	if len(query) > maxIdentQuery {
		query = query[:maxIdentQuery]
	}
	var out []string
	for _, w := range identToken.FindAllString(query, maxQueryIdents*8) {
		if len(w) < minIdentLength || len(w) > maxIdentLength || numericLike.MatchString(w) {
			continue
		}
		lw := strings.ToLower(w)
		if identStop[lw] || !isIdentifier(w) {
			continue
		}
		out = append(out, lw)
		if len(out) == maxQueryIdents {
			break
		}
	}
	return out
}

// containsAny reports whether text holds one of idents as a whole token, so
// "1.5" never matches inside "21.50" nor "a/b" inside "data/bin".
func containsAny(text string, idents []string) bool {
	if len(idents) == 0 {
		return false
	}
	want := make(map[string]bool, len(idents))
	for _, id := range idents {
		want[id] = true
	}
	for _, tok := range identToken.FindAllString(strings.ToLower(text), -1) {
		if want[tok] {
			return true
		}
	}
	return false
}

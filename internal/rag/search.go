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
	// StartedAt and EndedAt span what the hit describes, such as the turns of
	// a transcript chunk; IngestedAt is when it was stored. Zero when unknown.
	StartedAt, EndedAt, IngestedAt time.Time
	// Heading is the markdown section the chunk came from, when it has one.
	Heading string
	// Trust is the tier its writer gave it (TrustRepo, ...); empty for content
	// stored before writers were recorded, which readers treat as unverified.
	Trust string
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
	// AllTags keeps only content carrying every one of these tags.
	AllTags []string
	// SourceURI keeps only one source's content.
	SourceURI string
	// Mode overrides the service's SearchMode for this search ("" keeps it).
	Mode string
	// Range keeps only content dated within it.
	Range TimeRange
	// PerSource caps the hits one source may return: 0 uses the service's
	// PerSource, a negative value lifts the cap. A search filtered to one
	// source is never capped.
	PerSource int
	// Ranking is RankingWeighted or RankingPlain; "" uses the service's.
	Ranking string
	// Spaces are the spaces whose collections are searched (SP-024). nil
	// searches every collection, as before spaces were enforced; SP-025
	// sets each caller's readable spaces. An empty, non-nil list searches
	// nothing.
	Spaces []string
	// LabelsV, when set, reads in each named space only chunks labelled
	// under that policy revision (INV-08); 0 reads every chunk.
	LabelsV int64
	// DirectMessages keeps chat direct messages in the search. Only an
	// explicit interactive search sets it; every other read leaves them out
	// (SP-060, D-19).
	DirectMessages bool
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
// on hybrid collections, dense and BM25 keyword searches run side by side and
// are merged by reciprocal rank fusion: each list contributes 1/(k+rank) for
// every hit it holds, so a chunk both find rises, and one found only by exact
// words still places. Otherwise it is a dense search.
//
// opts.Spaces chooses the collections read (SP-024): each space's own, with
// the payload filter on space kept inside each as the second layer. The
// lists from several collections are merged by score before fusion: cosine
// means the same in every collection, and each keyword score is ranked by its
// own space's statistics. With one collection the search is exactly the one
// collection search it was before spaces.
//
// When hybrid mode comes from configuration and cannot run, the search answers
// by meaning alone and the returned note says why; when the caller asked for
// hybrid explicitly, it fails with ErrHybridUnavailable instead.
func (s *Service) Search(ctx context.Context, query string, opts SearchOpts) ([]Hit, string, error) {
	opts.TopK = min(max(opts.TopK, 0), maxTopK)
	if opts.TopK == 0 {
		opts.TopK = 5
	}
	if len(opts.Tags) > 0 || len(opts.AllTags) > 0 || opts.SourceType != "" {
		s.ensureSearchIndexes()
	}
	targets, err := s.readStores(ctx, opts.Spaces)
	if err != nil {
		return nil, "", err
	}
	targets = withDirectMessages(withLabels(targets, opts.LabelsV), opts.DirectMessages)
	if len(targets) == 0 {
		// No space is readable: nothing to search, and nothing to embed.
		return nil, "", nil
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
	if opts.SourceURI != "" {
		must["source_uri"] = opts.SourceURI
	}
	mode, explicit := opts.Mode, opts.Mode != ""
	if !explicit {
		mode = s.SearchMode
	}
	idents := queryIdentifiers(query)
	var note string
	if mode == ModeHybrid {
		var l Layout
		var lerr error
		targets, l, lerr = layoutOf(ctx, targets)
		if len(targets) == 0 {
			return nil, "", nil
		}
		switch {
		case lerr == nil && l == LayoutHybrid:
			hits, kerr, err := s.hybridSearch(ctx, targets, query, vecs[0], opts, must, idents)
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
				return nil, "", fmt.Errorf("%w: %s has no keyword index (run orch rag migrate-hybrid)", ErrHybridUnavailable, denseCollection(ctx, s.ActiveCollection(), targets))
			}
			note = "collection has no keyword index; answered by meaning only"
		}
	}
	// Reranking and the per-source cap both need more candidates than are
	// returned; a plain, uncapped dense search needs only what it returns.
	n := opts.TopK
	if s.reranks(opts) {
		n = candidateDepth(opts.TopK)
	}
	raw, err := eachStore(ctx, targets, func(t spaceStore) ([]SearchHit, error) {
		return t.st.Search(ctx, vecs[0], n, t.must(must), opts.Tags, opts.AllTags, opts.Range)
	})
	if err != nil {
		return nil, "", err
	}
	cands := make([]candidate, 0, n)
	for _, r := range mergeByScore(raw, n) {
		cands = append(cands, candidate{hit: r.SearchHit, score: float64(r.Score), cosine: r.Score})
	}
	return s.finish(cands, opts, idents), note, nil
}

// perSource is the cap a search applies.
func (s *Service) perSource(opts SearchOpts) int {
	n := opts.PerSource
	if n == 0 {
		n = s.PerSource
	}
	if n < 0 || opts.SourceURI != "" {
		return 0
	}
	return n
}

// reranks reports whether a search reorders or filters its candidates.
func (s *Service) reranks(opts SearchOpts) bool {
	profile := opts.Ranking
	if profile == "" {
		profile = s.Ranking.Profile
	}
	return profile != RankingPlain || s.perSource(opts) > 0
}

// candidateDepth is how many hits each side of a search offers for topK.
func candidateDepth(topK int) int { return min(max(topK*candidatesPer, 20), maxCandidates) }

// finish ranks candidates and converts the survivors to hits.
func (s *Service) finish(cands []candidate, opts SearchOpts, idents []string) []Hit {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	cands = rank(cands, s.Ranking, opts.Ranking, s.perSource(opts), opts.TopK, now)
	hits := make([]Hit, 0, len(cands))
	for _, c := range cands {
		hits = append(hits, toHit(c.hit, c.cosine, c.keyword, idents))
	}
	return hits
}

// hybridSearch fuses dense and keyword results from every target. A keyword
// failure is returned separately as kerr with the dense results, so the
// caller decides whether answering by meaning alone is acceptable.
func (s *Service) hybridSearch(ctx context.Context, targets []spaceStore, query string, vec []float32, opts SearchOpts, must map[string]string, idents []string) (hits []Hit, kerr, err error) {
	n := candidateDepth(opts.TopK)
	kn := min(n*2, maxCandidates)
	// Reason: returning every keyword hit's dense vector is most of a
	// keyword request's cost (SP-002), and each extra collection pays it
	// again. Across several collections the vectors are fetched afterwards,
	// only for the keyword-only hits whose cosine is needed (SP-024.3). One
	// collection keeps the request it always made.
	lazy := len(targets) > 1 || s.lazyVectors
	var dense, keyword [][]storeHit
	var derr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dense, derr = eachStore(ctx, targets, func(t spaceStore) ([]SearchHit, error) {
			return t.st.Search(ctx, vec, n, t.must(must), opts.Tags, opts.AllTags, opts.Range)
		})
	}()
	go func() {
		defer wg.Done()
		q := bm25.Query(query)
		keyword, kerr = eachStore(ctx, targets, func(t spaceStore) ([]SearchHit, error) {
			return t.st.keywordSearch(ctx, q, kn, t.must(must), opts.Tags, opts.AllTags, opts.Range, !lazy)
		})
	}()
	wg.Wait()
	if derr != nil {
		return nil, nil, derr
	}
	var kw []storeHit
	if kerr == nil {
		kw = mergeByScore(keyword, kn)
	}
	k := float64(s.RRFK)
	if k <= 0 {
		k = 60
	}
	weight := s.KeywordWeight
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = 0.5
	}
	type fused struct {
		candidate
		hasCos     bool
		collection string
	}
	byID := map[string]*fused{}
	var seq []string
	add := func(h storeHit, rank int, w float64) *fused {
		id := h.key()
		f, ok := byID[id]
		if !ok {
			f = &fused{candidate: candidate{hit: h.SearchHit}, collection: h.collection}
			byID[id] = f
			seq = append(seq, id)
		}
		f.score += w / (k + float64(rank))
		return f
	}
	for i, h := range mergeByScore(dense, n) {
		f := add(h, i+1, 1)
		f.cosine, f.hasCos = h.Score, true
	}
	for i, h := range capPerSource(kw) {
		f := add(h, i+1, weight)
		f.keyword = h.Score
		if !f.hasCos && !lazy {
			f.cosine, f.hasCos = cosineWith(vec, h.Vector[denseName]), true
		}
	}
	cands := make([]candidate, 0, len(seq))
	lazyCos := false
	for _, id := range seq {
		f := byID[id]
		f.candidate.collection = f.collection
		if !f.hasCos {
			f.candidate.needsCos, lazyCos = true, true
		}
		cands = append(cands, f.candidate)
	}
	if !lazyCos {
		return s.finish(cands, opts, idents), kerr, nil
	}
	hits, ferr := s.finishLazy(ctx, targets, cands, vec, opts, idents)
	if ferr != nil {
		// Without the vectors the keyword-only hits have no cosine, so the
		// search answers by meaning alone, as when keyword search fails.
		var denseOnly []candidate
		for _, h := range mergeByScore(dense, n) {
			denseOnly = append(denseOnly, candidate{hit: h.SearchHit, score: float64(h.Score), cosine: h.Score})
		}
		return s.finish(denseOnly, opts, idents), ferr, nil
	}
	return hits, kerr, nil
}

// capPerSource keeps at most perSourceCap hits from each source, in order, so
// one source repeating a rare term many times cannot fill the keyword list on
// its own. The keyword list is fetched twice as deep to leave room for others;
// dense results are not capped, so a search filtered to one source still fills.
func capPerSource(hits []storeHit) []storeHit {
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
		StartedAt:  unixTime(r.Payload[fieldStartedAt]),
		EndedAt:    unixTime(r.Payload[fieldEndedAt]),
		IngestedAt: unixTime(r.Payload[fieldIngestedAt]),
		Heading:    str(r.Payload["heading"]),
		Trust:      str(r.Payload[fieldTrust]),
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

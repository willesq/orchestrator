package rageval

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// ReportVersion is the scoring a report was made with. Version 2 ranks by
// chunk position, since that is what a caller sees (the top three chunks are
// what gets auto-injected); version 0 reports ranked distinct sources, so the
// two do not compare.
const ReportVersion = 2

// Hit is one search result, reduced to what scoring needs.
type Hit struct {
	SourceURI string  `json:"source_uri"`
	Score     float32 `json:"score"`
	Text      string  `json:"-"`
	// Raw is the caller's own form of the hit, for its Injector.
	Raw any `json:"-"`
}

// Searcher runs one case's query and returns its hits, best first.
type Searcher func(ctx context.Context, c Case, topK int) ([]Hit, error)

// Injector reports how many characters auto-injection would add to the
// prompt for these hits; nil leaves InjectedTokens at zero.
type Injector func(c Case, hits []Hit) int

// Result is how one case did.
type Result struct {
	Query   string `json:"query"`
	Section string `json:"section,omitempty"`
	Split   string `json:"split,omitempty"`
	// Key identifies the case (query and filters), for pairing two runs.
	Key string `json:"key,omitempty"`
	// Rank is the 1-based position, among the hits returned, of the first hit
	// from an expected source (whose text holds the case's span, when it has
	// one); 0 means none was returned.
	Rank int `json:"rank"`
	// SourceRank is the same ignoring the span: any chunk of an expected source.
	SourceRank int      `json:"source_rank"`
	Top        []string `json:"top"` // distinct sources returned, best first
	Err        string   `json:"error,omitempty"`
	LatencyMS  float64  `json:"latency_ms"`
	// InjectedTokens estimates what auto-injection would add (4 chars a token).
	InjectedTokens int `json:"injected_tokens"`
}

// Metrics summarize a set of results.
type Metrics struct {
	N          int     `json:"n"`
	Errors     int     `json:"errors"`
	RecallAt1  float64 `json:"recall_at_1"`
	RecallAt3  float64 `json:"recall_at_3"`
	RecallAt5  float64 `json:"recall_at_5"`
	RecallAt10 float64 `json:"recall_at_10"`
	MRR        float64 `json:"mrr"`
	NDCG10     float64 `json:"ndcg_at_10"`
	// SourceHit is the share of cases with any chunk of an expected source in
	// the top k, span or not.
	SourceHit      float64 `json:"source_hit"`
	InjectedTokens float64 `json:"injected_tokens"` // mean per case
	P50MS          float64 `json:"p50_ms"`
	P95MS          float64 `json:"p95_ms"`
}

// Report is a whole run.
type Report struct {
	Version int    `json:"version"`
	TopK    int    `json:"top_k"`
	Label   string `json:"label,omitempty"`
	Metrics
	Sections map[string]Metrics `json:"sections,omitempty"`
	// Splits are the tuning and held-out halves; SectionSplits each section's
	// halves, keyed "<section>/<split>".
	Splits        map[string]Metrics `json:"splits,omitempty"`
	SectionSplits map[string]Metrics `json:"section_splits,omitempty"`
	Results       []Result           `json:"results"`
}

// Run scores every case. A case whose search fails counts as a miss and
// records the error.
func Run(ctx context.Context, s Set, topK int, search Searcher, inject Injector) Report {
	r := Report{Version: ReportVersion, TopK: topK}
	for _, c := range s.Cases {
		// A cancelled run stops rather than sending every remaining query.
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		hits, err := search(ctx, c, topK)
		res := score(c, hits)
		res.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			res.Err = err.Error()
		} else if inject != nil {
			res.InjectedTokens = (inject(c, hits) + 3) / 4
		}
		r.Results = append(r.Results, res)
	}
	r.Metrics = Summarize(r.Results, topK)
	group := func(key func(Result) string) map[string]Metrics {
		by := map[string][]Result{}
		for _, res := range r.Results {
			if k := key(res); k != "" {
				by[k] = append(by[k], res)
			}
		}
		if len(by) == 0 {
			return nil
		}
		out := map[string]Metrics{}
		for name, rs := range by {
			out[name] = Summarize(rs, topK)
		}
		return out
	}
	r.Sections = group(func(res Result) string { return res.Section })
	r.Splits = group(func(res Result) string { return res.Split })
	r.SectionSplits = group(func(res Result) string {
		if res.Section == "" {
			return ""
		}
		return res.Section + "/" + res.Split
	})
	return r
}

// score ranks one case's hits.
func score(c Case, hits []Hit) Result {
	res := Result{Query: c.Query, Section: c.Section, Split: SplitOf(c), Key: c.Key()}
	seen := map[string]bool{}
	span := strings.ToLower(strings.TrimSpace(c.Span))
	for i, h := range hits {
		if !seen[h.SourceURI] {
			seen[h.SourceURI] = true
			res.Top = append(res.Top, h.SourceURI)
		}
		if !matchesAny(h.SourceURI, c.Expect) {
			continue
		}
		if res.SourceRank == 0 {
			res.SourceRank = i + 1
		}
		if res.Rank == 0 && (span == "" || strings.Contains(strings.ToLower(h.Text), span)) {
			res.Rank = i + 1
		}
	}
	return res
}

// Summarize computes the metrics of results scored at top k.
func Summarize(results []Result, topK int) Metrics {
	m := Metrics{N: len(results)}
	if m.N == 0 {
		return m
	}
	lat := make([]float64, 0, len(results))
	for _, r := range results {
		if r.Err != "" {
			m.Errors++
		}
		m.RecallAt1 += within(r.Rank, 1)
		m.RecallAt3 += within(r.Rank, 3)
		m.RecallAt5 += within(r.Rank, 5)
		m.RecallAt10 += within(r.Rank, 10)
		m.MRR += RR(r)
		m.NDCG10 += NDCG10(r)
		m.SourceHit += within(r.SourceRank, topK)
		m.InjectedTokens += float64(r.InjectedTokens)
		lat = append(lat, r.LatencyMS)
	}
	n := float64(m.N)
	for _, f := range []*float64{&m.RecallAt1, &m.RecallAt3, &m.RecallAt5, &m.RecallAt10, &m.MRR, &m.NDCG10, &m.SourceHit, &m.InjectedTokens} {
		*f /= n
	}
	sort.Float64s(lat)
	m.P50MS, m.P95MS = percentile(lat, 0.50), percentile(lat, 0.95)
	return m
}

func within(rank, k int) float64 {
	if rank >= 1 && rank <= k {
		return 1
	}
	return 0
}

// RR is a result's reciprocal rank.
func RR(r Result) float64 {
	if r.Rank < 1 {
		return 0
	}
	return 1 / float64(r.Rank)
}

// NDCG10 is a result's nDCG@10. Expected sources are alternatives, so there
// is one relevant item and the ideal ranking puts it first.
func NDCG10(r Result) float64 {
	if r.Rank < 1 || r.Rank > 10 {
		return 0
	}
	return 1 / math.Log2(float64(r.Rank)+1)
}

// RecallAt3 is 1 when the result ranked in the top three: what auto-injection
// shows.
func RecallAt3(r Result) float64 { return within(r.Rank, 3) }

// percentile reads the q-quantile of sorted values (nearest rank).
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func matchesAny(uri string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(uri, prefix) {
				return true
			}
		} else if uri == p {
			return true
		}
	}
	return false
}

// Change is one case whose rank moved between two runs.
type Change struct {
	Query    string
	From, To int // 0 = missed
}

// Better reports whether the case improved.
func (c Change) Better() bool { return c.To != 0 && (c.From == 0 || c.To < c.From) }

// Compare lists cases present in both runs whose rank changed, in run order,
// each case once.
func Compare(before, after Report) []Change {
	var out []Change
	for _, pr := range pairResults(before, after) {
		if pr[0].Rank != pr[1].Rank {
			out = append(out, Change{Query: pr[1].Query, From: pr[0].Rank, To: pr[1].Rank})
		}
	}
	return out
}

// pairResults matches each case of after with its result in before, once:
// by case key, or failing that by query when the query names one case in
// before (a run saved before keys were recorded, or with keys spelled
// another way). A case repeated in either run pairs only its first result,
// so it cannot weigh twice.
func pairResults(before, after Report) [][2]Result {
	byKey, byQuery := map[string]int{}, map[string]int{}
	queries := map[string]int{}
	for i, r := range before.Results {
		if _, dup := byKey[r.Key]; r.Key != "" && !dup {
			byKey[r.Key] = i
		}
		if queries[r.Query]++; queries[r.Query] == 1 {
			byQuery[r.Query] = i
		}
	}
	// A before-result pairs at most once, whether matched by key or by
	// query, so one baseline row cannot stand in for two after-rows.
	used := map[int]bool{}
	var out [][2]Result
	for _, r := range after.Results {
		b, ok := byKey[r.Key]
		if !ok || r.Key == "" {
			b, ok = byQuery[r.Query]
			ok = ok && queries[r.Query] == 1
		}
		if !ok || used[b] {
			continue
		}
		used[b] = true
		out = append(out, [2]Result{before.Results[b], r})
	}
	return out
}

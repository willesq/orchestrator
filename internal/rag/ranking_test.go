package rag

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

var rankNow = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

func cand(uri, sourceType string, score float64, ended time.Time) candidate {
	p := map[string]any{"source_uri": uri, "source_type": sourceType, "text": uri}
	if !ended.IsZero() {
		p[fieldEndedAt] = float64(ended.Unix())
	}
	return candidate{hit: SearchHit{ID: uri, Payload: p}, score: score, cosine: float32(score)}
}

func uris(cs []candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, str(c.hit.Payload["source_uri"]))
	}
	return out
}

func TestRankingWeight(t *testing.T) {
	p := RankingParams{HalfLife: 100 * 24 * time.Hour, Floor: 0.5, TypeWeights: map[string]float64{"t": 0.5, "bad": math.NaN()}}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if w := p.weight("x", rankNow, rankNow); !near(w, 1) {
		t.Errorf("fresh = %v", w)
	}
	if w := p.weight("x", rankNow.Add(-100*24*time.Hour), rankNow); !near(w, 0.75) {
		t.Errorf("one half-life = %v", w)
	}
	if w := p.weight("x", rankNow.Add(-100*365*24*time.Hour), rankNow); w < 0.5 || w > 0.5001 {
		t.Errorf("ancient decays to the floor, not below: %v", w)
	}
	if w := p.weight("x", time.Time{}, rankNow); !near(w, 0.75) {
		t.Errorf("undated counts as one half-life old: %v", w)
	}
	if w := p.weight("x", rankNow.Add(time.Hour), rankNow); !near(w, 1) {
		t.Errorf("future-dated is not boosted: %v", w)
	}
	if w := p.weight("t", rankNow, rankNow); !near(w, 0.5) {
		t.Errorf("type weight = %v", w)
	}
	if w := p.weight("bad", rankNow, rankNow); !near(w, 1) {
		t.Errorf("a NaN weight is ignored: %v", w)
	}
	off := RankingParams{TypeWeights: map[string]float64{"t": 2}}
	if w := off.weight("t", time.Time{}, rankNow); !near(w, 2) {
		t.Errorf("no half-life means no recency: %v", w)
	}
	if w := (RankingParams{HalfLife: time.Hour, Floor: 7}).weight("x", rankNow.Add(-1000*time.Hour), rankNow); !near(w, 1) {
		t.Errorf("a floor above 1 is clamped: %v", w)
	}
}

func TestRankOrdersCapsAndCuts(t *testing.T) {
	old := rankNow.AddDate(-2, 0, 0)
	cands := func() []candidate {
		return []candidate{
			cand("transcript", "session_transcript", 1.0, rankNow),
			cand("record", "session_record", 0.9, rankNow),
			cand("old-doc", "repo_doc", 0.95, old),
			cand("record", "session_record", 0.5, rankNow),
			cand("note", "manual", 0.4, rankNow),
		}
	}
	p := DefaultRanking()
	if got := uris(rank(cands(), p, RankingPlain, 0, 10, rankNow)); !slices.Equal(got, []string{"transcript", "old-doc", "record", "record", "note"}) {
		t.Errorf("plain = %v", got)
	}
	// Weighted: the record outranks the transcript it summarizes, and the old
	// doc drops but stays above the weak note (the floor).
	if got := uris(rank(cands(), p, "", 0, 10, rankNow)); !slices.Equal(got, []string{"record", "transcript", "old-doc", "record", "note"}) {
		t.Errorf("weighted = %v", got)
	}
	if got := uris(rank(cands(), p, RankingWeighted, 1, 3, rankNow)); !slices.Equal(got, []string{"record", "transcript", "old-doc"}) {
		t.Errorf("capped and cut = %v", got)
	}
	// Ties fall back to cosine.
	a, b := cand("a", "x", 1, rankNow), cand("b", "x", 1, rankNow)
	a.cosine, b.cosine = 0.1, 0.9
	if got := uris(rank([]candidate{a, b}, RankingParams{Profile: RankingPlain}, "", 0, 2, rankNow)); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("tie = %v", got)
	}
	// A candidate dated only by when it was stored is still dated.
	c := candidate{hit: SearchHit{Payload: map[string]any{fieldIngestedAt: float64(rankNow.Unix())}}}
	if !c.date().Equal(rankNow) {
		t.Errorf("ingest date = %v", c.date())
	}
	c.hit.Payload[fieldStartedAt] = float64(old.Unix())
	if !c.date().Equal(old) {
		t.Errorf("start date = %v", c.date())
	}
}

func TestSearchAppliesRankingAndCap(t *testing.T) {
	day := func(d time.Time) float64 { return float64(d.Unix()) }
	recent := time.Now().Add(-time.Hour)
	mk := func(id, uri, st string, score float64) map[string]any {
		return map[string]any{"id": id, "score": score, "payload": map[string]any{
			"text": id, "source_uri": uri, "source_type": st, fieldEndedAt: day(recent), "heading": "H › " + id}}
	}
	dense := []map[string]any{
		mk("t1", "session://s/transcript", "session_transcript", 0.9),
		mk("t2", "session://s/transcript", "session_transcript", 0.89),
		mk("r1", "session://s/record", "session_record", 0.85),
		mk("d1", "repo://o/r/a.md", "repo_doc", 0.6),
	}
	st, dc, _ := hybridStub(t, LayoutDense, dense, nil, false)
	svc := NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16)
	svc.SearchMode = ModeDense
	got := func(opts SearchOpts) []string {
		t.Helper()
		hits, _, err := svc.Search(context.Background(), "q", opts)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.Text)
		}
		return out
	}
	if g := got(SearchOpts{TopK: 3, Ranking: RankingPlain}); !slices.Equal(g, []string{"t1", "t2", "r1"}) {
		t.Errorf("plain = %v", g)
	}
	if g := got(SearchOpts{TopK: 3}); !slices.Equal(g, []string{"r1", "t1", "t2"}) {
		t.Errorf("weighted = %v", g)
	}
	if g := got(SearchOpts{TopK: 3, PerSource: 1}); !slices.Equal(g, []string{"r1", "t1", "d1"}) {
		t.Errorf("capped = %v", g)
	}
	svc.PerSource = 1
	if g := got(SearchOpts{TopK: 3, PerSource: -1}); len(g) != 3 || g[2] != "t2" {
		t.Errorf("a negative cap lifts the service default: %v", g)
	}
	if g := got(SearchOpts{TopK: 3, SourceURI: "session://s/transcript"}); len(g) != 3 {
		t.Errorf("a one-source search is never capped: %v", g)
	}
	hits, _, _ := svc.Search(context.Background(), "q", SearchOpts{TopK: 1})
	if len(hits) != 1 || hits[0].Heading != "H › r1" {
		t.Errorf("heading = %+v", hits)
	}
	// A plain, uncapped dense search asks only for what it returns.
	svc.PerSource, svc.Ranking.Profile = 0, RankingPlain
	before := dc.Load()
	_ = got(SearchOpts{TopK: 2})
	if dc.Load() != before+1 || svc.reranks(SearchOpts{}) {
		t.Errorf("plain dense search reranks")
	}
	if !svc.reranks(SearchOpts{Ranking: RankingWeighted}) || candidateDepth(1) != 20 || candidateDepth(1000) != maxCandidates {
		t.Error("reranks/candidateDepth")
	}
	if !strings.HasPrefix(DefaultRanking().Profile, "weighted") {
		t.Error("default profile")
	}
}

func nanFloat() float64 { z := 0.0; return z / z }

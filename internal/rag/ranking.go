package rag

import (
	"math"
	"sort"
	"time"
)

// Ranking profiles a search may ask for.
const (
	// RankingWeighted scales each candidate's fused score by its source type's
	// weight and by how recent it is.
	RankingWeighted = "weighted"
	// RankingPlain ranks by the fused (or cosine) score alone.
	RankingPlain = "plain"
)

// RankingParams weight candidates after fusion. A candidate's score becomes
// score × type weight × recency, where recency falls from 1 for content dated
// now toward Floor, halving its distance to Floor every HalfLife: an old
// decision that still matches well keeps most of its score rather than being
// buried by a fresher, weaker match.
type RankingParams struct {
	// Profile is RankingWeighted or RankingPlain (rag.search.ranking).
	Profile string
	// HalfLife is how long recency takes to fall halfway to Floor; zero turns
	// recency off.
	HalfLife time.Duration
	// Floor is the least recency can scale a score by, in [0, 1].
	Floor float64
	// TypeWeights scale scores by source type; a missing type weighs 1.
	TypeWeights map[string]float64
}

// DefaultTypeWeights favour what records a decision or a summary over the raw
// transcripts the summaries were written from, and own knowledge over web
// pages. Tuned on the golden-set eval; see docs/internals/memory.md.
func DefaultTypeWeights() map[string]float64 {
	return map[string]float64{
		"session_record":     1.0,
		"decision":           1.0,
		"session_transcript": 0.85,
		"web":                0.9,
	}
}

// DefaultRanking is the ranking a new Service uses.
func DefaultRanking() RankingParams {
	return RankingParams{Profile: RankingWeighted, HalfLife: 180 * 24 * time.Hour, Floor: 0.85, TypeWeights: DefaultTypeWeights()}
}

// weight is the factor a candidate's score is scaled by.
func (p RankingParams) weight(sourceType string, date time.Time, now time.Time) float64 {
	w := 1.0
	if tw, ok := p.TypeWeights[sourceType]; ok && finitePositive(tw) {
		w = tw
	}
	if p.HalfLife <= 0 {
		return w
	}
	floor := p.Floor
	if math.IsNaN(floor) {
		floor = DefaultRanking().Floor
	}
	floor = math.Min(math.Max(floor, 0), 1)
	// Undated content is treated as one half-life old: neither favoured as new
	// nor sunk as old.
	age := p.HalfLife
	if !date.IsZero() {
		age = max(now.Sub(date), 0)
	}
	decay := math.Exp2(-float64(age) / float64(p.HalfLife))
	return w * (floor + (1-floor)*decay)
}

func finitePositive(f float64) bool { return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f) }

// candidate is one search result before final ranking.
type candidate struct {
	hit     SearchHit
	score   float64 // fused or cosine score, higher is better
	cosine  float32
	keyword float32
}

// date is when a candidate's content happened, or failing that, when it was
// stored.
func (c candidate) date() time.Time {
	if t := unixTime(c.hit.Payload[fieldEndedAt]); !t.IsZero() {
		return t
	}
	if t := unixTime(c.hit.Payload[fieldStartedAt]); !t.IsZero() {
		return t
	}
	return unixTime(c.hit.Payload[fieldIngestedAt])
}

// rank orders candidates for the caller: weighted by type and recency unless
// the profile is plain, then capped per source, then cut to topK.
func rank(cands []candidate, p RankingParams, profile string, perSource, topK int, now time.Time) []candidate {
	if profile == "" {
		profile = p.Profile
	}
	if profile != RankingPlain {
		for i := range cands {
			// A weight below 1 would raise a negative (dense-only) score.
			cands[i].score = max(cands[i].score, 0)
			cands[i].score *= p.weight(str(cands[i].hit.Payload["source_type"]), cands[i].date(), now)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].cosine > cands[j].cosine
	})
	if perSource > 0 {
		cands = capSources(cands, perSource)
	}
	if len(cands) > topK {
		cands = cands[:topK]
	}
	return cands
}

// capSources keeps at most n candidates from each source, in order, so one
// document cannot fill the few results a caller shows.
func capSources(cands []candidate, n int) []candidate {
	seen := map[string]int{}
	out := cands[:0:0]
	for _, c := range cands {
		uri := str(c.hit.Payload["source_uri"])
		if seen[uri] >= n {
			continue
		}
		seen[uri]++
		out = append(out, c)
	}
	return out
}

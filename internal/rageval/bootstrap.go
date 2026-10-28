package rageval

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Paired is a paired-bootstrap comparison of one metric between two runs.
type Paired struct {
	Metric string  `json:"metric"`
	N      int     `json:"n"` // cases present in both runs
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Delta  float64 `json:"delta"`
	// Lo and Hi bound the 95% confidence interval of Delta.
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
	// P is the share of resamples in which after did not beat before (one-sided):
	// small means the gain is unlikely to be noise.
	P float64 `json:"p"`
}

// minSignificantN is the fewest paired cases a comparison is called
// significant on: below it the bootstrap interval says little.
const minSignificantN = 10

// Significant reports whether the interval excludes zero, over enough cases.
func (p Paired) Significant() bool { return p.N >= minSignificantN && (p.Lo > 0 || p.Hi < 0) }

// bootstrapIters resamples a comparison; enough for a stable 95% interval.
const bootstrapIters = 10000

// Bootstrap compares a per-case metric between two runs over the cases both
// hold (matched by query), resampling cases with replacement. The seed is
// fixed, so the same two runs always give the same answer.
func Bootstrap(name string, before, after Report, metric func(Result) float64) Paired {
	var deltas []float64
	p := Paired{Metric: name}
	for _, pr := range pairResults(before, after) {
		b, r := pr[0], pr[1]
		vb, va := metric(b), metric(r)
		if math.IsNaN(vb) || math.IsNaN(va) || math.IsInf(vb, 0) || math.IsInf(va, 0) {
			continue
		}
		p.Before += vb
		p.After += va
		deltas = append(deltas, va-vb)
	}
	p.N = len(deltas)
	if p.N == 0 {
		return p
	}
	n := float64(p.N)
	p.Before /= n
	p.After /= n
	p.Delta = p.After - p.Before
	rng := rand.New(rand.NewPCG(1, 2))
	means := make([]float64, bootstrapIters)
	notBetter := 0
	for i := range means {
		var sum float64
		for range deltas {
			sum += deltas[rng.IntN(len(deltas))]
		}
		means[i] = sum / n
		if means[i] <= 0 {
			notBetter++
		}
	}
	sort.Float64s(means)
	p.Lo = means[int(0.025*bootstrapIters)]
	p.Hi = means[int(0.975*bootstrapIters)-1]
	p.P = float64(notBetter) / bootstrapIters
	return p
}

// CompareMetrics runs the bootstrap on the metrics a report summarizes.
func CompareMetrics(before, after Report) []Paired {
	return []Paired{
		Bootstrap("MRR", before, after, RR),
		Bootstrap("recall@3", before, after, RecallAt3),
		Bootstrap("nDCG@10", before, after, NDCG10),
	}
}

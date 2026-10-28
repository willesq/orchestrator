package daemon

import (
	"log"
	"math"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
)

// rankingParams reads rag.search's ranking settings, falling back to the
// defaults for any value out of range or unset.
func rankingParams(s config.SearchSettings) rag.RankingParams {
	p := rag.DefaultRanking()
	switch s.Ranking {
	case "":
	case rag.RankingWeighted, rag.RankingPlain:
		p.Profile = s.Ranking
	default:
		log.Printf("orchd: rag.search.ranking %q is not weighted or plain; using %s", s.Ranking, p.Profile)
	}
	if d := s.RecencyHalfLifeDays; d >= 0 && d <= 36500 && !math.IsNaN(d) {
		p.HalfLife = time.Duration(d * float64(24*time.Hour))
	}
	if f := s.RecencyFloor; f >= 0 && f <= 1 {
		p.Floor = f
	}
	// Configured weights overlay the defaults: setting one type keeps the
	// others' defaults (config replaces the whole map, so it is merged here).
	for k, w := range s.SourceWeights {
		if w > 0 && w <= 10 {
			p.TypeWeights[k] = w
		} else {
			log.Printf("orchd: rag.search.source_weights.%s = %v is not in (0, 10]; ignored", k, w)
		}
	}
	return p
}

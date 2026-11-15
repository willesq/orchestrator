package status

import (
	"fmt"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// quarantineLine reports how much content quarantine holds (SP-028): counts
// only, never a source. ok is false when orchd sent no counts (no policy
// configured, an older orchd, or a store it could not count).
func quarantineLine(sp *orchdv1.SpacesStatus) (Line, bool) {
	q := sp.GetQuarantine()
	if q == nil {
		return Line{}, false
	}
	l := Line{Name: "quarantine", Level: LevelOK, Value: "empty"}
	if q.GetSources() > 0 {
		l.Value = fmt.Sprintf("%d source(s), %d chunk(s)", q.GetSources(), q.GetChunks())
		l.Detail = "content that could not be placed; review with 'orch quarantine list'"
	}
	return l, true
}

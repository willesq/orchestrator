package status

import (
	"fmt"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// The memory spaces switch's states as orchd reports them.
const (
	spacesOff       = "off"
	spacesTurningOn = "turning_on"
)

// spacesOffSection reports spaces switched off. With nothing kept and no
// move running there is nothing to say, so the report is as before spaces.
func spacesOffSection(sp *orchdv1.SpacesStatus) (Section, bool) {
	sec := Section{Title: "spaces"}
	move, moving := storeMoveLine(sp.GetStoreMove())
	if sp.GetAppliedRevision() == 0 && !moving {
		return sec, false
	}
	l := Line{Name: "switch", Value: "off", Level: LevelInfo,
		Detail: "nothing is enforced; turn on with 'orch spaces on' or in Orch.app"}
	if n := sp.GetAppliedRevision(); n > 0 {
		l.Detail = fmt.Sprintf("nothing is enforced; policy revision %d is kept for when spaces are turned on ('orch spaces on' or Orch.app)", n)
	}
	sec.Lines = append(sec.Lines, l)
	if moving {
		sec.Lines = append(sec.Lines, move)
	}
	return sec, true
}

// storeMoveLine reports the switch's store move, if one runs or failed. It
// names counts only, never a source or space.
func storeMoveLine(m *orchdv1.SpacesStoreMove) (Line, bool) {
	if m == nil {
		return Line{}, false
	}
	what := "merging every space's memory into one store"
	if m.GetDirection() == "split" {
		what = "moving memory back into its spaces"
	}
	l := Line{Name: "store move", Level: LevelInfo,
		Value: fmt.Sprintf("%s: %d of %d collection(s), %d source(s) and %d chunk(s) moved", what,
			m.GetCollectionsDone(), m.GetCollections(), m.GetSources(), m.GetChunks())}
	if n := m.GetFailed(); n > 0 {
		l.Level = LevelWarn
		l.Detail = fmt.Sprintf("%d chunk(s) could not be moved and stay where they are; it tries them again", n)
	}
	if e := m.GetError(); e != "" {
		l.Level, l.Detail = LevelWarn, printable(e)+"; it runs again"
	}
	return l, true
}

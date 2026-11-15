package status

import (
	"fmt"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// countLines reports what the spaces hold (SP-031): the spaces the caller
// may see with their chunks, how far a relabel has got, and the sessions in
// a conflict. orchd narrows the counts to the caller, so a session sees only
// its own space and public ones. Nothing is reported when orchd sent no
// counts (no policy applied, or an older orchd).
//
// Reason for printable: space names come from the policy in orch.db, which
// a process running as the user can edit, and land in a terminal.
func countLines(sp *orchdv1.SpacesStatus) []Line {
	c := sp.GetCounts()
	if c == nil {
		return nil
	}
	if e := c.GetError(); e != "" {
		return []Line{{Name: "counts", Value: "unavailable", Level: LevelWarn, Detail: printable(e)}}
	}
	head := Line{Name: "spaces", Level: LevelOK, Value: fmt.Sprintf("%d", len(c.GetSpaces()))}
	if c.GetEverySpace() {
		head.Detail = "every space of the policy"
	} else {
		head.Value += " readable here"
		head.Detail = "your own space and public ones; Orch.app shows every space"
	}
	lines := []Line{head}
	for _, s := range c.GetSpaces() {
		l := Line{Name: "space " + printable(s.GetSpace()), Level: LevelOK,
			Value: fmt.Sprintf("%d chunk(s)", s.GetChunks()), Detail: printable(s.GetKind())}
		if n := s.GetStale(); n > 0 {
			l.Detail += fmt.Sprintf(", %d not yet relabelled", n)
		}
		lines = append(lines, l)
	}
	return append(lines, relabelLine(c), conflictLine(c))
}

// relabelLine is how far 'orch rag relabel' has brought the counted chunks
// up to the applied revision.
func relabelLine(c *orchdv1.SpacesCounts) Line {
	l := Line{Name: "relabel", Level: LevelOK, Value: "done"}
	total, stale := c.GetChunks(), c.GetStale()
	switch {
	case total == 0:
		l.Value = "nothing stored"
	case stale > 0:
		l.Level = LevelWarn
		l.Value = fmt.Sprintf("%d of %d chunk(s) labelled (%d%%)", total-stale, total, int64(total-stale)*100/int64(total))
		l.Detail = "chunks not yet labelled by the applied revision are unreadable; run 'orch rag relabel'"
	}
	return l
}

// conflictLine counts live sessions whose folder two isolated spaces claim.
func conflictLine(c *orchdv1.SpacesCounts) Line {
	l := Line{Name: "conflicts", Level: LevelOK, Value: "none"}
	switch {
	case !c.GetConflictsCounted():
		l.Value = "shown to the operator"
		l.Detail = "Orch.app counts them"
	case c.GetConflicts() > 0:
		l.Level = LevelWarn
		l.Value = fmt.Sprintf("%d session(s)", c.GetConflicts())
		l.Detail = "two isolated spaces claim their folder, so they have no space; bind the folder to one space"
	}
	return l
}

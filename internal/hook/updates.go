package hook

import (
	"fmt"
	"io"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
)

// updateStreams are the event streams surfaced mid-session: the session's own
// stream, where its finished subagents report, and "queue", where background
// tasks report. The PR babysitter's stream refreshes every few minutes and stays
// in the SessionStart digest only.
func updateStreams(sessionID string) []string {
	if !sessionIDPattern.MatchString(sessionID) {
		return []string{taskEventStream}
	}
	return []string{sessionID, taskEventStream}
}

// taskEventStream is where orchd reports finished queue tasks.
const taskEventStream = "queue"

// maxUpdateLines bounds one prompt's update block; a long absence summarizes
// the overflow instead of flooding the turn.
const maxUpdateLines = 10

// markSeen records every event currently in the update streams as shown, so
// the first prompt after SessionStart does not repeat what its digest just said.
func markSeen(st *sessionState, sessionID string) {
	if st.Seen == nil {
		st.Seen = map[string]int{}
	}
	for _, s := range updateStreams(sessionID) {
		evs, _ := events.Read(paths.EventsDir(), s)
		st.Seen[s] = len(evs)
	}
}

// takeUpdates returns a line per event appended since the session last looked,
// and advances its cursors. A stream the session has no cursor for yet (one
// that began before this hook existed) is baselined silently rather than
// replayed in full; a stream that shrank (rotated or cleared) is re-baselined.
func takeUpdates(st *sessionState, sessionID string) []string {
	if st.Seen == nil {
		st.Seen = map[string]int{}
	}
	var lines []string
	for _, s := range updateStreams(sessionID) {
		evs, _ := events.Read(paths.EventsDir(), s)
		seen, ok := st.Seen[s]
		st.Seen[s] = len(evs)
		if !ok || seen > len(evs) {
			continue
		}
		for _, e := range evs[seen:] {
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s", e.Kind, e.Source, e.Summary))
		}
	}
	if extra := len(lines) - maxUpdateLines; extra > 0 {
		lines = append([]string{fmt.Sprintf("- (%d earlier update(s) not shown)", extra)}, lines[extra:]...)
	}
	return lines
}

func writeUpdates(out io.Writer, lines []string) {
	fmt.Fprintln(out, "## Orchestrator updates")
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	fmt.Fprintln(out)
}

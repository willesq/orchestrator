package hook

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/redact"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// updateStreams are the event streams surfaced mid-session: the session's own
// stream, where its finished subagents report, and "queue", where background
// tasks report. The PR babysitter's stream refreshes every few minutes and stays
// in the SessionStart digest only.
func updateStreams(sessionID string) []string {
	// Reason: a session named like a shared stream ("spaces") must not
	// read it as its own; the spaces stream is the operator's.
	if !sessionIDPattern.MatchString(sessionID) || sharedStreams[sessionID] {
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
	var shows func(events.Event) bool
	for _, s := range updateStreams(sessionID) {
		evs, _ := events.Read(paths.EventsDir(), s)
		seen, ok := st.Seen[s]
		st.Seen[s] = len(evs)
		if !ok || seen > len(evs) {
			continue
		}
		fresh := evs[seen:]
		if s != sessionID && len(fresh) > 0 {
			// Reason: the guard files are read only when a shared stream
			// has something new to show.
			if shows == nil {
				shows = eventFilter(sessionID)
			}
			fresh = sharedEvents(s, sessionID, fresh, shows)
		}
		for _, e := range fresh {
			lines = append(lines, eventLine(e))
		}
	}
	if extra := len(lines) - maxUpdateLines; extra > 0 {
		lines = append([]string{fmt.Sprintf("- (%d earlier update(s) not shown)", extra)}, lines[extra:]...)
	}
	return lines
}

// spaceFreeStreams are shared streams of orchd's own state, which belong
// to no space: model routing changes. Every other shared stream's events
// carry the space of their subject (SP-026).
var spaceFreeStreams = map[string]bool{"routing": true}

// eventFilter returns whether an event of a shared stream may reach session
// sessionID's context: one placed in a space the session reads, by orchd's
// guard files (SP-026, INV-01). With no policy applied every event shows
// (INV-10); a policy that cannot be read shows none.
func eventFilter(sessionID string) func(events.Event) bool {
	ctx, cancel := context.WithTimeout(context.Background(), enforcementTimeout)
	defer cancel()
	reads := guardFilesFor().SessionReads(ctx, sessionID)
	return func(e events.Event) bool { return reads.Shows(e.Space) }
}

// sharedEvents keeps of stream's events those session sessionID may see:
// all of its own stream and of a space-free stream, and of another shared
// stream those shows passes.
func sharedEvents(stream, sessionID string, evs []events.Event, shows func(events.Event) bool) []events.Event {
	if stream == sessionID || spaceFreeStreams[stream] {
		return evs
	}
	out := evs[:0:0]
	for _, e := range evs {
		if shows(e) {
			out = append(out, e)
		}
	}
	return out
}

func writeUpdates(out io.Writer, lines []string) {
	fmt.Fprintln(out, "## Orchestrator updates")
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	fmt.Fprintln(out)
}

// maxEventLine caps one event's line in a digest or update block.
const maxEventLine = 300

// eventLine renders an event as one digest line. Event streams are files any
// process running as the user can append to, and the line lands in a session's
// context, so it is flattened to a single line, redacted, and capped: an event
// cannot open a section of its own or smuggle a secret into the transcript.
func eventLine(e events.Event) string {
	line := fmt.Sprintf("- [%s] %s: %s", e.Kind, e.Source, e.Summary)
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, line)
	line, _ = redact.Redact(line)
	if r := []rune(line); len(r) > maxEventLine {
		line = string(r[:maxEventLine-1]) + "…"
	}
	return line
}

// enforcementTimeout bounds asking orchd for the enforcement switches at
// SessionStart, inside the start hook's budget.
const enforcementTimeout = 500 * time.Millisecond

// enforcementNotice is a digest line per enforcement layer that does not
// enforce: switched off by the operator, the guard only logging, or the
// tool hooks gone though their switch is on; each names the layer and since
// when (SP-070, INV-16). It asks orchd, never a file a session could write
// alone. When orchd does not answer it reads orch.db's switches read-only,
// so a stopped orchd cannot hide a layer the operator switched off, and
// says when it cannot tell at all. It also returns the sandbox setting as
// orchd, or else orch.db, gives it (D-68), nil when unknown.
func enforcementNotice(cf clientFactory) ([]string, *sandboxView) {
	if rep, client, cleanup, ok := askEnforcement(cf); ok {
		defer cleanup()
		var lines []string
		for _, sw := range rep.GetSwitches() {
			if sw.GetRelaxed() {
				lines = append(lines, switchLine(sw, ""))
			}
		}
		if e := rep.GetError(); e != "" {
			lines = append(lines, noticeLine(e))
		}
		return lines, sandboxFromOrchd(client, rep)
	}
	ctx, cancel := context.WithTimeout(context.Background(), enforcementTimeout)
	defer cancel()
	sw, err := spaces.ReadEnforcement(ctx, paths.DBFile())
	if sw == nil {
		return []string{noticeLine("orchd did not answer and orch.db's enforcement switches could not be read, so a layer may be switched off; check 'orch spaces enforce'")}, sandboxFromDB()
	}
	var lines []string
	for _, s := range sw.At(time.Now()).Relaxed() {
		lines = append(lines, switchLine(&orchdv1.EnforcementSwitch{
			Layer: string(s.Layer), Title: s.Layer.Title(), State: s.State, SetAt: unixOrZero(s.SetAt),
			Until: unixOrZero(s.Until), RevertsTo: s.Previous,
		}, " (orchd did not answer; read from orch.db)"))
	}
	if err != nil {
		lines = append(lines, noticeLine(err.Error()+"; those layers enforce"))
	}
	return lines, sandboxFromDB()
}

// askEnforcement asks orchd for the switches; ok is false when it did not
// answer. When ok, the caller closes the client with cleanup.
func askEnforcement(cf clientFactory) (*orchdv1.EnforcementState, orchdv1.OrchdClient, func(), bool) {
	client, cleanup, err := cf()
	if err != nil {
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), enforcementTimeout)
	defer cancel()
	rep, err := client.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
	if err != nil || rep == nil {
		cleanup()
		return nil, nil, nil, false
	}
	return rep, client, cleanup, true
}

// switchLine is one layer that does not enforce, flattened and capped like
// an event line.
func switchLine(sw *orchdv1.EnforcementSwitch, suffix string) string {
	what := "is switched off"
	switch {
	case sw.GetProblem() != "":
		// Capped so the line's end survives eventLine's cap.
		what = "does not enforce: " + capRunes(sw.GetProblem(), 160)
	case sw.GetState() != "off":
		what = "only logs (" + sw.GetState() + ")"
	}
	line := fmt.Sprintf("%s (%s) %s", sw.GetLayer(), sw.GetTitle(), what)
	if at := sw.GetSetAt(); at > 0 {
		line += " since " + time.Unix(at, 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	if u := sw.GetUntil(); u > 0 {
		line += "; it reverts to " + sw.GetRevertsTo() + " at " + time.Unix(u, 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	line += "; spaces does not enforce this layer meanwhile" + suffix
	return noticeLine(line)
}

// noticeLine is an enforcement notice as a digest line.
func noticeLine(s string) string {
	return eventLine(events.Event{Kind: "enforcement", Source: "orchd", Summary: s})
}

// capRunes cuts s to at most n runes, marking a cut.
func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

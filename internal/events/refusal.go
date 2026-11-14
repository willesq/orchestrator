package events

import (
	"fmt"
	"regexp"
	"time"
)

// Refusal events (spaces plan SP-048): each tool call the spaces guard or
// the outbound terms check refuses is reported on the operator's stream,
// so a refusal is visible without reading the command log. The event names
// the call, never what it holds: no command, path, repository or term.

// SpacesStream is the operator's spaces stream: enforcement switch changes
// and refusals. Sessions never read it (their digests read their own
// stream, "queue", "pr-babysitter" and "routing").
const SpacesStream = "spaces"

// KindRefusal is the kind of a refusal event.
const KindRefusal = "refusal"

// Refusal is what a refusal event says about the refused call.
type Refusal struct {
	// Session is the session the harness named; "" when it named none
	// a stream could hold.
	Session string `json:"session,omitempty"`
	// Space is the space the session is pinned to; "" when the guard had
	// not read a pin (no pin, or it refused before reading one).
	Space string `json:"space"`
	// Tool is the harness's tool name, such as Bash or Write.
	Tool string `json:"tool"`
	// Rule is the guard's or the terms check's rule id.
	Rule string `json:"rule"`
	// Term is the term an outbound_term refusal found, by its place in
	// a terms file; nil for every other rule.
	Term *TermRef `json:"term,omitempty"`
}

// TermRef names a term by its space, line and hash, never by its text
// (INV-07).
type TermRef struct {
	Space string `json:"space"`
	Line  int    `json:"line"`
	Hash  string `json:"hash"`
}

// The shapes a refusal's fields may take. Every field comes from a
// harness payload or a file another process wrote, and the event reaches
// the app and the operator's terminal, so anything else is replaced
// rather than stored.
var (
	refusalSession = streamName
	refusalSpace   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	refusalTool    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,127}$`)
	refusalRule    = regexp.MustCompile(`^[a-z][a-z_]{0,39}$`)
	refusalHash    = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
)

// NewRefusal returns the event for r refused at at. A field not of its
// expected shape is replaced (or dropped, for the session and the term),
// so no free text reaches the stream.
func NewRefusal(r Refusal, at time.Time) Event {
	if !refusalSession.MatchString(r.Session) {
		r.Session = ""
	}
	if !refusalSpace.MatchString(r.Space) {
		r.Space = ""
	}
	if !refusalTool.MatchString(r.Tool) {
		r.Tool = "unknown"
	}
	if !refusalRule.MatchString(r.Rule) {
		r.Rule = "unknown"
	}
	if t := r.Term; t != nil {
		if !refusalSpace.MatchString(t.Space) || t.Line < 1 || !refusalHash.MatchString(t.Hash) {
			r.Term = nil
		} else {
			r.Term = &TermRef{Space: t.Space, Line: t.Line, Hash: t.Hash}
		}
	}
	return Event{Kind: KindRefusal, Source: "guard", Summary: r.summary(), Timestamp: at.UTC().Format(time.RFC3339), Refusal: &r}
}

// summary is the refusal as one line, from the checked fields only.
func (r Refusal) summary() string {
	space := "no space"
	if r.Space != "" {
		space = "space " + r.Space
	}
	s := fmt.Sprintf("refused a %s call (rule %s) in %s", r.Tool, r.Rule, space)
	if r.Session != "" {
		s += ", session " + r.Session
	}
	if t := r.Term; t != nil {
		s += fmt.Sprintf("; a term of space %s, terms file line %d (hash %s)", t.Space, t.Line, t.Hash)
	}
	return s
}

// AppendSpaces appends e to dir's spaces stream, the way every writer of
// that stream must. The stream is made readable by its owner only:
// refusals name sessions and spaces, switch changes name callers.
func AppendSpaces(dir string, e Event) error {
	return appendMode(dir, SpacesStream, e, 0o600)
}

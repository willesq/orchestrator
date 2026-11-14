package daemon

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// Outbound terms never reach the command log (spaces plan SP-044, INV-07):
// before a record is built, every space's terms are replaced with [term]
// in its command (the tool input decoded, so a term after an escape is
// found), its targets and its directories. When the policy or a terms file
// cannot be read the terms are not known, so those fields are withheld.

// withheldTerms replaces a text field while the terms cannot be read.
const withheldTerms = "[withheld: the outbound terms cannot be read]"

// termsCache keeps the parsed terms until the policy or a terms file
// changes; the log's writer goroutine reads it for every record.
type termsCache struct {
	mu     sync.Mutex
	key    string
	lists  []spaces.TermList
	broken bool
}

// terms returns the terms of res's spaces, and whether one could not be
// read.
func (c *termsCache) terms(res *spaces.Resolved, home string) ([]spaces.TermList, bool) {
	key := termsKey(res, home)
	c.mu.Lock()
	defer c.mu.Unlock()
	if key != c.key {
		lists, errs := spaces.SpaceTerms(res, home)
		c.key, c.lists, c.broken = key, lists, len(errs) > 0
	}
	return c.lists, c.broken
}

// termsKey names the terms files' current state: path, size and time of
// each, so an edited file is read again.
func termsKey(res *spaces.Resolved, home string) string {
	paths := spaces.TermsPaths(res, home)
	names := make([]string, 0, len(paths))
	for n := range paths {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s=%s;", n, paths[n])
		if st, err := os.Stat(paths[n]); err == nil {
			fmt.Fprintf(&b, "%d:%d;", st.Size(), st.ModTime().UnixNano())
		}
	}
	return b.String()
}

// termsView is how one record's text is redacted.
type termsView struct {
	lists []spaces.TermList
	// hidden is set when the terms cannot be known: text fields are
	// withheld.
	hidden bool
}

// termsView reads the current terms.
func (s *server) termsView() termsView {
	if s.spaces == nil {
		return termsView{}
	}
	res, err := s.spaces.Current()
	switch {
	case err != nil:
		return termsView{hidden: true}
	case !res.Configured():
		return termsView{}
	}
	lists, broken := s.terms.terms(res, s.home)
	return termsView{lists: lists, hidden: broken}
}

func (v termsView) text(s string) string {
	if len(v.lists) == 0 {
		return s
	}
	return spaces.RedactTerms(s, v.lists)
}

func (v termsView) json(doc []byte) []byte {
	if len(v.lists) == 0 {
		return doc
	}
	return spaces.RedactJSON(doc, v.lists)
}

func (v termsView) targets(t spaces.ToolTargets) spaces.ToolTargets {
	if len(v.lists) == 0 {
		return t
	}
	return spaces.RedactTargets(t, v.lists)
}

// withhold replaces r's text fields when the terms cannot be known.
func (v termsView) withhold(r *toollog.Record) {
	if !v.hidden {
		return
	}
	for _, f := range []*string{&r.Command, &r.Targets, &r.Cwd, &r.OldCwd, &r.NewCwd} {
		if *f != "" {
			*f = withheldTerms
		}
	}
}

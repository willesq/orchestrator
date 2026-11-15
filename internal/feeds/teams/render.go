package teams

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/willesq/orchestrator/internal/feeds"
)

// visible reports whether m is a message someone wrote that still exists:
// not a system event (member added, call started) and not deleted.
func visible(m message) bool {
	return (m.MessageType == "" || m.MessageType == "message") && m.DeletedDateTime == nil
}

var (
	breakTag = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li)\b[^>]*>`)
	anyTag   = regexp.MustCompile(`<[^>]*>`)
	spaces   = regexp.MustCompile(`[ \t]+`)
	blank    = regexp.MustCompile(`\n{3,}`)
	// lineBreaks are the characters some reader may break a line at.
	lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
)

// plain is m's body as plain text: Teams sends most bodies as HTML.
func plain(m message) string {
	s := m.Body.Content
	if strings.EqualFold(m.Body.ContentType, "html") {
		s = breakTag.ReplaceAllString(s, "\n")
		s = anyTag.ReplaceAllString(s, "")
		s = html.UnescapeString(s)
	}
	// Reason: every line break becomes "\n", which conversation indents,
	// so none can start a line that poses as another message.
	s = lineBreaks.Replace(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = spaces.ReplaceAllString(s, " ")
	s = blank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// sender is who wrote m: the user's display name, or their ID without one.
func sender(m message) (id, name string) {
	if m.From == nil || m.From.User == nil {
		return "", "unknown"
	}
	u := m.From.User
	name = strings.TrimSpace(u.DisplayName)
	if name == "" {
		name = u.ID
	}
	return u.ID, name
}

// oneLine is s with control characters and line breaks made spaces.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

// conversation is the record for msgs, the visible messages of one
// conversation, in the order they were written. ok is false when none has
// text.
func conversation(base feeds.Record, msgs []message) (feeds.Record, bool) {
	msgs = slices.DeleteFunc(slices.Clone(msgs), func(m message) bool { return !visible(m) || plain(m) == "" })
	if len(msgs) == 0 {
		return feeds.Record{}, false
	}
	slices.SortStableFunc(msgs, func(a, b message) int { return a.CreatedDateTime.Compare(b.CreatedDateTime) })
	r := base
	var b strings.Builder
	var parts []string
	for _, m := range msgs {
		id, name := sender(m)
		if id != "" && validID(id) {
			parts = append(parts, id)
		}
		// Reason: a name or a message with line breaks could otherwise
		// forge a line that reads as another person's message.
		body := strings.ReplaceAll(plain(m), "\n", "\n  ")
		b.WriteString("[" + m.CreatedDateTime.UTC().Format("2006-01-02 15:04 MST") + "] " + oneLine(name) + ": " + body + "\n")
		if m.LastModifiedDateTime.After(r.EndedAt) {
			r.EndedAt = m.LastModifiedDateTime
		}
	}
	slices.Sort(parts)
	r.Participants = slices.Compact(parts)
	r.Text = b.String()
	r.StartedAt = msgs[0].CreatedDateTime
	if last := msgs[len(msgs)-1].CreatedDateTime; last.After(r.EndedAt) {
		r.EndedAt = last
	}
	return r, true
}

// day is the UTC calendar day t falls on, as a chat's conversation ID.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// dayStart is the start of the UTC day d (as day returns it).
func dayStart(d string) time.Time {
	t, _ := time.Parse("2006-01-02", d)
	return t
}

// hash is what dedupes a conversation: its text and participants. A record
// whose hash is unchanged since it was stored is not emitted again.
func hash(r feeds.Record) string {
	h := sha256.New()
	h.Write([]byte(r.Text))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(r.Participants, ",")))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

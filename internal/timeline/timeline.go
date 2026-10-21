// Package timeline dates stored text. A transcript is stored with each message
// prefixed by a marker such as "[2026-10-21T14:02Z]", so its text dates itself
// and every chunk cut from it can be given the span of time it covers. The
// marker is one word, so chunking, which splits between words, never cuts it.
package timeline

import (
	"regexp"
	"strings"
	"time"
)

const (
	layout       = "2006-01-02 15:04"
	markerLayout = "2006-01-02T15:04Z"
)

var marker = regexp.MustCompile(`\[(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}Z)\]`)

// Marker is the prefix that dates a message, in UTC to the minute.
func Marker(t time.Time) string { return "[" + t.UTC().Format(markerLayout) + "]" }

// Neutralize rewrites marker-shaped strings in text written by someone else,
// such as a message quoting a web page, from [..] to (..), so only the markers
// a transcript's writer adds can date it.
func Neutralize(text string) string {
	return marker.ReplaceAllStringFunc(text, func(m string) string {
		return "(" + m[1:len(m)-1] + ")"
	})
}

// Span returns the earliest and latest marked times in text; ok is false when
// it holds none.
func Span(text string) (first, last time.Time, ok bool) {
	for _, m := range marker.FindAllStringSubmatch(text, -1) {
		t, err := time.Parse(markerLayout, m[1])
		if err != nil {
			continue
		}
		if !ok || t.Before(first) {
			first = t
		}
		if !ok || t.After(last) {
			last = t
		}
		ok = true
	}
	return first, last, ok
}

// Range describes a span of time as "2026-10-21 14:02 to 15:40 UTC", naming
// the second day only when it differs.
func Range(start, end time.Time) string {
	start, end = start.UTC(), end.UTC()
	switch {
	case start.IsZero() && end.IsZero():
		return "unknown"
	case start.IsZero():
		return end.Format(layout) + " UTC"
	case end.IsZero() || end.Equal(start):
		return start.Format(layout) + " UTC"
	case start.Format("2006-01-02") == end.Format("2006-01-02"):
		return start.Format(layout) + " to " + end.Format("15:04") + " UTC"
	}
	return start.Format(layout) + " to " + end.Format(layout) + " UTC"
}

// Describe labels when stored content happened, from Unix seconds: its span,
// "date unknown" for a session stored before sessions were dated, and "" for
// other undated content, which never had a date to show.
func Describe(start, end int64, sourceType string) string {
	if start > 0 || end > 0 {
		return Range(fromUnix(start), fromUnix(end))
	}
	if strings.HasPrefix(sourceType, "session_") {
		return "date unknown"
	}
	return ""
}

func fromUnix(s int64) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

package rag

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Payload fields that date a chunk, in Unix seconds. started_at and ended_at
// span what the chunk describes (the turns of a transcript chunk, or a whole
// session for its record); ingested_at is when it was stored.
const (
	fieldStartedAt  = "started_at"
	fieldEndedAt    = "ended_at"
	fieldIngestedAt = "ingested_at"
)

// TimeRange limits results to content dated within it. A zero end is open.
// Content without a date never matches a range, since it cannot be placed.
type TimeRange struct {
	Since, Until time.Time
}

// IsZero reports whether the range limits nothing.
func (r TimeRange) IsZero() bool { return r.Since.IsZero() && r.Until.IsZero() }

// conditions match chunks whose span overlaps the range.
func (r TimeRange) conditions() []any {
	var conds []any
	if !r.Since.IsZero() {
		conds = append(conds, map[string]any{"key": fieldEndedAt, "range": map[string]any{"gte": r.Since.Unix()}})
	}
	if !r.Until.IsZero() {
		conds = append(conds, map[string]any{"key": fieldStartedAt, "range": map[string]any{"lte": r.Until.Unix()}})
	}
	return conds
}

// maxAgeDays bounds an age such as 7d: a century back is already before
// anything stored.
const maxAgeDays = 36500

// ParseBound reads one end of a range: an RFC 3339 time; a UTC date
// (2026-10-21), whose end is that day's last second when end is set; "today"
// or "yesterday" in UTC, likewise; or an age such as 7d or 12h before now.
func ParseBound(s string, now time.Time, end bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if t.Before(time.Unix(1, 0)) {
			return time.Time{}, fmt.Errorf("%q is before 1970", s)
		}
		return t.UTC(), nil
	}
	day := func(d time.Time) time.Time {
		d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		if end {
			return d.Add(24*time.Hour - time.Second)
		}
		return d
	}
	now = now.UTC()
	switch strings.ToLower(s) {
	case "today":
		return day(now), nil
	case "yesterday":
		return day(now.AddDate(0, 0, -1)), nil
	}
	if d, err := time.Parse("2006-01-02", s); err == nil && d.Year() >= 1970 {
		return day(d), nil
	}
	// Ages are capped at a century, far below where a Duration overflows.
	if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n >= 0 {
		switch s[len(s)-1] {
		case 'd':
			if n <= maxAgeDays {
				return now.AddDate(0, 0, -n), nil
			}
		case 'h':
			if n <= maxAgeDays*24 {
				return now.Add(-time.Duration(n) * time.Hour), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use 2026-10-21, an RFC 3339 time, today, yesterday, or an age like 7d or 12h", s)
}

// Stored times outside [minStored, now+maxAhead] are treated as unknown: a
// corrupt or hostile payload must not stretch a span to 1970 or 9999 and so
// defeat the bounds built from it.
const (
	minStored = 946684800 // 2000-01-01
	maxAhead  = 48 * time.Hour
)

// unixTime reads a Unix-seconds payload field; zero when absent or implausible.
func unixTime(v any) time.Time {
	f, ok := v.(float64)
	if !ok || f < minStored || f > float64(time.Now().Add(maxAhead).Unix()) {
		return time.Time{}
	}
	return time.Unix(int64(f), 0).UTC()
}

package rag

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/timeline"
)

func TestParseBound(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 30, 0, 0, time.UTC)
	day := func(d int, h, m, s int) time.Time { return time.Date(2026, 10, d, h, m, s, 0, time.UTC) }
	for _, tt := range []struct {
		in   string
		end  bool
		want time.Time
	}{
		{"", false, time.Time{}},
		{"2026-10-20", false, day(20, 0, 0, 0)},
		{"2026-10-20", true, day(20, 23, 59, 59)},
		{"today", false, day(21, 0, 0, 0)},
		{"Yesterday", true, day(20, 23, 59, 59)},
		{"7d", false, now.AddDate(0, 0, -7)},
		{"12h", false, now.Add(-12 * time.Hour)},
		{"36500d", false, now.AddDate(0, 0, -36500)},
		{"2026-10-20T08:00:00-07:00", false, day(20, 15, 0, 0)},
	} {
		got, err := ParseBound(tt.in, now, tt.end)
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("ParseBound(%q, end=%v) = %v, %v; want %v", tt.in, tt.end, got, err, tt.want)
		}
	}
	// Out-of-range ages and pre-1970 times would otherwise overflow or wrap.
	for _, bad := range []string{"last week", "-3d", "7w", "d", "2026-13-01", "36501d", "876001h",
		"9223372036854775807d", "1969-12-31", "1969-12-31T23:00:00Z"} {
		if _, err := ParseBound(bad, now, false); err == nil {
			t.Errorf("ParseBound(%q) accepted", bad)
		}
	}
}

func TestTimeRangeConditions(t *testing.T) {
	if !(TimeRange{}).IsZero() || len((TimeRange{}).conditions()) != 0 {
		t.Fatal("an empty range limits something")
	}
	r := TimeRange{Since: time.Unix(100, 0), Until: time.Unix(200, 0)}
	conds := r.conditions()
	// Overlap: ends at or after since, starts at or before until.
	if len(conds) != 2 || !strings.Contains(strings.Join([]string{str(conds[0].(map[string]any)["key"]), str(conds[1].(map[string]any)["key"])}, ","), "ended_at,started_at") {
		t.Fatalf("conditions = %v", conds)
	}
}

// datedService ingests into a fake Qdrant with one-word chunks.
func datedService(t *testing.T) (*Service, *fakeQdrant) {
	t.Helper()
	f, url := newFakeQdrant(t)
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "c", ""), 12, 0)
	return svc, f
}

func TestTranscriptChunksAreDatedByTheirTurns(t *testing.T) {
	svc, f := datedService(t)
	day1 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 21, 16, 45, 0, 0, time.UTC)
	long := strings.Repeat("word ", 40) // spans several chunks with no marker of its own
	text := timeline.Marker(day1) + " user: start the migration\n\n" +
		timeline.Marker(day1.Add(5*time.Minute)) + " assistant: " + long + "\n\n" +
		timeline.Marker(day2) + " user: what did we do yesterday\n\n"
	before := time.Now().Add(-time.Second)
	if _, err := svc.IngestText(context.Background(), text, Meta{SourceURI: "session://s/transcript", SourceType: "session_transcript", Timeline: true}); err != nil {
		t.Fatal(err)
	}
	c := f.collection("c")
	var sawDay1, sawDay2, carried int
	for _, p := range c.points {
		start, end := unixTime(p.Payload[fieldStartedAt]), unixTime(p.Payload[fieldEndedAt])
		if start.IsZero() || end.Before(start) {
			t.Fatalf("chunk %q dated %v to %v", p.Payload["text"], start, end)
		}
		if unixTime(p.Payload[fieldIngestedAt]).Before(before) {
			t.Fatalf("chunk not stamped with its ingest time: %v", p.Payload)
		}
		chunk := str(p.Payload["text"])
		switch {
		case strings.Contains(chunk, "yesterday"):
			sawDay2++
			if !end.Equal(day2) {
				t.Fatalf("the day-2 turn ends %v", end)
			}
		case !strings.Contains(chunk, "Z]") && strings.Contains(chunk, "word"):
			carried++ // inside the long turn: dated by the turn it continues
			if !start.Equal(day1.Add(5 * time.Minute)) {
				t.Fatalf("mid-turn chunk dated %v", start)
			}
		default:
			sawDay1++
		}
	}
	if sawDay1 == 0 || sawDay2 == 0 || carried == 0 {
		t.Fatalf("day1=%d day2=%d carried=%d", sawDay1, sawDay2, carried)
	}

	// A range for the second day finds only that day's part of the session.
	since := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	onDay2 := 0
	for _, p := range c.points {
		if !unixTime(p.Payload[fieldEndedAt]).Before(since) {
			onDay2++
		}
	}
	srcs, _, chunks, err := svc.ListSources(context.Background(), ListSourcesOpts{Range: TimeRange{Since: since}})
	if err != nil || len(srcs) != 1 || chunks != onDay2 || onDay2 >= len(c.points) {
		t.Fatalf("day-2 sources = %+v, chunks %d, %v", srcs, chunks, err)
	}
	all, _, _, _ := svc.ListSources(context.Background(), ListSourcesOpts{})
	if !all[0].StartedAt.Equal(day1) || !all[0].EndedAt.Equal(day2) || all[0].IngestedAt.Before(before) {
		t.Fatalf("source span = %+v", all[0])
	}
}

func TestSourceDatesAndUndatedContent(t *testing.T) {
	svc, f := datedService(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	if _, err := svc.IngestText(ctx, "the session summary", Meta{SourceURI: "session://s/record", SourceType: "session_record",
		StartedAt: start, EndedAt: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestText(ctx, "a plain note", Meta{SourceURI: "manual://n"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c").points {
		if str(p.Payload["source_uri"]) == "manual://n" {
			stored := unixTime(p.Payload[fieldIngestedAt])
			if stored.IsZero() {
				t.Fatal("every chunk records when it was stored")
			}
			// A note with no date of its own is dated by when it was stored.
			if !unixTime(p.Payload[fieldStartedAt]).Equal(stored) || !unixTime(p.Payload[fieldEndedAt]).Equal(stored) {
				t.Fatalf("note dated %v to %v, want its ingest time %v", p.Payload[fieldStartedAt], p.Payload[fieldEndedAt], stored)
			}
		}
	}
	// A range over the session finds only it; one over today finds the note.
	ranged, _, _, err := svc.ListSources(ctx, ListSourcesOpts{Range: TimeRange{Until: start.Add(time.Minute)}})
	if err != nil || len(ranged) != 1 || ranged[0].SourceURI != "session://s/record" {
		t.Fatalf("ranged = %+v, %v", ranged, err)
	}
	today, _, _, err := svc.ListSources(ctx, ListSourcesOpts{Range: TimeRange{Since: time.Now().Add(-time.Hour)}})
	if err != nil || len(today) != 1 || today[0].SourceURI != "manual://n" {
		t.Fatalf("today = %+v, %v", today, err)
	}
	if all, _, _, _ := svc.ListSources(ctx, ListSourcesOpts{}); len(all) != 2 {
		t.Fatalf("all = %+v", all)
	}
	hits, _, err := svc.Search(ctx, "summary", SearchOpts{Mode: ModeDense})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.SourceURI == "session://s/record" && (!h.StartedAt.Equal(start) || !h.EndedAt.Equal(start.Add(time.Hour))) {
			t.Fatalf("hit = %+v", h)
		}
	}
}

func TestTimelineChunksStayInsideTheSession(t *testing.T) {
	svc, f := datedService(t)
	start := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	// Raw markers that slipped past neutralizing are bounded by the session.
	text := timeline.Marker(start) + " user: [1970-01-01T00:00Z] [9999-12-31T23:59Z]\n\n" + strings.Repeat("word ", 30)
	if _, err := svc.IngestText(context.Background(), text, Meta{SourceURI: "session://s/transcript", Timeline: true,
		StartedAt: start, EndedAt: end}); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c").points {
		s, e := unixTime(p.Payload[fieldStartedAt]), unixTime(p.Payload[fieldEndedAt])
		if s.Before(start) || e.After(end) {
			t.Fatalf("chunk %q dated %v to %v, outside the session", p.Payload["text"], s, e)
		}
	}
	if s, e := clamp(time.Time{}, end, start, time.Time{}); !s.IsZero() || !e.Equal(end) {
		t.Fatal("clamp changed an unknown time or an open bound")
	}
}

func TestUnixTimeIgnoresImplausibleValues(t *testing.T) {
	good := float64(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).Unix())
	if unixTime(good).Unix() != int64(good) {
		t.Fatal("a plausible time was rejected")
	}
	for _, bad := range []any{float64(1), float64(minStored - 1), float64(time.Now().Add(72 * time.Hour).Unix()), 1e300, "1792591320", int64(1792591320), nil} {
		if !unixTime(bad).IsZero() {
			t.Errorf("unixTime(%v) accepted", bad)
		}
	}
}

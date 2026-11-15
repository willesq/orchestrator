package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
)

// State is what the connector remembers between runs, per conversation:
// watermarks and content hashes, never content. orchd keeps it; the
// connector process gets it with each request and reports each
// conversation's new state once all of its records were handed over.
type State struct {
	Channels map[string]ChannelState `json:"channels,omitempty"`
}

// ChannelState is one conversation's watermarks.
type ChannelState struct {
	// Kind is the conversation's container kind, so the state of DMs can
	// be dropped when DMs are turned off.
	Kind feeds.ContainerKind `json:"kind,omitempty"`
	// Latest is the newest top-level message stored (a Slack ts).
	Latest string `json:"latest,omitempty"`
	// Threads maps a thread watched for replies to its newest reply
	// stored. Threads older than the lookback are forgotten.
	Threads map[string]string `json:"threads,omitempty"`
	// Hashes maps a record's thread (a thread ts or a day) to the SHA-256
	// of the text last handed over, so an unchanged one is not sent again.
	Hashes map[string]string `json:"hashes,omitempty"`
}

const (
	// DefaultLookback is how long a thread is watched for new replies.
	DefaultLookback = 14 * 24 * time.Hour
	// DefaultHistory is how far back a conversation's first sync reads.
	DefaultHistory = 90 * 24 * time.Hour
	// maxRecordText caps one record's text; later messages are left out
	// and counted.
	maxRecordText = 4 << 20
	// maxMessageText caps one message's text in a record (Slack's own
	// limit is 40,000 characters).
	maxMessageText = 64 << 10
)

// noise is message subtypes that carry no conversation.
var noise = map[string]bool{
	"channel_join": true, "channel_leave": true, "group_join": true, "group_leave": true,
	"channel_topic": true, "channel_purpose": true, "channel_name": true, "channel_archive": true,
	"channel_unarchive": true, "group_topic": true, "group_purpose": true, "group_name": true,
}

// tsPattern is a Slack message timestamp, e.g. 1700000000.000100.
var tsPattern = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,9}$`)

// parseTS returns ts as a time; ok is false for anything not a Slack ts.
func parseTS(ts string) (time.Time, bool) {
	if !tsPattern.MatchString(ts) {
		return time.Time{}, false
	}
	sec, frac, _ := strings.Cut(ts, ".")
	s, err1 := strconv.ParseInt(sec, 10, 64)
	f, err2 := strconv.ParseInt((frac + "000000000")[:9], 10, 64)
	if err1 != nil || err2 != nil {
		return time.Time{}, false
	}
	return time.Unix(s, f).UTC(), true
}

// formatTS is t as a Slack ts.
func formatTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// after reports whether ts a is later than b; every ts is later than "".
func after(a, b string) bool {
	ta, ok := parseTS(a)
	if !ok {
		return false
	}
	tb, ok := parseTS(b)
	return !ok || ta.After(tb)
}

// day is the UTC day of t, which names the record of a conversation's
// messages outside threads.
func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// fetcher reads one workspace for one request. It runs in the connector
// process.
type fetcher struct {
	api  *client
	req  Request
	self string
	now  time.Time
	out  func(Line) error
}

// run reads the request's conversations in order: DMs and group DMs, then
// the allowlisted private channels, then the allowlisted public ones.
func (f *fetcher) run(ctx context.Context) error {
	convs, err := f.conversations(ctx)
	if err != nil {
		return err
	}
	for _, c := range convs {
		st, err := f.channel(ctx, c)
		if err != nil {
			return err
		}
		if err := f.out(Line{Channel: &ChannelDone{ID: c.ID, State: st}}); err != nil {
			return err
		}
	}
	return nil
}

// kind is c's container kind.
func kind(c conversation) feeds.ContainerKind {
	switch {
	case c.IsIM:
		return feeds.DM
	case c.IsMPIM:
		return feeds.GroupDM
	case c.IsPrivate:
		return feeds.PrivateChannel
	}
	return feeds.PublicChannel
}

func (f *fetcher) conversations(ctx context.Context) ([]conversation, error) {
	var dms, channels []conversation
	if f.req.DirectMessages {
		all, err := f.api.list(ctx, "im,mpim")
		if err != nil {
			return nil, err
		}
		// Reason: only what Slack says is a DM is read as one here; the
		// channels are the allowlist's alone.
		for _, c := range all {
			if kind(c).IsDirectMessage() && feeds.ValidID(c.ID) {
				dms = append(dms, c)
			}
		}
	}
	for _, id := range f.req.Containers {
		c, err := f.api.info(ctx, id)
		if err != nil {
			return nil, err
		}
		if c.ID != id || c.IsArchived {
			continue
		}
		if kind(c).IsDirectMessage() {
			// Reason: DMs follow the tenant's DM switch alone; a DM listed
			// as a container is read only when DMs are on, above.
			continue
		}
		channels = append(channels, c)
	}
	// Private channels before public ones (SP-061: DMs, then private).
	slices.SortStableFunc(channels, func(a, b conversation) int {
		switch {
		case a.IsPrivate == b.IsPrivate:
			return 0
		case a.IsPrivate:
			return -1
		}
		return 1
	})
	var out []conversation
	seen := map[string]bool{}
	for _, c := range append(dms, channels...) {
		if !seen[c.ID] && !c.IsArchived {
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// channel reads one conversation from its watermark and hands over every
// record that changed: each day with a new message outside threads (the
// whole day, so the day's record replaces the one stored), and each thread
// whose newest reply is newer than the one stored (the whole thread, under
// the thread's URI). It returns the conversation's new state.
func (f *fetcher) channel(ctx context.Context, c conversation) (ChannelState, error) {
	old := f.req.State.Channels[c.ID]
	lookback := f.req.Lookback
	if lookback <= 0 {
		lookback = DefaultLookback
	}
	windowStart := f.now.Add(-lookback)
	oldest := ""
	if latest, ok := parseTS(old.Latest); ok {
		// Reason: the day of the watermark is read whole so its record is
		// complete, and the lookback so threads in it show new replies.
		dayStart := time.Date(latest.Year(), latest.Month(), latest.Day(), 0, 0, 0, 0, time.UTC)
		if windowStart.Before(dayStart) {
			oldest = formatTS(windowStart)
		} else {
			oldest = formatTS(dayStart)
		}
	} else {
		history := f.req.History
		if history <= 0 {
			history = DefaultHistory
		}
		oldest = formatTS(f.now.Add(-history))
	}
	msgs, err := f.api.history(ctx, c.ID, oldest)
	if err != nil {
		return ChannelState{}, err
	}
	top := topLevel(msgs)
	st := ChannelState{
		Kind:    kind(c),
		Latest:  old.Latest,
		Threads: maps.Clone(old.Threads),
		Hashes:  maps.Clone(old.Hashes),
	}
	if st.Threads == nil {
		st.Threads = map[string]string{}
	}
	if st.Hashes == nil {
		st.Hashes = map[string]string{}
	}
	byDay := map[string][]message{}
	changed := map[string]bool{}
	for _, m := range top {
		t, _ := parseTS(m.TS)
		d := day(t)
		byDay[d] = append(byDay[d], m)
		if after(m.TS, old.Latest) {
			changed[d] = true
			if after(m.TS, st.Latest) {
				st.Latest = m.TS
			}
		}
	}
	for _, d := range slices.Sorted(maps.Keys(changed)) {
		if err := f.emit(c, d, byDay[d], &st); err != nil {
			return ChannelState{}, err
		}
	}
	for _, m := range top {
		if m.ReplyCount == 0 || (m.ThreadTS != "" && m.ThreadTS != m.TS) || !after(m.LatestReply, old.Threads[m.TS]) {
			continue
		}
		thread, err := f.api.replies(ctx, c.ID, m.TS)
		if err != nil {
			return ChannelState{}, err
		}
		if err := f.emit(c, m.TS, inThread(thread, m.TS), &st); err != nil {
			return ChannelState{}, err
		}
		st.Threads[m.TS] = m.LatestReply
	}
	forget(&st, windowStart)
	return st, nil
}

// topLevel is msgs outside threads (and thread parents), oldest first,
// without noise or messages whose ts is not one.
func topLevel(msgs []message) []message {
	var out []message
	for _, m := range msgs {
		if noise[m.Subtype] || !tsPattern.MatchString(m.TS) {
			continue
		}
		if m.ThreadTS != "" && m.ThreadTS != m.TS && m.Subtype != "thread_broadcast" {
			continue
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b message) int {
		ta, _ := parseTS(a.TS)
		tb, _ := parseTS(b.TS)
		return ta.Compare(tb)
	})
	return out
}

// inThread is a thread's messages, oldest first, without noise.
func inThread(msgs []message, ts string) []message {
	var out []message
	for _, m := range msgs {
		if noise[m.Subtype] || !tsPattern.MatchString(m.TS) || (m.TS != ts && m.ThreadTS != ts) {
			continue
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b message) int {
		ta, _ := parseTS(a.TS)
		tb, _ := parseTS(b.TS)
		return ta.Compare(tb)
	})
	return out
}

// forget drops the threads and hashes older than the lookback, so the state
// stays small; a thread forgotten is no longer watched for replies.
func forget(st *ChannelState, windowStart time.Time) {
	for ts := range st.Threads {
		if t, ok := parseTS(ts); !ok || t.Before(windowStart) {
			delete(st.Threads, ts)
		}
	}
	for key := range st.Hashes {
		if _, watched := st.Threads[key]; watched {
			continue
		}
		if d, err := time.Parse(time.DateOnly, key); err == nil && !d.Before(windowStart.Truncate(24*time.Hour)) {
			continue
		}
		delete(st.Hashes, key)
	}
}

// emit hands over the record of msgs under thread (a thread ts or a day),
// unless its text is what was handed over last time.
func (f *fetcher) emit(c conversation, thread string, msgs []message, st *ChannelState) error {
	if len(msgs) == 0 {
		return nil
	}
	rec := f.record(c, thread, msgs)
	sum := sha256.Sum256([]byte(rec.Text))
	h := hex.EncodeToString(sum[:])
	if st.Hashes[thread] == h {
		return nil
	}
	if err := f.out(Line{Record: &rec}); err != nil {
		return err
	}
	st.Hashes[thread] = h
	return nil
}

// record renders msgs as one feeds.Record.
func (f *fetcher) record(c conversation, thread string, msgs []message) feeds.Record {
	k := kind(c)
	parts := map[string]bool{}
	if k.IsDirectMessage() && f.self != "" {
		parts[f.self] = true
	}
	if feeds.ValidID(c.User) {
		parts[c.User] = true
	}
	var b strings.Builder
	first, _ := parseTS(msgs[0].TS)
	last, _ := parseTS(msgs[len(msgs)-1].TS)
	b.WriteString(header(c, k, thread, first))
	for i, m := range msgs {
		if b.Len() > maxRecordText {
			fmt.Fprintf(&b, "(%d later messages left out: the record is too long)\n", len(msgs)-i)
			break
		}
		who := author(m)
		if feeds.ValidID(who) {
			parts[who] = true
		} else {
			who = "unknown"
		}
		t, _ := parseTS(m.TS)
		// Reason: a message's own line breaks are indented, so its text
		// cannot add a line that reads as another author's message.
		text := m.Text
		if len(text) > maxMessageText {
			text = text[:maxMessageText] + " (cut)"
		}
		text = strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(text, ""), "\r", ""), "\n", "\n    ")
		if m.ReplyCount > 0 && m.TS != thread {
			text += fmt.Sprintf(" (thread: %d replies)", m.ReplyCount)
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", t.Format("2006-01-02 15:04 UTC"), who, text)
	}
	return feeds.Record{
		System:       feeds.Slack,
		Tenant:       f.req.Tenant,
		Container:    c.ID,
		Kind:         k,
		Thread:       thread,
		Participants: slices.Sorted(maps.Keys(parts)),
		Text:         b.String(),
		StartedAt:    first,
		EndedAt:      last,
	}
}

// author is who wrote m: a user, else a bot.
func author(m message) string {
	if m.User != "" {
		return m.User
	}
	return m.BotID
}

// header names the conversation a record is from.
func header(c conversation, k feeds.ContainerKind, thread string, first time.Time) string {
	where := "Slack " + strings.ReplaceAll(string(k), "_", " ")
	if c.Name != "" && !k.IsDirectMessage() {
		where += " #" + strings.Join(strings.Fields(strings.ToValidUTF8(c.Name, "")), "-")
	}
	where += " (" + c.ID + ")"
	if _, isTS := parseTS(thread); isTS {
		return fmt.Sprintf("%s, thread started %s\n", where, first.Format("2006-01-02 15:04 UTC"))
	}
	return fmt.Sprintf("%s, messages of %s\n", where, thread)
}

package teams

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
)

// Connector reads Microsoft Teams through Graph for feeds.Sync. It reads
// messages only from the conversations Allow names; the rest it lists by
// ID and kind alone, to know what Allow covers.
//
// Conversations: a channel thread (its root message and replies) is one
// record, with the root's ID as its thread; a chat has no threads, so each
// UTC day of a chat is one record, with the day (2026-11-15) as its thread.
// A thread or day that grows is emitted again whole under the same URI.
//
// Watermarks: per container, the newest change stored. A run reads a chat's
// messages changed since it (Graph's lastModifiedDateTime filter), and a
// channel's threads whose latest activity is at or after it. A record whose
// text and participants hash the same as when it was stored is not emitted
// again. The watermark advances, and the state is saved, only after every
// record of the container was emitted without error.
type Connector struct {
	Graph Graph
	// Tenant is the ID of the tenant the Graph login belongs to
	// (AzRest.Status). Only that tenant's chats and teams are read.
	Tenant string
	// Allow is the allowlist feeds.Sync is given: the connector reads no
	// message of a conversation it does not allow.
	Allow feeds.Allowlist
	// State is what earlier runs stored; nil starts empty in memory.
	State *State
}

// System is feeds.Teams.
func (c *Connector) System() feeds.System { return feeds.Teams }

// chatKinds maps a Graph chatType to a container kind; other types
// (unknownFutureValue) are not read.
var chatKinds = map[string]feeds.ContainerKind{
	"oneOnOne": feeds.DM,
	"group":    feeds.GroupDM,
	"meeting":  feeds.Meeting,
}

// pageSize is the most messages Graph returns in a page ($top).
const pageSize = "50"

// ErrNoTenant refuses a run whose connector does not know its login's
// tenant.
var ErrNoTenant = errors.New("teams: the connector has no tenant; set it from the profile's az account")

// Read emits every allowed conversation that changed since the last run. A
// Graph error on one container does not stop the others; they are returned
// together at the end. An error from emit stops the run at once.
func (c *Connector) Read(ctx context.Context, emit func(feeds.Record) error) error {
	if c.State == nil {
		c.State = NewState()
	}
	if c.Tenant == "" {
		return ErrNoTenant
	}
	if !c.allowsTeams() {
		// Nothing of Teams is allowed: read nothing, and forget what earlier
		// runs stored, so a container allowed again starts from the start.
		if len(c.State.Watermarks) > 0 || len(c.State.Hashes) > 0 || len(c.State.AtMark) > 0 {
			clear(c.State.Watermarks)
			clear(c.State.Hashes)
			clear(c.State.AtMark)
			return c.State.Save()
		}
		return nil
	}
	var errs []error
	chats, err := list[chat](ctx, c.Graph, graphURL(url.Values{"$top": {pageSize}}, "me", "chats"))
	if err != nil {
		errs = append(errs, fmt.Errorf("list chats: %w", err))
	}
	for _, ch := range chats {
		kind, ok := chatKinds[ch.ChatType]
		if !ok || !idPattern.MatchString(ch.ID) || !c.own(ch.TenantID) {
			continue
		}
		base := feeds.Record{System: feeds.Teams, Tenant: c.Tenant, Container: ch.ID, Kind: kind}
		if err := c.sync(ctx, base, emit, c.chatConversations); err != nil {
			if errors.Is(err, errEmit) {
				return err
			}
			errs = append(errs, err)
		}
	}
	if err := c.readChannels(ctx, emit, &errs); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// readChannels syncs the allowed channels of every team the user is in.
// Graph errors are added to errs; an emit error is returned.
func (c *Connector) readChannels(ctx context.Context, emit func(feeds.Record) error, errs *[]error) error {
	teams, err := list[team](ctx, c.Graph, graphURL(nil, "me", "joinedTeams"))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("list teams: %w", err))
	}
	for _, tm := range teams {
		if !idPattern.MatchString(tm.ID) || !c.own(tm.TenantID) {
			continue
		}
		channels, err := list[channel](ctx, c.Graph, graphURL(nil, "teams", tm.ID, "channels"))
		if err != nil {
			*errs = append(*errs, fmt.Errorf("list channels of team %s: %w", tm.ID, err))
			continue
		}
		for _, ch := range channels {
			if !idPattern.MatchString(ch.ID) {
				continue
			}
			base := feeds.Record{System: feeds.Teams, Tenant: c.Tenant, Container: ch.ID, Kind: channelKind(tm, ch)}
			read := func(ctx context.Context, base feeds.Record, w mark) ([]feeds.Record, mark, error) {
				return c.channelConversations(ctx, tm.ID, base, w)
			}
			if err := c.sync(ctx, base, emit, read); err != nil {
				if errors.Is(err, errEmit) {
					return err
				}
				*errs = append(*errs, err)
			}
		}
	}
	return nil
}

// own reports whether tenant is the login's own. Reason: a chat or team
// hosted in another tenant (a guest's or a federated chat) carries that
// tenant's ID, and the tenant decides the space content goes to, so content
// read with this login would land in whatever space binds the other tenant,
// chosen by whoever created the chat. Such chats are not read.
func (c *Connector) own(tenant string) bool {
	return idPattern.MatchString(tenant) && strings.EqualFold(tenant, c.Tenant)
}

// channelKind is a channel's container kind. Reason: only a standard
// channel of a public team is open to everyone in the tenant; a private or
// shared channel, or any channel of a private team, is not, and when Graph
// does not say the team is public it is taken as private.
func channelKind(tm team, ch channel) feeds.ContainerKind {
	if ch.MembershipType == "standard" && strings.EqualFold(tm.Visibility, "public") {
		return feeds.PublicChannel
	}
	return feeds.PrivateChannel
}

// allowsTeams reports whether the allowlist names any Teams tenant.
func (c *Connector) allowsTeams() bool {
	teams := func(t string) bool { return strings.HasPrefix(strings.ToLower(t), string(feeds.Teams)+":") }
	return slices.ContainsFunc(c.Allow.DirectMessages, teams) ||
		slices.ContainsFunc(slices.Collect(maps.Keys(c.Allow.Containers)), func(t string) bool {
			return teams(t) && len(c.Allow.Containers[t]) > 0
		})
}

// errEmit marks an error from emit, which stops the whole run.
var errEmit = errors.New("not stored")

// conversations reads the records of base's container changed since w (all
// of them when w has no time), and returns them with the watermark to keep
// once they are stored.
type conversations func(ctx context.Context, base feeds.Record, w mark) ([]feeds.Record, mark, error)

// mark is a container's watermark: the newest change stored, and for a
// chat the IDs of the messages changed at that millisecond.
type mark struct {
	at  time.Time
	has bool
	ids []string
}

// sync emits the changed conversations of base's container when the
// allowlist allows it, and then advances its watermark; a container it does
// not allow is not read, and what was stored about it is forgotten.
func (c *Connector) sync(ctx context.Context, base feeds.Record, emit func(feeds.Record) error, read conversations) error {
	key := base.URI()
	if !c.Allow.Allows(base) {
		if c.State.forget(key) {
			return c.State.Save()
		}
		return nil
	}
	at, has := c.State.Watermarks[key]
	recs, next, err := read(ctx, base, mark{at: at, has: has, ids: c.State.AtMark[key]})
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	for _, r := range recs {
		h := hash(r)
		if c.State.Hashes[r.URI()] == h {
			continue
		}
		if err := emit(r); err != nil {
			return fmt.Errorf("%w: %w", errEmit, err)
		}
		c.State.Hashes[r.URI()] = h
	}
	if next.has {
		c.State.Watermarks[key] = next.at
		if len(next.ids) > 0 {
			c.State.AtMark[key] = next.ids
		} else {
			delete(c.State.AtMark, key)
		}
	}
	return c.State.Save()
}

// chatConversations reads a chat's days changed since w. Graph filters a
// chat's messages by lastModifiedDateTime only, so a run first lists the
// messages changed since the watermark, then lists from the start of the
// earliest day they were written on, and keeps the messages of the changed
// days: a message is never modified before it is created, so that second
// listing holds every message of those days.
func (c *Connector) chatConversations(ctx context.Context, base feeds.Record, w mark) ([]feeds.Record, mark, error) {
	messages := func(since time.Time, filter bool) ([]message, error) {
		q := url.Values{"$top": {pageSize}}
		if filter {
			q.Set("$orderby", "lastModifiedDateTime desc")
			q.Set("$filter", "lastModifiedDateTime gt "+graphTime(since))
		}
		return list[message](ctx, c.Graph, graphURL(q, "me", "chats", base.Container, "messages"))
	}
	// Reason: Graph keeps milliseconds and filters "gt", so the run asks
	// from a millisecond before the watermark, to get what changed in that
	// millisecond after the last run, and sets aside what it already had.
	changed, err := messages(w.at.Add(-time.Millisecond), w.has)
	if err != nil {
		return nil, w, err
	}
	changed = slices.DeleteFunc(changed, func(m message) bool {
		return w.has && !m.LastModifiedDateTime.After(w.at) && slices.Contains(w.ids, m.ID)
	})
	if len(changed) == 0 {
		return nil, w, nil
	}
	days := map[string]bool{}
	next := mark{at: w.at, has: true}
	for _, m := range changed {
		days[day(m.CreatedDateTime)] = true
		if m.LastModifiedDateTime.After(next.at) {
			next.at = m.LastModifiedDateTime
		}
	}
	if next.at.Equal(w.at) {
		next.ids = slices.Clone(w.ids)
	}
	for _, m := range changed {
		if m.LastModifiedDateTime.Equal(next.at) && !slices.Contains(next.ids, m.ID) {
			next.ids = append(next.ids, m.ID)
		}
	}
	all := changed
	order := slices.Sorted(maps.Keys(days))
	if w.has {
		if all, err = messages(dayStart(order[0]).Add(-time.Millisecond), true); err != nil {
			return nil, w, err
		}
	}
	byDay := map[string][]message{}
	for _, m := range all {
		if d := day(m.CreatedDateTime); days[d] {
			byDay[d] = append(byDay[d], m)
		}
	}
	var recs []feeds.Record
	for _, d := range order {
		r := base
		r.Thread = d
		if rec, ok := conversation(r, byDay[d]); ok {
			recs = append(recs, rec)
		}
	}
	return recs, next, nil
}

// channelConversations reads a channel's threads with activity at or after
// mark: Graph lists a channel's root messages with their replies, and has
// no filter for when a thread last changed.
func (c *Connector) channelConversations(ctx context.Context, teamID string, base feeds.Record, w mark) ([]feeds.Record, mark, error) {
	roots, err := list[message](ctx, c.Graph, graphURL(url.Values{"$top": {pageSize}, "$expand": {"replies"}},
		"teams", teamID, "channels", base.Container, "messages"))
	if err != nil {
		return nil, w, err
	}
	next := mark{at: w.at, has: true}
	var recs []feeds.Record
	for _, root := range roots {
		if !idPattern.MatchString(root.ID) {
			continue
		}
		if root.RepliesNext != "" {
			// Graph expanded only some replies; list them all.
			replies, err := list[message](ctx, c.Graph, graphURL(url.Values{"$top": {pageSize}},
				"teams", teamID, "channels", base.Container, "messages", root.ID, "replies"))
			if err != nil {
				return nil, w, err
			}
			root.Replies = replies
		}
		act := root.activity()
		if w.has && act.Before(w.at) {
			continue
		}
		if act.After(next.at) {
			next.at = act
		}
		r := base
		r.Thread = root.ID
		if rec, ok := conversation(r, append([]message{root}, root.Replies...)); ok {
			recs = append(recs, rec)
		}
	}
	return recs, next, nil
}

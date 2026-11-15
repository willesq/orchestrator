// Package teams is the Microsoft Teams chat connector (SP-062, D-07): it
// reads the signed-in user's chats (one-to-one, group and meeting chats) and
// the channels of the teams they belong to through Microsoft Graph, and
// reports each conversation to feeds.Sync as a feeds.Record of facts.
//
// It never handles a credential. Graph is reached through a Graph
// implementation; the production one (AzRest) runs the Azure CLI, which the
// user logs in themselves in a profile directory, so the token stays inside
// that process (docs/internals/memory.md, "Chat feeds").
package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
)

// GraphBase is the Microsoft Graph endpoint every request goes to.
const GraphBase = "https://graph.microsoft.com/v1.0"

// Graph answers a GET of a Microsoft Graph URL with the response body. The
// URL always starts with GraphBase.
type Graph interface {
	Get(ctx context.Context, rawURL string) ([]byte, error)
}

// GraphFunc is a function as a Graph.
type GraphFunc func(ctx context.Context, rawURL string) ([]byte, error)

// Get calls f.
func (f GraphFunc) Get(ctx context.Context, rawURL string) ([]byte, error) { return f(ctx, rawURL) }

// chat is a Graph chat (GET /me/chats).
type chat struct {
	ID       string `json:"id"`
	ChatType string `json:"chatType"`
	TenantID string `json:"tenantId"`
}

// team is a Graph team the user belongs to (GET /me/joinedTeams).
type team struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenantId"`
	Visibility string `json:"visibility"`
}

// channel is a Graph channel (GET /teams/{id}/channels).
type channel struct {
	ID             string `json:"id"`
	MembershipType string `json:"membershipType"`
}

// message is a Graph chatMessage, in a chat or a channel.
type message struct {
	ID                   string     `json:"id"`
	ReplyToID            string     `json:"replyToId"`
	MessageType          string     `json:"messageType"`
	CreatedDateTime      time.Time  `json:"createdDateTime"`
	LastModifiedDateTime time.Time  `json:"lastModifiedDateTime"`
	DeletedDateTime      *time.Time `json:"deletedDateTime"`
	From                 *struct {
		User *struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"user"`
	} `json:"from"`
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	// Replies are a channel message's replies when listed with
	// $expand=replies; RepliesNext is set when Graph left some out.
	Replies     []message `json:"replies"`
	RepliesNext string    `json:"replies@odata.nextLink"`
}

// activity is when m or any of its replies last changed.
func (m message) activity() time.Time {
	t := m.LastModifiedDateTime
	for _, r := range m.Replies {
		if r.LastModifiedDateTime.After(t) {
			t = r.LastModifiedDateTime
		}
	}
	return t
}

// page is one page of a Graph collection.
type page[T any] struct {
	Value    []T    `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// maxPages bounds one listing, so a server that keeps answering with a next
// link cannot hold a sync forever.
const maxPages = 2000

// ErrGraph marks an answer from Graph the connector cannot use.
var ErrGraph = errors.New("unusable Microsoft Graph response")

// list GETs every page of the collection at rawURL.
func list[T any](ctx context.Context, g Graph, rawURL string) ([]T, error) {
	var all []T
	seen := map[string]bool{}
	for n := 0; rawURL != ""; n++ {
		if n == maxPages || seen[rawURL] {
			return nil, fmt.Errorf("%w: the listing does not end", ErrGraph)
		}
		seen[rawURL] = true
		// Reason: a next link comes from the server; the transport attaches
		// the user's Graph login to what it fetches, so it is never sent
		// anywhere but Graph.
		if !strings.HasPrefix(rawURL, GraphBase+"/") {
			return nil, fmt.Errorf("%w: a next link leaves %s", ErrGraph, GraphBase)
		}
		body, err := g.Get(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		var p page[T]
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGraph, err)
		}
		all = append(all, p.Value...)
		rawURL = p.NextLink
	}
	return all, nil
}

// validID reports whether v is a Graph ID the connector puts in a URL or a
// record: chat and channel IDs (19:abc@thread.v2), GUIDs and message IDs.
// It is the stable ID feeds.Record accepts (feeds.ValidID), which has no
// '/', '?' or '%'.
func validID(v string) bool { return feeds.ValidID(v) }

// graphURL is GraphBase joined with path segments, each an ID or a fixed
// name, and query q.
func graphURL(q url.Values, segments ...string) string {
	var b strings.Builder
	b.WriteString(GraphBase)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	if len(q) > 0 {
		b.WriteByte('?')
		b.WriteString(q.Encode())
	}
	return b.String()
}

// graphTime is t as a Graph $filter literal, to the millisecond Graph keeps.
func graphTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

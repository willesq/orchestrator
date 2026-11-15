package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/feeds/teams"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/spaces"
)

// teamsGraph is a one-page Graph tenant: globex-tenant's meeting chat and
// a channel of its team, each with messages.
func teamsGraph(t *testing.T, messages map[string][]map[string]any) teams.Graph {
	t.Helper()
	pages := map[string]any{
		"/me/chats":       []map[string]any{{"id": "19:standup@thread.v2", "chatType": "meeting", "tenantId": "globex-tenant"}},
		"/me/joinedTeams": []map[string]any{{"id": "team-1", "tenantId": "globex-tenant"}},
		"/teams/team-1/channels": []map[string]any{
			{"id": "19:eng@thread.tacv2", "membershipType": "standard"},
		},
		"/me/chats/19:standup@thread.v2/messages":             messages["19:standup@thread.v2"],
		"/teams/team-1/channels/19:eng@thread.tacv2/messages": messages["19:eng@thread.tacv2"],
	}
	return teams.GraphFunc(func(_ context.Context, raw string) ([]byte, error) {
		path, _, _ := strings.Cut(strings.TrimPrefix(raw, teams.GraphBase), "?")
		v, ok := pages[path]
		if !ok {
			t.Errorf("unexpected Graph request %s", raw)
		}
		return json.Marshal(map[string]any{"value": v})
	})
}

func teamsMessage(id, text, at string) map[string]any {
	return map[string]any{"id": id, "messageType": "message", "createdDateTime": at, "lastModifiedDateTime": at,
		"from": map[string]any{"user": map[string]any{"id": "u-1", "displayName": "Pat"}}, "body": map[string]any{"contentType": "text", "content": text}}
}

// TestTeamsConnectorPlacedByTenant: the Teams connector's records go
// through orchd's placed write path, so with memory spaces on the tenant's
// binding decides their space (globex binds teams:globex-tenant), and with
// them off everything is stored in the one shared store (SP-062).
func TestTeamsConnectorPlacedByTenant(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	allow := feeds.Allowlist{Containers: map[string][]string{"teams:globex-tenant": {"19:standup@thread.v2", "19:eng@thread.tacv2"}}}
	msgs := map[string][]map[string]any{
		"19:standup@thread.v2": {teamsMessage("1", "standup: parser ships friday", "2026-11-14T09:00:00Z")},
		"19:eng@thread.tacv2":  {teamsMessage("100", "eng: freeze starts monday", "2026-11-14T10:00:00Z")},
	}
	conn := &teams.Connector{Graph: teamsGraph(t, msgs), Tenant: "globex-tenant", Allow: allow}
	if rep, err := feeds.Sync(context.Background(), conn, allow, e.svc); err != nil || rep.Stored != 2 {
		t.Fatalf("sync = %+v, %v", rep, err)
	}
	for uri, kind := range map[string]string{
		"teams://globex-tenant/19:standup@thread.v2/2026-11-14": rag.ChatMeeting,
		"teams://globex-tenant/19:eng@thread.tacv2/100":         rag.ChatPrivateChannel,
	} {
		if origin := e.placedAs(t, uri, "globex", "teams"); origin["container_kind"] != kind || origin["participants"] != "u-1" {
			t.Errorf("%s: origin = %v", uri, origin)
		}
	}

	// Spaces off: one store, nothing placed by tenant.
	e.s.spaces.SetOff(true)
	t.Cleanup(func() { e.s.spaces.SetOff(false) })
	msgs["19:eng@thread.tacv2"] = append(msgs["19:eng@thread.tacv2"], teamsMessage("200", "eng: written while spaces are off", "2026-11-15T10:00:00Z"))
	conn.Graph = teamsGraph(t, msgs)
	if rep, err := feeds.Sync(context.Background(), conn, allow, e.svc); err != nil || rep.Stored != 1 {
		t.Fatalf("sync with spaces off = %+v, %v", rep, err)
	}
	uri := "teams://globex-tenant/19:eng@thread.tacv2/200"
	pts := e.q.storedIn("c", uri)
	if len(pts) == 0 {
		t.Fatalf("%s not in the one store (have %v)", uri, e.q.uris())
	}
	for _, p := range pts {
		if p["space"] != spaces.Shared || p["chat_kind"] != rag.ChatPrivateChannel {
			t.Errorf("%s: payload = %v", uri, p)
		}
	}
}

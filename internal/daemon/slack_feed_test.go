package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/feeds/slack"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/spaces"
)

// slackLines is a Slack connector process that answers with recs, each
// followed by its conversation's watermark.
func slackLines(recs ...feeds.Record) slack.Runner {
	return func(_ context.Context, _ slack.Request, out func(slack.Line) error) error {
		for _, r := range recs {
			if err := out(slack.Line{Record: &r}); err != nil {
				return err
			}
			if err := out(slack.Line{Channel: &slack.ChannelDone{ID: r.Container, State: slack.ChannelState{Kind: r.Kind, Latest: "1.0"}}}); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestSlackConnectorSpacesOnAndOff: the Slack connector's records go
// through orchd's placed write path. With spaces on, the workspace's tenant
// binding decides the space and an unbound workspace goes to quarantine
// (INV-03); with spaces off, everything is in the one shared store, in the
// implicit shared space, as before spaces (INV-10).
func TestSlackConnectorSpacesOnAndOff(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	ctx := context.Background()
	allow := feeds.Allowlist{
		DirectMessages: []string{"slack:TACME"},
		Containers:     map[string][]string{"slack:TACME": {"C0001"}, "slack:TOTHER": {"C0009"}},
	}
	sync := func(tenant string, recs ...feeds.Record) {
		t.Helper()
		c := &slack.Connector{Tenant: tenant, Profile: "/unused", Allow: allow,
			State: slack.FileState{Path: filepath.Join(t.TempDir(), "slack.json")}, Run: slackLines(recs...)}
		if rep, err := feeds.Sync(ctx, c, allow, e.svc); err != nil || rep.Stored != len(recs) {
			t.Fatalf("sync %s = %+v, %v", tenant, rep, err)
		}
	}
	sync("TACME",
		chatRecord(feeds.Slack, "TACME", "D0001", feeds.DM, "2026-11-01", "can you review my patch"),
		chatRecord(feeds.Slack, "TACME", "C0001", feeds.PrivateChannel, "1700000000.000100", "release train moves to thursday"))
	sync("TOTHER", chatRecord(feeds.Slack, "TOTHER", "C0009", feeds.PublicChannel, "2026-11-01", "an unbound workspace"))
	e.placedAs(t, "slack://TACME/D0001/2026-11-01", "acme", "slack")
	e.placedAs(t, "slack://TACME/C0001/1700000000.000100", "acme", "slack")
	e.placedAs(t, "slack://TOTHER/C0009/2026-11-01", "quarantine", "slack")

	e.s.spaces.SetOff(true)
	sync("TACME", chatRecord(feeds.Slack, "TACME", "C0001", feeds.PrivateChannel, "1700000100.000100", "spaces are off now"))
	sync("TOTHER", chatRecord(feeds.Slack, "TOTHER", "C0009", feeds.PublicChannel, "2026-11-02", "still unbound"))
	for _, uri := range []string{"slack://TACME/C0001/1700000100.000100", "slack://TOTHER/C0009/2026-11-02"} {
		pts := e.q.stored(uri)
		if len(pts) == 0 || len(e.q.storedIn("c", uri)) != len(pts) {
			t.Fatalf("%s: not all in the shared store (have %v)", uri, e.q.uris())
		}
		for _, p := range pts {
			if p["space"] != spaces.Shared {
				t.Errorf("%s: space %v with spaces off, want the implicit shared space", uri, p["space"])
			}
			if p["writer"] != rag.WriterChat || p["chat_kind"] == nil {
				t.Errorf("%s: payload = %v", uri, p)
			}
		}
	}
}

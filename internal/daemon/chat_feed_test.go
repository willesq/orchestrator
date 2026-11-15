package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// fakeConnector is a chat connector that reports canned records.
type fakeConnector struct {
	system  feeds.System
	records []feeds.Record
}

func (c fakeConnector) System() feeds.System { return c.system }

func (c fakeConnector) Read(_ context.Context, emit func(feeds.Record) error) error {
	for _, r := range c.records {
		if err := emit(r); err != nil {
			return err
		}
	}
	return nil
}

func chatRecord(system feeds.System, tenant, container string, kind feeds.ContainerKind, thread, text string) feeds.Record {
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	return feeds.Record{System: system, Tenant: tenant, Container: container, Kind: kind, Thread: thread,
		Participants: []string{"U2", "U1"}, Text: text, StartedAt: at, EndedAt: at.Add(time.Hour)}
}

// TestChatRecordsPlacedByTenant is SP-060.1.a1: a fake connector's records
// go to the space their tenant is bound to, through the same placed write
// path as every writer, and an unbound tenant's go to quarantine (INV-03).
func TestChatRecordsPlacedByTenant(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	slack := fakeConnector{system: feeds.Slack, records: []feeds.Record{
		chatRecord(feeds.Slack, "TACME", "C0001", feeds.PrivateChannel, "1700000000.000100", "release train moves to thursday"),
		chatRecord(feeds.Slack, "TACME", "D0001", feeds.DM, "", "can you review my patch"),
		chatRecord(feeds.Slack, "TOTHER", "C0009", feeds.PublicChannel, "", "an unbound workspace's channel"),
	}}
	teams := fakeConnector{system: feeds.Teams, records: []feeds.Record{
		chatRecord(feeds.Teams, "globex-tenant", "19:standup@thread.v2", feeds.Meeting, "", "globex standup notes"),
	}}
	allow := feeds.Allowlist{
		DirectMessages: []string{"slack:TACME"},
		Containers: map[string][]string{
			"slack:TACME": {"C0001"}, "slack:TOTHER": {"C0009"}, "teams:globex-tenant": {"19:standup@thread.v2"},
		},
	}
	ctx := context.Background()
	for _, c := range []feeds.Connector{slack, teams} {
		rep, err := feeds.Sync(ctx, c, allow, e.svc)
		if err != nil || rep.Refused != 0 || rep.Skipped != 0 {
			t.Fatalf("%s sync = %+v, %v", c.System(), rep, err)
		}
	}
	for uri, want := range map[string]struct{ space, system, kind string }{
		"slack://TACME/C0001/1700000000.000100":      {"acme", "slack", rag.ChatPrivateChannel},
		"slack://TACME/D0001":                        {"acme", "slack", rag.ChatDM},
		"slack://TOTHER/C0009":                       {"quarantine", "slack", rag.ChatPublicChannel},
		"teams://globex-tenant/19:standup@thread.v2": {"globex", "teams", rag.ChatMeeting},
	} {
		origin := e.placedAs(t, uri, want.space, want.system)
		if origin["container_kind"] != want.kind || origin["participants"] != "U1,U2" {
			t.Errorf("%s: origin = %v", uri, origin)
		}
		for _, p := range e.q.stored(uri) {
			if p["chat_kind"] != want.kind || p["writer"] != rag.WriterChat || p["trust"] != rag.TrustChat || p["source_type"] != feeds.SourceType {
				t.Errorf("%s: payload = %v", uri, p)
			}
		}
	}
}

// TestChatSourceNotReplacedByClients: a client cannot overwrite what a chat
// feed keeps, as it cannot a docs or sources sync's copy.
func TestChatSourceNotReplacedByClients(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	conn := fakeConnector{system: feeds.Slack, records: []feeds.Record{
		chatRecord(feeds.Slack, "TACME", "C0001", feeds.PrivateChannel, "", "the real thread"),
	}}
	if _, err := feeds.Sync(context.Background(), conn, feeds.Allowlist{Containers: map[string][]string{"slack:TACME": {"C0001"}}}, e.svc); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.Ingest(as(resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "s", Space: "acme"}}),
		&orchdv1.IngestRequest{SourceUri: "slack://TACME/C0001", Text: "a forged thread"})
	// The refusal does not confirm that a chat source is stored there.
	if grpcstatus.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "chat") {
		t.Fatalf("client overwrite of a chat source = %v, want the generic refusal", err)
	}
}

// chatEnv is readEnv with chat chunks in acme and globex: a channel thread
// and a DM in each.
type chatEnv struct {
	readEnv
}

const (
	acmeChannelText = "acme channel thread: the launch checklist lives in the wiki"
	acmeDMText      = "acme direct message: the merger codename is bluefin"
	globexDMText    = "globex direct message: the merger codename is bluefin too"
	acmeChannelURI  = "slack://TACME/C0001/1700000000.000100"
	acmeDMURI       = "slack://TACME/D0001"
	globexDMURI     = "teams://globex-tenant/19:dm@unq.gbl.spaces"
)

func newChatEnv(t *testing.T) chatEnv {
	t.Helper()
	e := newReadEnv(t)
	seed := func(collection, id, space, uri, kind, text string) {
		e.q.seed(collection, id, map[string]any{
			"text": text, "source_uri": uri, "source_type": feeds.SourceType, "chunk_index": 0.0, "space": space,
			"writer": rag.WriterChat, "chat_kind": kind, "labels_v": e.labels,
		})
	}
	seed("c__acme", "chat-acme-channel", "acme", acmeChannelURI, rag.ChatPrivateChannel, acmeChannelText)
	seed("c__acme", "chat-acme-dm", "acme", acmeDMURI, rag.ChatDM, acmeDMText)
	seed("c__globex", "chat-globex-dm", "globex", globexDMURI, rag.ChatGroupDM, globexDMText)
	return chatEnv{e}
}

// mentions reports whether any of got contains needle.
func mentions(got []string, needle string) bool {
	return slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, needle) })
}

// TestChatDMsNeverAutoRecalled is SP-060.1.a2 (auto-RAG): what auto-RAG
// considers and injects for a session in acme, prompted with a DM's very
// text, holds acme's channel but never a DM; nor does any other read path a
// session has but the explicit DM search.
func TestChatDMsNeverAutoRecalled(t *testing.T) {
	e := newChatEnv(t)
	sess := resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-acme", Space: "acme"}}
	client := e.clientFor(t, sess)
	for _, prompt := range []string{acmeDMText, acmeChannelText + " " + acmeDMText} {
		got := explained(t, client, prompt, e.code+"/acme/repo")
		if mentions(got, "bluefin") || mentions(got, acmeDMURI) {
			t.Errorf("auto-RAG for %q considered a DM: %v", prompt, got)
		}
	}
	if got := explained(t, client, acmeChannelText, e.code+"/acme/repo"); !mentions(got, acmeChannelURI) {
		t.Errorf("auto-RAG lost acme's channel: %v", got)
	}
	got := e.readAll(t, inSpace("acme"), acmeDMText, acmeDMURI)
	if mentions(got, "bluefin") || mentions(got, acmeDMURI) {
		t.Errorf("a session's ordinary reads returned a DM")
	}
	// The operator's whole-store reads leave DMs out too: auto-RAG runs as
	// whoever is at the prompt.
	rep, err := e.s.Search(operatorCtx, &orchdv1.SearchRequest{Query: acmeDMText, TopK: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rep.GetHits() {
		if strings.Contains(h.GetText(), "bluefin") {
			t.Errorf("the operator's Search returned a DM: %s", h.GetSourceUri())
		}
	}
}

// TestChatDMsNeverGivenToTasks is SP-060.1.a2 (tasks, D-19): a queued task
// in acme searches acme's channels but no DM, by any read path, and is
// refused the DM search both by the guard and by the handler.
func TestChatDMsNeverGivenToTasks(t *testing.T) {
	e := newChatEnv(t)
	task := as(resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-1", Space: "acme"}})
	got := e.readAll(t, task, acmeDMText, acmeDMURI)
	if mentions(got, "bluefin") || mentions(got, acmeDMURI) {
		t.Errorf("a task's reads returned a DM")
	}
	if got := e.readAll(t, task, acmeChannelText, acmeChannelURI); !mentions(got, acmeChannelText) {
		t.Errorf("a task lost its space's channel")
	}
	req := &orchdv1.SearchDirectMessagesRequest{Search: &orchdv1.SearchRequest{Query: acmeDMText, TopK: 100}}
	if _, err := e.s.SearchDirectMessages(task, req); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("a task's DM search = %v, want PermissionDenied", err)
	}
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	if err := refuseQueued(queued, orchdv1.Orchd_SearchDirectMessages_FullMethodName); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("the queued-caller guard let a task search DMs: %v", err)
	}
}

// TestChatDMsInExplicitSearch is SP-060.1.a2 (explicit interactive
// search): the operator's DM search (Orch.app under a policy, behind
// 'orch search --direct-messages') finds DMs, while an agent in an
// interactive session, and a terminal that is not Orch.app under a policy
// (D-71), are refused it.
func TestChatDMsInExplicitSearch(t *testing.T) {
	e := newChatEnv(t)
	req := &orchdv1.SearchDirectMessagesRequest{Search: &orchdv1.SearchRequest{Query: acmeDMText, TopK: 100}}
	rep, err := e.s.SearchDirectMessages(appOperatorCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, h := range rep.GetHits() {
		texts = append(texts, h.GetText(), h.GetSourceUri())
	}
	if !mentions(texts, acmeDMText) || !mentions(texts, acmeDMURI) || !mentions(texts, globexDMURI) {
		t.Errorf("the operator's DM search lost a DM: %v", texts)
	}
	session := resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-acme", Space: "acme"}}
	for name, ctx := range map[string]context.Context{
		"a session's agent":                    context.WithValue(callerCtx(callerAuth{pid: 43, harness: "claude"}), resolvedKey{}, session),
		"a session resolved on a clean socket": context.WithValue(operatorCtx, resolvedKey{}, session),
		"a terminal while a policy is applied": context.WithValue(operatorCtx, resolvedKey{}, resolvedCaller{kind: callerOperator, evidenceNeeded: true}),
		"an unidentified caller":               inSpace("acme"),
	} {
		if _, err := e.s.SearchDirectMessages(ctx, req); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: DM search = %v, want PermissionDenied", name, err)
		}
	}
	if _, err := e.s.SearchDirectMessages(appOperatorCtx, &orchdv1.SearchDirectMessagesRequest{}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("an empty DM search = %v, want InvalidArgument", err)
	}
}

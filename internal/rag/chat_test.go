package rag

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ingestChat writes a chat thread of kind as uri, placed in space.
func ingestChat(t *testing.T, svc *Service, space, uri, kind, text string) {
	t.Helper()
	m := Meta{SourceURI: uri, SourceType: "chat", Tags: []string{space}, Writer: WriterChat,
		Origin: Origin{System: OriginSlack, Tenant: "T1", Container: "C1", ContainerKind: kind, Participants: "U1,U2"}}
	if _, err := svc.IngestText(context.Background(), text, m); err != nil {
		t.Fatalf("ingest %s: %v", uri, err)
	}
}

// TestChatDirectMessagesLeftOutOfReads: a DM is stored with its kind and
// origin, every read leaves it out (search in both modes, the inventory and
// a document read, a whole-store one included), and only a search asking
// for direct messages finds it (SP-060). The daemon's tests cover a
// whole-store search, the operator's.
func TestChatDirectMessagesLeftOutOfReads(t *testing.T) {
	svc, f := spacesService(t)
	ingestChat(t, svc, "acme", "slack://T1/C1", ChatPrivateChannel, "the deploy runbook in the channel")
	ingestChat(t, svc, "acme", "slack://T1/D1", ChatDM, "the deploy runbook in a direct message")
	ingestChat(t, svc, "acme", "slack://T1/G1", ChatGroupDM, "the deploy runbook in a group message")
	ingestIn(t, svc, "acme", "manual://note", "the deploy runbook in a note")
	for _, p := range f.collection("c__acme").points {
		if str(p.Payload["source_uri"]) == "slack://T1/D1" {
			o := originFrom(p.Payload[fieldOrigin])
			if p.Payload[fieldChatKind] != ChatDM || o.ContainerKind != ChatDM || o.Tenant != "T1" || o.Participants != "U1,U2" {
				t.Fatalf("DM stored as %v", p.Payload)
			}
		}
	}
	ctx := context.Background()
	uris := func(hits []Hit) string {
		var out []string
		for _, h := range hits {
			out = append(out, h.SourceURI)
		}
		return strings.Join(out, " ")
	}
	for _, opts := range []SearchOpts{
		{TopK: 10, Spaces: []string{"acme"}, Mode: ModeHybrid},
		{TopK: 10, Spaces: []string{"acme"}, Mode: ModeDense},
	} {
		hits, _, err := svc.Search(ctx, "deploy runbook", opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := uris(hits); strings.Contains(got, "/D1") || strings.Contains(got, "/G1") || !strings.Contains(got, "slack://T1/C1") {
			t.Errorf("search %+v = %s", opts, got)
		}
		opts.DirectMessages = true
		if hits, _, err = svc.Search(ctx, "deploy runbook", opts); err != nil {
			t.Fatal(err)
		}
		if got := uris(hits); !strings.Contains(got, "slack://T1/D1") || !strings.Contains(got, "slack://T1/G1") || !strings.Contains(got, "manual://note") {
			t.Errorf("DM search %+v = %s", opts, got)
		}
	}
	srcs, _, _, err := svc.ListSources(ctx, ListSourcesOpts{Spaces: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.SourceURI == "slack://T1/D1" || s.SourceURI == "slack://T1/G1" {
			t.Errorf("ListSources listed a DM: %s", s.SourceURI)
		}
	}
	if _, err := svc.DocumentIn(ctx, "slack://T1/D1", []string{"acme"}, 0); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("DocumentIn of a DM = %v", err)
	}
	if _, err := svc.Document(ctx, "slack://T1/G1"); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("Document of a group DM = %v", err)
	}
	if d, err := svc.Document(ctx, "slack://T1/C1"); err != nil || d.Meta.Origin.ContainerKind != ChatPrivateChannel {
		t.Errorf("Document of a channel = %+v, %v", d.Meta.Origin, err)
	}
}

// TestChatWriteNeedsAKnownKind: a chat write naming no kind, or one orch
// does not know, is refused before it is placed, since only the known DM
// kinds are kept out of reads.
func TestChatWriteNeedsAKnownKind(t *testing.T) {
	svc, f := spacesService(t)
	for _, kind := range []string{"", "im", "DM"} {
		m := Meta{SourceURI: "teams://T/x", Tags: []string{"acme"}, Writer: WriterChat,
			Origin: Origin{System: OriginTeams, Tenant: "T", Container: "x", ContainerKind: kind}}
		if _, err := svc.IngestText(context.Background(), "hello", m); err == nil {
			t.Errorf("kind %q was stored", kind)
		}
	}
	// A chat origin is a chat feed's alone.
	m := Meta{SourceURI: "slack://T/C", Tags: []string{"acme"}, Writer: WriterClient,
		Origin: Origin{System: OriginSlack, Tenant: "T", Container: "C", ContainerKind: ChatPublicChannel}}
	if _, err := svc.IngestText(context.Background(), "hello", m); err == nil {
		t.Error("a client write with a chat origin was stored")
	}
	if len(f.collections) != 0 {
		t.Errorf("a refused write reached the store: %v", f.collections)
	}
}

func TestDirectMessageFilter(t *testing.T) {
	b, _ := json.Marshal(buildFilter(map[string]string{excludeDirectMessages: "1"}, nil, nil, TimeRange{}))
	if got := string(b); got != `{"must_not":[{"key":"chat_kind","match":{"any":["dm","group_dm"]}}]}` {
		t.Errorf("filter = %s", got)
	}
	b, _ = json.Marshal(buildFilter(map[string]string{"scope": "w", excludeDirectMessages: "1"}, nil, nil, TimeRange{}))
	if got := string(b); !strings.Contains(got, `"must":[{"key":"scope"`) || !strings.Contains(got, `"must_not"`) {
		t.Errorf("filter = %s", got)
	}
	if (spaceStore{}).must(nil)[excludeDirectMessages] == "" {
		t.Error("a whole-store read keeps DMs")
	}
	if withDirectMessages([]spaceStore{{space: "acme"}}, true)[0].must(nil)[excludeDirectMessages] != "" {
		t.Error("a DM search leaves DMs out")
	}
}

func TestChatWriterAndKinds(t *testing.T) {
	if TrustFor(WriterChat, "chat") != TrustChat {
		t.Error("chat trust")
	}
	for w, want := range map[string]bool{WriterDocs: true, WriterSources: true, WriterChat: true, WriterClient: false, WriterSession: false, WriterFile: false} {
		if IsSyncWriter(w) != want {
			t.Errorf("IsSyncWriter(%s) = %v", w, !want)
		}
	}
	for k, want := range map[string]bool{ChatDM: true, ChatGroupDM: true, ChatPrivateChannel: false, ChatPublicChannel: false, ChatMeeting: false, "": false} {
		if IsDirectMessage(k) != want {
			t.Errorf("IsDirectMessage(%q) = %v", k, !want)
		}
	}
	if !IsChatOrigin(OriginSlack) || !IsChatOrigin(OriginTeams) || IsChatOrigin(OriginWeb) {
		t.Error("IsChatOrigin")
	}
	// An origin without chat facts keeps the key it had before them.
	plain := Origin{System: OriginWeb, URL: "https://x"}
	if strings.Count(plain.key(), "\x01") != 9 {
		t.Errorf("a non-chat origin's key has %d separators", strings.Count(plain.key(), "\x01"))
	}
	chat := Origin{System: OriginSlack, Tenant: "T", Container: "C", ContainerKind: ChatDM}
	if chat.key() == (Origin{System: OriginSlack, Tenant: "T", Container: "C", ContainerKind: ChatMeeting}).key() {
		t.Error("the container kind is not part of the key")
	}
}

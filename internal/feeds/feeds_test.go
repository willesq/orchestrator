package feeds

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
)

// fakeConnector reports canned records, and stops at the first error emit
// returns, as a real connector must.
type fakeConnector struct {
	system  System
	records []Record
	err     error
	emitted int
}

func (c *fakeConnector) System() System { return c.system }

func (c *fakeConnector) Read(_ context.Context, emit func(Record) error) error {
	for _, r := range c.records {
		if err := emit(r); err != nil {
			return err
		}
		c.emitted++
	}
	return c.err
}

// fakeIngester records what it is asked to store.
type fakeIngester struct {
	texts []string
	metas []rag.Meta
	err   error
}

func (f *fakeIngester) IngestText(_ context.Context, text string, m rag.Meta) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.texts = append(f.texts, text)
	f.metas = append(f.metas, m)
	return 1, nil
}

func rec(kind ContainerKind, container string) Record {
	return Record{System: Slack, Tenant: "T0123", Container: container, Kind: kind, Thread: "1700000000.000100",
		Participants: []string{"U2", "U1", "U2"}, Text: "hello",
		StartedAt: time.Unix(1700000000, 0), EndedAt: time.Unix(1700003600, 0)}
}

// TestRecordCarriesFactsOnly is SP-060.1.a3: a connector's record has only
// fact fields, so it cannot name a space, trust tier, scope, tag or any
// other label, and what Sync stores carries no space: orchd's placer alone
// sets it.
func TestRecordCarriesFactsOnly(t *testing.T) {
	facts := map[string]bool{"System": true, "Tenant": true, "Container": true, "Kind": true, "Thread": true,
		"Participants": true, "Text": true, "StartedAt": true, "EndedAt": true}
	rt := reflect.TypeOf(Record{})
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if !facts[name] {
			t.Errorf("Record has field %s, which is not a fact a connector reports", name)
		}
		for _, label := range []string{"space", "level", "trust", "scope", "tag", "writer", "label", "quarantine", "public"} {
			if strings.Contains(strings.ToLower(name), label) {
				t.Errorf("Record field %s names a label (%s)", name, label)
			}
		}
	}
	ing := &fakeIngester{}
	conn := &fakeConnector{system: Slack, records: []Record{rec(PrivateChannel, "C1")}}
	if _, err := Sync(context.Background(), conn, Allowlist{Containers: map[string][]string{"slack:T0123": {"C1"}}}, ing); err != nil {
		t.Fatal(err)
	}
	m := ing.metas[0]
	if m.Space != "" || m.LabelsV != 0 || m.Scope != "" || len(m.Tags) != 0 {
		t.Errorf("Sync labelled a write itself: %+v", m)
	}
	if m.Writer != rag.WriterChat || m.SourceType != SourceType {
		t.Errorf("writer %q, type %q", m.Writer, m.SourceType)
	}
	want := rag.Origin{System: "slack", Tenant: "T0123", Container: "C1", ContainerKind: "private_channel", Participants: "U1,U2"}
	if m.Origin != want {
		t.Errorf("origin = %+v, want %+v", m.Origin, want)
	}
	if !m.StartedAt.Equal(time.Unix(1700000000, 0)) || !m.EndedAt.Equal(time.Unix(1700003600, 0)) {
		t.Errorf("dates = %v..%v", m.StartedAt, m.EndedAt)
	}
}

func TestRecordValidate(t *testing.T) {
	ok := rec(PublicChannel, "C1")
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	teams := Record{System: Teams, Tenant: "72f988bf-86f1-41af-91ab-2d7cd011db47", Container: "19:abc_def@thread.v2",
		Kind: Meeting, Text: "notes"}
	if err := teams.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Record){
		"unknown system":       func(r *Record) { r.System = "irc" },
		"unknown kind":         func(r *Record) { r.Kind = "secret_dm" },
		"empty kind":           func(r *Record) { r.Kind = "" },
		"empty tenant":         func(r *Record) { r.Tenant = "" },
		"tenant with a slash":  func(r *Record) { r.Tenant = "T1/../T2" },
		"container with space": func(r *Record) { r.Container = "C 1" },
		"bad thread":           func(r *Record) { r.Thread = "a/b" },
		"participant comma":    func(r *Record) { r.Participants = []string{"U1,U2"} },
		"no text":              func(r *Record) { r.Text = "  \n" },
		"leading dot":          func(r *Record) { r.Container = ".hidden" },
		"Slack IM as channel":  func(r *Record) { r.Container = "D0123" },
	} {
		r := ok
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

func TestRecordURI(t *testing.T) {
	if got := rec(DM, "D1").URI(); got != "slack://T0123/D1/1700000000.000100" {
		t.Errorf("URI = %s", got)
	}
	r := rec(DM, "D1")
	r.Thread = ""
	if got := r.URI(); got != "slack://T0123/D1" {
		t.Errorf("URI without thread = %s", got)
	}
	if got := r.TenantKey(); got != "slack:T0123" {
		t.Errorf("TenantKey = %s", got)
	}
	if !DM.IsDirectMessage() || !GroupDM.IsDirectMessage() || PrivateChannel.IsDirectMessage() || Meeting.IsDirectMessage() {
		t.Error("IsDirectMessage")
	}
}

func TestAllowlist(t *testing.T) {
	a := AllowlistFrom(config.ChatFeedSettings{
		DirectMessages: []string{"Slack:t0123"},
		Containers:     map[string][]string{"slack:t0123": {"C1"}, "teams:x": {"C2"}},
	})
	for _, c := range []struct {
		r    Record
		want bool
	}{
		{rec(DM, "D1"), true},
		{rec(GroupDM, "G1"), true},
		{rec(PrivateChannel, "C1"), true},
		{rec(PublicChannel, "C2"), false},  // C2 is allowed in another tenant only
		{rec(PrivateChannel, "c1"), false}, // container IDs match exactly
		{Record{System: Teams, Tenant: "x", Container: "C2", Kind: Meeting}, true},
		{Record{System: Teams, Tenant: "x", Container: "D9", Kind: DM}, false},
	} {
		if got := a.Allows(c.r); got != c.want {
			t.Errorf("Allows(%s %s %s) = %v", c.r.TenantKey(), c.r.Kind, c.r.Container, got)
		}
	}
	if (Allowlist{}).Allows(rec(DM, "D1")) || (Allowlist{}).Allows(rec(PublicChannel, "C1")) {
		t.Error("an empty allowlist stored something")
	}
}

// TestSyncCountsAndRedacts: allowed records are stored redacted, others
// skipped, invalid ones and another system's refused; none stops the run.
func TestSyncCountsAndRedacts(t *testing.T) {
	secret := rec(PrivateChannel, "C1")
	secret.Text = "deploy with xoxb-123456789012-abcdefghijkl please"
	invalid := rec(PrivateChannel, "C1")
	invalid.Tenant = ""
	foreign := rec(PrivateChannel, "C1")
	foreign.System = Teams
	conn := &fakeConnector{system: Slack, records: []Record{secret, rec(DM, "D1"), rec(PublicChannel, "C9"), invalid, foreign}}
	ing := &fakeIngester{}
	allow := Allowlist{DirectMessages: []string{"slack:T0123"}, Containers: map[string][]string{"slack:T0123": {"C1"}}}
	rep, err := Sync(context.Background(), conn, allow, ing)
	if err != nil {
		t.Fatal(err)
	}
	if rep != (Report{Stored: 2, Skipped: 1, Refused: 2}) {
		t.Errorf("report = %+v", rep)
	}
	if strings.Contains(ing.texts[0], "xoxb-") {
		t.Errorf("a token was stored: %q", ing.texts[0])
	}
	if ing.metas[1].Origin.ContainerKind != "dm" {
		t.Errorf("DM stored as %+v", ing.metas[1].Origin)
	}
}

// TestSyncStopsOnFailedWrite: a failed write stops the connector, so its
// watermark does not pass what was not stored, and the error is returned.
func TestSyncStopsOnFailedWrite(t *testing.T) {
	conn := &fakeConnector{system: Slack, records: []Record{rec(PrivateChannel, "C1"), rec(PrivateChannel, "C1")}}
	boom := errors.New("qdrant down")
	rep, err := Sync(context.Background(), conn, Allowlist{Containers: map[string][]string{"slack:T0123": {"C1"}}}, &fakeIngester{err: boom})
	if !errors.Is(err, boom) || conn.emitted != 0 || rep.Stored != 0 {
		t.Fatalf("Sync = %+v, %v; emitted %d", rep, err, conn.emitted)
	}
	if !strings.Contains(err.Error(), "slack://T0123/C1/") {
		t.Errorf("error names no URI: %v", err)
	}
	read := errors.New("rate limited")
	if _, err := Sync(context.Background(), &fakeConnector{system: Slack, err: read}, Allowlist{}, &fakeIngester{}); !errors.Is(err, read) {
		t.Errorf("a connector's error was lost: %v", err)
	}
}

// TestDirectIDsMustBeDirectMessages: a conversation whose ID says it is a
// DM is refused as any other kind, so a connector's mislabel cannot store it
// where every read sees it; an over-long value is cut in the error.
func TestDirectIDsMustBeDirectMessages(t *testing.T) {
	teams := Record{System: Teams, Tenant: "t", Container: "19:a_b@unq.gbl.spaces", Kind: PrivateChannel, Text: "hi"}
	if err := teams.Validate(); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("a Teams one-to-one chat stored as a channel: %v", err)
	}
	teams.Kind = DM
	if err := teams.Validate(); err != nil {
		t.Error(err)
	}
	slack := rec(GroupDM, "D0123")
	if err := slack.Validate(); err != nil {
		t.Errorf("a Slack IM as a group DM: %v", err)
	}
	long := rec(PublicChannel, strings.Repeat("x/", 200))
	if err := long.Validate(); err == nil || len(err.Error()) > 200 {
		t.Errorf("error = %v", err)
	}
}

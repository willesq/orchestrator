package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// logFake is what fakeClient's VerifyToolLog answers and records.
var logFake struct {
	req *orchdv1.VerifyToolLogRequest
	rep *orchdv1.VerifyToolLogResponse
	err error
}

func (f *fakeClient) VerifyToolLog(_ context.Context, req *orchdv1.VerifyToolLogRequest, _ ...grpc.CallOption) (*orchdv1.VerifyToolLogResponse, error) {
	logFake.req = req
	return logFake.rep, logFake.err
}

func runLogVerify(t *testing.T, rep *orchdv1.VerifyToolLogResponse, err error, owner string) (string, error) {
	t.Helper()
	logFake.req, logFake.rep, logFake.err = nil, rep, err
	t.Cleanup(func() { logFake.req, logFake.rep, logFake.err = nil, nil, nil })
	out, runErr := run(factory(&fakeClient{}, nil), "", "spaces", "log", "verify", owner)
	if logFake.req == nil || logFake.req.GetOwner() != owner {
		t.Errorf("VerifyToolLog request = %v, want owner %q", logFake.req, owner)
	}
	return out, runErr
}

func TestSpacesLogVerify(t *testing.T) {
	tests := []struct {
		name    string
		rep     *orchdv1.VerifyToolLogResponse
		want    []string
		wantErr bool
	}{
		{"intact", &orchdv1.VerifyToolLogResponse{Records: 12, Anchors: 1, Ends: 1},
			[]string{"sess-1: 12 record(s), 1 chain head(s) anchored at SessionEnd", "chain intact"}, false},
		{"gaps", &orchdv1.VerifyToolLogResponse{Records: 5, Gaps: 2, Dropped: 7},
			[]string{"2 gap record(s): 7 event(s) were dropped", "chain intact"}, false},
		{"empty", &orchdv1.VerifyToolLogResponse{}, []string{"sess-1: no records"}, false},
		{"pruned", &orchdv1.VerifyToolLogResponse{Records: 2, Anchors: 1, Pruned: 9},
			[]string{"sess-1: 2 record(s)", "records up to 9: deleted by retention", "chain intact"}, false},
		{"pruned whole", &orchdv1.VerifyToolLogResponse{Anchors: 1, Pruned: 6},
			[]string{"sess-1: 0 record(s)", "records up to 6: deleted by retention", "chain intact"}, false},
		{"broken", &orchdv1.VerifyToolLogResponse{Records: 4, Problems: []*orchdv1.ToolLogProblem{
			{Seq: 3, Detail: "content does not match its hash (modified or moved)"},
			{Seq: 5, Detail: "record 4 before it is missing"}}},
			[]string{"chain broken: 2 problem(s)", "record 3: content does not match", "record 5: record 4 before it is missing"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runLogVerify(t, tt.rep, nil, "sess-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tt.wantErr, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
	if _, err := runLogVerify(t, nil, errors.New("only the operator can read the command log"), "task:task-1"); err == nil ||
		!strings.Contains(err.Error(), "operator") {
		t.Errorf("refusal = %v", err)
	}
}

// eventsFake is what fakeClient's ListToolEvents and SummarizeToolLog answer
// and record.
var eventsFake struct {
	reqs    []*orchdv1.ListToolEventsRequest
	pages   []*orchdv1.ListToolEventsResponse
	err     error
	sumReq  *orchdv1.SummarizeToolLogRequest
	summary *orchdv1.ToolLogSummary
}

func (f *fakeClient) ListToolEvents(_ context.Context, req *orchdv1.ListToolEventsRequest, _ ...grpc.CallOption) (*orchdv1.ListToolEventsResponse, error) {
	eventsFake.reqs = append(eventsFake.reqs, req)
	if eventsFake.err != nil {
		return nil, eventsFake.err
	}
	page := eventsFake.pages[0]
	eventsFake.pages = eventsFake.pages[1:]
	return page, nil
}

func (f *fakeClient) SummarizeToolLog(_ context.Context, req *orchdv1.SummarizeToolLogRequest, _ ...grpc.CallOption) (*orchdv1.ToolLogSummary, error) {
	eventsFake.sumReq = req
	return eventsFake.summary, eventsFake.err
}

func resetEventsFake(t *testing.T) {
	t.Helper()
	reset := func() {
		eventsFake.reqs, eventsFake.pages, eventsFake.err = nil, nil, nil
		eventsFake.sumReq, eventsFake.summary = nil, nil
	}
	reset()
	t.Cleanup(reset)
}

func TestSpacesLogShowsEvents(t *testing.T) {
	resetEventsFake(t)
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.Local).UnixNano()
	eventsFake.pages = []*orchdv1.ListToolEventsResponse{
		{Owner: "sess-1", Pruned: 2, More: true, Events: []*orchdv1.ToolEvent{
			{Seq: 3, At: at, Kind: "event", Event: "pre-tool-use", Tool: "Bash", Decision: "logged", Command: "git status"},
			{Seq: 4, At: at, Kind: "event", Event: "cwd-changed", Decision: "logged", OldCwd: "/w", NewCwd: "/w/sub"},
		}},
		{Owner: "sess-1", Events: []*orchdv1.ToolEvent{
			{Seq: 5, At: at, Kind: "gap", Dropped: 7},
			{Seq: 6, At: at, Kind: "end"},
		}},
	}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "sess-1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, w := range []string{"records up to 2 were deleted by retention", "SEQ", "2026-11-10 09:30:00",
		"pre-tool-use", "git status", "/w -> /w/sub", "7 event(s) dropped", "end"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if len(eventsFake.reqs) != 2 || eventsFake.reqs[0].GetOwner() != "sess-1" || eventsFake.reqs[1].GetAfterSeq() != 4 {
		t.Errorf("requests = %v, want two pages of sess-1, the second after 4", eventsFake.reqs)
	}
}

func TestSpacesLogOwnAndLimit(t *testing.T) {
	resetEventsFake(t)
	eventsFake.pages = []*orchdv1.ListToolEventsResponse{{Owner: "sess-me", More: true, Events: []*orchdv1.ToolEvent{
		{Seq: 11, Kind: "event", Event: "pre-tool-use", Tool: "Read", Command: "{\"file_path\":\"/x\"}"},
		{Seq: 12, Kind: "event", Event: "pre-tool-use", Tool: "Read", Command: "{}"},
	}}}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "--after", "10", "--limit", "2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r := eventsFake.reqs; len(r) != 1 || r[0].GetOwner() != "" || r[0].GetAfterSeq() != 10 || r[0].GetLimit() != 2 {
		t.Errorf("requests = %v, want one for the caller's own log after 10, limit 2", r)
	}
	if !strings.Contains(out, "file_path") {
		t.Errorf("output:\n%s", out)
	}

	// A page that says more but holds nothing ends the listing.
	resetEventsFake(t)
	eventsFake.pages = []*orchdv1.ListToolEventsResponse{
		{Owner: "sess-me", More: true, Events: []*orchdv1.ToolEvent{{Seq: 1, Kind: "end"}}},
		{Owner: "sess-me", More: true},
	}
	if out, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "sess-me"); err != nil || len(eventsFake.reqs) != 2 {
		t.Errorf("empty page with more: %v, %d requests\n%s", err, len(eventsFake.reqs), out)
	}

	resetEventsFake(t)
	eventsFake.pages = []*orchdv1.ListToolEventsResponse{{Owner: "task:t-1"}}
	if out, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "task:t-1"); err != nil || !strings.Contains(out, "task:t-1: no records") {
		t.Errorf("empty log: %v\n%s", err, out)
	}

	resetEventsFake(t)
	eventsFake.err = errors.New("a session can read only its own command log")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "sess-other"); err == nil || !strings.Contains(err.Error(), "only its own") {
		t.Errorf("refusal = %v", err)
	}
}

func TestSpacesLogSummary(t *testing.T) {
	resetEventsFake(t)
	eventsFake.summary = &orchdv1.ToolLogSummary{Since: 1, Until: 2, UnplacedMoves: 3, Spaces: []*orchdv1.SpaceToolActivity{
		{Space: "", Sessions: 1, ToolCalls: 4},
		{Space: "acme", Sessions: 2, ToolCalls: 40, DirectoryMoves: 6, CrossSpaceMoves: 2, Refusals: 1},
	}}
	before := time.Now()
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "summary")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := eventsFake.sumReq.GetSince(); got < before.Add(-7*24*time.Hour-time.Second).Unix() || got > before.Add(-7*24*time.Hour+time.Minute).Unix() {
		t.Errorf("since = %d, want a week ago", got)
	}
	for _, w := range []string{"SPACE", "INTO ANOTHER SPACE", "acme   2         40          6                2                   1",
		"3 directory move(s) were not checked"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "summary", "--since", "24h"); err != nil {
		t.Fatal(err)
	}
	if got := eventsFake.sumReq.GetSince(); got < time.Now().Add(-25*time.Hour).Unix() {
		t.Errorf("--since 24h sent %d", got)
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "summary", "--since", "0s"); err == nil {
		t.Error("--since 0s accepted")
	}

	eventsFake.summary = &orchdv1.ToolLogSummary{}
	if out, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "summary"); err != nil || !strings.Contains(out, "no tool events") {
		t.Errorf("empty summary: %v\n%s", err, out)
	}
	eventsFake.err = errors.New("only the operator can summarize the command log")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "log", "summary"); err == nil {
		t.Error("refusal not returned")
	}
}

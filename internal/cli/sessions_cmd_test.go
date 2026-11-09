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

// sessionsFake is what fakeClient's ListSessions answers and records.
var sessionsFake struct {
	req *orchdv1.ListSessionsRequest
	rep *orchdv1.ListSessionsResponse
	err error
}

func (f *fakeClient) ListSessions(_ context.Context, req *orchdv1.ListSessionsRequest, _ ...grpc.CallOption) (*orchdv1.ListSessionsResponse, error) {
	sessionsFake.req = req
	return sessionsFake.rep, sessionsFake.err
}

func runSessionsList(t *testing.T, rep *orchdv1.ListSessionsResponse, err error, args ...string) (string, error) {
	t.Helper()
	sessionsFake.req, sessionsFake.rep, sessionsFake.err = nil, rep, err
	t.Cleanup(func() { sessionsFake.req, sessionsFake.rep, sessionsFake.err = nil, nil, nil })
	return run(factory(&fakeClient{}, nil), "", append([]string{"sessions", "list"}, args...)...)
}

func TestSessionsList(t *testing.T) {
	at := time.Date(2026, 11, 9, 14, 5, 0, 0, time.Local)
	rep := &orchdv1.ListSessionsResponse{Sessions: []*orchdv1.SessionInfo{
		{Harness: "claude", SessionId: "sess-1", HarnessPid: 400, Space: "acme", FirstCwd: "/code/company1/app",
			State: "active", LastActivity: at.Unix(), ToolEvents: 12, Refusals: 1},
		{Harness: "codex", HarnessPid: 410, State: "unpinned"},
		// A directory name may hold any character; it must not drive the
		// terminal or break the table.
		{Harness: "claude", SessionId: "sess-2", HarnessPid: 420, FirstCwd: "/tmp/a\tb\n\x1b]52;c;evil\x07",
			State: "idle", LastActivity: at.Unix()},
	}}
	out, err := runSessionsList(t, rep, nil, "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !sessionsFake.req.GetAll() {
		t.Errorf("--all was not sent: %v", sessionsFake.req)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("output:\n%s", out)
	}
	for i, want := range []string{
		"HARNESS SESSION PID SPACE DIRECTORY STATE LAST ACTIVITY EVENTS REFUSALS",
		"claude sess-1 400 acme /code/company1/app active 2026-11-09 14:05 12 1",
		"codex - 410 - - unpinned - - -",
	} {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != want {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
	}
	if strings.ContainsAny(lines[3], "\x1b\x07") || !strings.Contains(lines[3], "/tmp/a b") {
		t.Errorf("unsafe directory cell: %q", lines[3])
	}

	if _, err := runSessionsList(t, &orchdv1.ListSessionsResponse{}, nil); err != nil || sessionsFake.req.GetAll() {
		t.Errorf("default request = %v, %v", sessionsFake.req, err)
	}
	if out, _ := runSessionsList(t, &orchdv1.ListSessionsResponse{}, nil); strings.TrimSpace(out) != "no sessions" {
		t.Errorf("empty list output = %q", out)
	}
	if _, err := runSessionsList(t, nil, errors.New("session pins are unavailable")); err == nil {
		t.Error("an orchd error was not returned")
	}
}

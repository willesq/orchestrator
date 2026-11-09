package hook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func (f *fakeClient) VerifyToolLog(context.Context, *orchdv1.VerifyToolLogRequest, ...grpc.CallOption) (*orchdv1.VerifyToolLogResponse, error) {
	return &orchdv1.VerifyToolLogResponse{}, nil
}

func (f *fakeClient) RecordToolEvent(context.Context, *orchdv1.RecordToolEventRequest, ...grpc.CallOption) (*orchdv1.RecordToolEventResponse, error) {
	return &orchdv1.RecordToolEventResponse{}, f.err
}

// toolEventClient records the tool events a hook hands to orchd.
type toolEventClient struct {
	*fakeClient
	events []*orchdv1.RecordToolEventRequest
	// block makes each call wait for its deadline, like a hung orchd.
	block bool
}

func (c *toolEventClient) RecordToolEvent(ctx context.Context, req *orchdv1.RecordToolEventRequest, _ ...grpc.CallOption) (*orchdv1.RecordToolEventResponse, error) {
	c.events = append(c.events, req)
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &orchdv1.RecordToolEventResponse{}, c.err
}

type fixture struct {
	harness, event, scenario string
	payload                  json.RawMessage
}

// readFixtures returns every payload SP-001 recorded from a real harness.
func readFixtures(t *testing.T) []fixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "payloads", "*.jsonl"))
	if err != nil || len(files) < 2 {
		t.Fatalf("want fixtures for Claude Code and Codex, got %v (%v)", files, err)
	}
	var out []fixture
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var rec struct {
				Harness  string          `json:"harness"`
				Event    string          `json:"event"`
				Scenario string          `json:"scenario"`
				Payload  json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			out = append(out, fixture{rec.Harness, rec.Event, rec.Scenario, rec.Payload})
		}
		f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestToolHooksHandEveryFixtureToOrchd is SP-012.1.a1: pre-tool-use and
// cwd-changed parse every recorded PreToolUse and CwdChanged payload of
// both harnesses, hand orchd one event carrying what the harness sent, and
// allow the call: they print nothing, which no harness reads as a deny.
func TestToolHooksHandEveryFixtureToOrchd(t *testing.T) {
	slugs := map[string]string{"PreToolUse": "pre-tool-use", "CwdChanged": "cwd-changed"}
	seen := map[string]int{}
	for _, fx := range readFixtures(t) {
		slug, ok := slugs[fx.event]
		if !ok {
			continue
		}
		seen[fx.harness+"/"+fx.event]++
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(fx.payload, &raw); err != nil {
			t.Fatalf("%s %s: %v", fx.harness, fx.scenario, err)
		}
		str := func(k string) string {
			var s string
			_ = json.Unmarshal(raw[k], &s)
			return s
		}
		c := &toolEventClient{fakeClient: &fakeClient{}}
		var out bytes.Buffer
		before := time.Now()
		if err := run([]string{slug}, bytes.NewReader(fx.payload), &out, factory(c, nil)); err != nil {
			t.Fatalf("%s %s: %v", fx.harness, fx.scenario, err)
		}
		if out.Len() != 0 {
			t.Errorf("%s %s: printed %q; the guard is off, so it must print nothing", fx.harness, fx.scenario, out.String())
		}
		if len(c.events) != 1 {
			t.Fatalf("%s %s: handed %d events to orchd, want 1", fx.harness, fx.scenario, len(c.events))
		}
		got := c.events[0]
		want := &orchdv1.RecordToolEventRequest{
			Event: slug, SessionId: str("session_id"), Cwd: str("cwd"), ToolName: str("tool_name"),
			ToolUseId: str("tool_use_id"), AgentId: str("agent_id"), AgentType: str("agent_type"),
			OldCwd: str("old_cwd"), NewCwd: str("new_cwd"),
		}
		if got.Event != want.Event || got.SessionId != want.SessionId || got.Cwd != want.Cwd ||
			got.ToolName != want.ToolName || got.ToolUseId != want.ToolUseId || got.AgentId != want.AgentId ||
			got.AgentType != want.AgentType || got.OldCwd != want.OldCwd || got.NewCwd != want.NewCwd {
			t.Errorf("%s %s:\n got %v\nwant %v", fx.harness, fx.scenario, got, want)
		}
		if want.SessionId == "" || want.Cwd == "" {
			t.Errorf("%s %s: fixture has no session_id or cwd", fx.harness, fx.scenario)
		}
		if in, ok := raw["tool_input"]; ok {
			if !jsonEqual(t, in, got.ToolInput) || got.ToolInputSize != int64(len(got.ToolInput)) {
				t.Errorf("%s %s: tool_input %s handed off as %s (size %d)", fx.harness, fx.scenario, in, got.ToolInput, got.ToolInputSize)
			}
		} else if slug == "pre-tool-use" {
			t.Errorf("%s %s: PreToolUse fixture without tool_input", fx.harness, fx.scenario)
		}
		if at := time.Unix(0, got.At); at.Before(before.Add(-time.Second)) || at.After(time.Now()) {
			t.Errorf("%s %s: at %v is not when the hook ran", fx.harness, fx.scenario, at)
		}
	}
	// Codex has no CwdChanged event (docs/internals/hooks.md).
	for _, k := range []string{"claude-code/PreToolUse", "claude-code/CwdChanged", "codex/PreToolUse"} {
		if seen[k] == 0 {
			t.Errorf("no %s fixtures", k)
		}
	}
	t.Logf("fixtures handed off: %v", seen)
}

// TestToolHookSendsLargeInputAsSize: a tool input over 1 MiB is handed off as
// its size alone, so one call cannot make the hand-off slow.
func TestToolHookSendsLargeInputAsSize(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"file_path": "/x", "content": strings.Repeat("a", maxToolInput)})
	c := &toolEventClient{fakeClient: &fakeClient{}}
	dispatch("pre-tool-use", Payload{SessionID: "s", ToolName: "Write", ToolInput: big}, &bytes.Buffer{}, factory(c, nil))
	if len(c.events) != 1 || c.events[0].ToolInput != nil || c.events[0].ToolInputSize != int64(len(big)) {
		t.Fatalf("events = %v", c.events)
	}
}

// TestToolHooksAlwaysAllow is SP-012.1.a3: with the guard off, a panic, bad
// JSON, or an orchd that is unreachable, refusing or hung still allows the
// tool call: the hook returns nil, prints nothing, and returns promptly.
func TestToolHooksAlwaysAllow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		cf   clientFactory
		// handed is how many events must still reach orchd.
		client *toolEventClient
		handed int
	}{
		{name: "panic", in: `{"session_id":"s","tool_name":"Bash"}`, cf: func() (orchdv1.OrchdClient, func(), error) {
			panic("boom")
		}},
		{name: "bad JSON", in: `{"session_id": not json`, client: &toolEventClient{fakeClient: &fakeClient{}}, handed: 1},
		{name: "unreachable orchd", in: `{"session_id":"s","tool_name":"Bash"}`, cf: func() (orchdv1.OrchdClient, func(), error) {
			return orchdclient.Dial()
		}},
		{name: "dial fails", in: `{"session_id":"s"}`, cf: factory(nil, errors.New("down"))},
		{name: "orchd refuses", in: `{"session_id":"s"}`, client: &toolEventClient{fakeClient: &fakeClient{err: errors.New("denied")}}, handed: 1},
		{name: "orchd hangs", in: `{"session_id":"s"}`, client: &toolEventClient{fakeClient: &fakeClient{}, block: true}, handed: 1},
		{name: "empty input", in: ``, client: &toolEventClient{fakeClient: &fakeClient{}}, handed: 1},
		{name: "payload over 8 MiB", in: `{"session_id":"s","tool_input":{"content":"` + strings.Repeat("a", maxPayload) + `"}}`,
			client: &toolEventClient{fakeClient: &fakeClient{}}, handed: 1},
	}
	t.Setenv("ORCH_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	for _, tc := range cases {
		for _, slug := range []string{"pre-tool-use", "cwd-changed"} {
			t.Run(tc.name+"/"+slug, func(t *testing.T) {
				cf := tc.cf
				if tc.client != nil {
					tc.client.events = nil
					cf = factory(tc.client, nil)
				}
				var out bytes.Buffer
				start := time.Now()
				if err := run([]string{slug}, strings.NewReader(tc.in), &out, cf); err != nil {
					t.Fatalf("err = %v", err)
				}
				if took := time.Since(start); took > 10*toolEventTimeout {
					t.Errorf("took %v", took)
				}
				if out.Len() != 0 {
					t.Errorf("printed %q", out.String())
				}
				if tc.client != nil && len(tc.client.events) != tc.handed {
					t.Errorf("handed %d events, want %d", len(tc.client.events), tc.handed)
				}
			})
		}
	}
}

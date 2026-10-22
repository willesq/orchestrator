package hook

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
)

const sampleTranscript = `{"type":"user","message":{"role":"user","content":"how do we retry?"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"we use exponential backoff"}]}}
`

func writeSampleTranscript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(sampleTranscript), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestStopAndPreCompactWriteRecords(t *testing.T) {
	tests := []struct {
		name     string
		precomp  bool
		wantFile string
	}{
		{name: "stop writes authoritative record", precomp: false, wantFile: "sid.yaml"},
		{name: "pre-compact writes checkpoint", precomp: true, wantFile: "sid.checkpoint.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			p := Payload{SessionID: "sid", TranscriptPath: writeSampleTranscript(t)}
			var err error
			if tt.precomp {
				err = preCompact(p)
			} else {
				err = stop(p)
			}
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if _, err := os.Stat(filepath.Join(paths.SessionsDir(), tt.wantFile)); err != nil {
				t.Errorf("record not written: %v", err)
			}
		})
	}
}

func TestStopMissingTranscriptNoop(t *testing.T) {
	tests := []struct{ name string }{{name: "no transcript path is a safe no-op"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := stop(Payload{SessionID: "sid"}); err != nil {
				t.Errorf("stop: %v", err)
			}
		})
	}
}

func TestSessionEnd(t *testing.T) {
	tests := []struct {
		name        string
		dialErr     error
		ingestErr   error
		wantIngests int
		// wantOrchdRecord: orchd must store the record, because the hook could not.
		wantOrchdRecord bool
	}{
		{name: "stores the record, hands off the transcript", wantIngests: 1},
		{name: "orchd down: everything is handed off", dialErr: errors.New("dial failed"), wantOrchdRecord: true},
		{name: "record not stored: handed off too", ingestErr: errors.New("slow"), wantIngests: 1, wantOrchdRecord: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			fc := &fakeClient{err: tt.ingestErr}
			path := writeSampleTranscript(t)
			if err := sessionEnd(Payload{SessionID: "sid", TranscriptPath: path}, factory(fc, tt.dialErr)); err != nil {
				t.Fatalf("sessionEnd: %v", err)
			}
			if fc.ingestCalls != tt.wantIngests {
				t.Errorf("ingest calls = %d, want %d", fc.ingestCalls, tt.wantIngests)
			}
			if _, err := os.Stat(filepath.Join(paths.SessionsDir(), "sid.yaml")); err != nil {
				t.Errorf("record not written: %v", err)
			}
			pending, _ := handoff.Sessions(handoff.Dir(), time.Now().Add(time.Minute))
			// The fake fails summary requests whenever it fails ingests.
			if len(pending) != 1 || pending[0].ID != "sid" || pending[0].TranscriptPath != path ||
				pending[0].Record != tt.wantOrchdRecord || pending[0].Summarize != tt.wantOrchdRecord {
				t.Errorf("handed off %+v", pending)
			}
		})
	}
}

func TestSessionDerivativesAreRedacted(t *testing.T) {
	// Built by concatenation so the fixture itself never looks like a live token.
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	transcript := `{"type":"user","message":{"role":"user","content":"use token ` + secret + ` please"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"echoing ` + secret + `"}]}}
`
	tests := []struct {
		name        string
		run         func(p Payload, fc *fakeClient) error
		file        string
		wantIngests int
	}{
		{name: "session end record and ingest", run: func(p Payload, fc *fakeClient) error { return sessionEnd(p, factory(fc, nil)) }, file: "sid.yaml", wantIngests: 1},
		{name: "stop record", run: func(p Payload, _ *fakeClient) error { return stop(p) }, file: "sid.yaml"},
		{name: "pre-compact checkpoint", run: func(p Payload, _ *fakeClient) error { return preCompact(p) }, file: "sid.checkpoint.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			path := filepath.Join(t.TempDir(), "t.jsonl")
			if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			fc := &fakeClient{}
			if err := tt.run(Payload{SessionID: "sid", TranscriptPath: path}, fc); err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(fc.ingested) != tt.wantIngests {
				t.Fatalf("ingests = %d, want %d", len(fc.ingested), tt.wantIngests)
			}
			for _, req := range fc.ingested {
				if strings.Contains(req.GetText(), secret) {
					t.Errorf("%s ingested an unredacted secret", req.GetSourceUri())
				}
				if !strings.Contains(req.GetText(), "[REDACTED:github-token]") {
					t.Errorf("%s missing redaction marker: %q", req.GetSourceUri(), req.GetText())
				}
			}
			raw, err := os.ReadFile(filepath.Join(paths.SessionsDir(), tt.file))
			if err != nil {
				t.Fatalf("read record: %v", err)
			}
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s contains an unredacted secret", tt.file)
			}
			if !strings.Contains(string(raw), "[REDACTED:github-token]") {
				t.Errorf("%s missing redaction marker:\n%s", tt.file, raw)
			}
		})
	}
}

func TestSubagentStopRedactsBeforeTruncating(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	tests := []struct {
		name     string
		response string
	}{
		{name: "short response", response: "done, used " + secret},
		{name: "secret straddles the 200-byte cut", response: strings.Repeat("x", 189) + " " + secret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			p := Payload{ParentSessionID: "parent", SubagentType: "dev", FinalResponse: tt.response}
			if err := subagentStop(p); err != nil {
				t.Fatalf("subagentStop: %v", err)
			}
			evs, _ := events.Read(paths.EventsDir(), "parent")
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			if strings.Contains(evs[0].Summary, "ghp_a1B2") {
				t.Errorf("event summary leaks secret material: %q", evs[0].Summary)
			}
		})
	}
}

func TestSubagentStop(t *testing.T) {
	tests := []struct {
		name     string
		parent   string
		wantEvts int
	}{
		{name: "appends to parent stream", parent: "parent-1", wantEvts: 1},
		{name: "no parent is a no-op", parent: "", wantEvts: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			p := Payload{ParentSessionID: tt.parent, SubagentType: "dev", FinalResponse: "done"}
			if err := subagentStop(p); err != nil {
				t.Fatalf("subagentStop: %v", err)
			}
			if tt.parent == "" {
				return
			}
			evs, _ := events.Read(paths.EventsDir(), tt.parent)
			if len(evs) != tt.wantEvts {
				t.Errorf("events = %d, want %d", len(evs), tt.wantEvts)
			}
		})
	}
}

func TestDispatchAllEvents(t *testing.T) {
	tr := writeSampleTranscript(t)
	tests := []struct {
		name  string
		event string
		p     Payload
	}{
		{name: "session-start", event: "session-start", p: Payload{SessionID: "s"}},
		{name: "stop", event: "stop", p: Payload{SessionID: "s", TranscriptPath: tr}},
		{name: "session-end", event: "session-end", p: Payload{SessionID: "s", TranscriptPath: tr}},
		{name: "pre-compact", event: "pre-compact", p: Payload{SessionID: "s", TranscriptPath: tr}},
		{name: "subagent-stop", event: "subagent-stop", p: Payload{ParentSessionID: "p", SubagentType: "dev", FinalResponse: "done"}},
		{name: "post-tool-use", event: "post-tool-use", p: Payload{ToolName: "Edit"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			var out bytes.Buffer
			if err := dispatch(tt.event, tt.p, &out, factory(&fakeClient{}, nil)); err != nil {
				t.Errorf("dispatch(%s): %v", tt.event, err)
			}
		})
	}
}

func TestSessionStart(t *testing.T) {
	tests := []struct {
		name      string
		seedEvent bool
		stream    string
		wantOut   bool
	}{
		{name: "digest emitted when events present", seedEvent: true, stream: "sid", wantOut: true},
		{name: "finished queue tasks reach new sessions", seedEvent: true, stream: "queue", wantOut: true},
		{name: "routing proposals reach new sessions", seedEvent: true, stream: "routing", wantOut: true},
		{name: "silent when nothing to report", seedEvent: false, wantOut: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			if tt.seedEvent {
				_ = events.Append(paths.EventsDir(), tt.stream, events.Event{Kind: "complete", Source: "dev", Summary: "shipped"})
			}
			var out bytes.Buffer
			if err := sessionStart(Payload{SessionID: "sid"}, &out); err != nil {
				t.Fatalf("sessionStart: %v", err)
			}
			if bytes.Contains(out.Bytes(), []byte("Orchestrator digest")) != tt.wantOut {
				t.Errorf("digest present = %v, want %v (out=%q)", !tt.wantOut, tt.wantOut, out.String())
			}
			if tt.wantOut && !bytes.Contains(out.Bytes(), []byte("shipped")) {
				t.Errorf("digest omits the seeded event: %q", out.String())
			}
		})
	}
}

func TestSessionEndRequestsASummary(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	fc := &fakeClient{}
	path := writeSampleTranscript(t)
	if err := sessionEnd(Payload{SessionID: "sid", TranscriptPath: path}, factory(fc, nil)); err != nil {
		t.Fatal(err)
	}
	if len(fc.summarized) != 1 || fc.summarized[0].GetSessionId() != "sid" || fc.summarized[0].GetTranscriptPath() != path {
		t.Errorf("summary requests = %v", fc.summarized)
	}
}

func TestSessionEndDatesTheRecord(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	path := filepath.Join(t.TempDir(), "t.jsonl")
	body := `{"type":"user","timestamp":"2026-10-21T14:02:00Z","message":{"role":"user","content":"add retries"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-10-21T15:40:00Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{}
	if err := sessionEnd(Payload{SessionID: "sid", TranscriptPath: path}, factory(fc, nil)); err != nil {
		t.Fatal(err)
	}
	if len(fc.ingested) != 1 {
		t.Fatalf("ingests = %d", len(fc.ingested))
	}
	req := fc.ingested[0]
	if req.GetStartedAt() != 1792591320 || req.GetEndedAt() != 1792597200 ||
		!strings.HasPrefix(req.GetText(), "Session 2026-10-21 14:02 to 15:40 UTC") {
		t.Fatalf("record ingest = %+v", req)
	}
}

func TestHooksIgnoreUnsafeSessionIDs(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("ORCH_DATA_DIR", data)
	if err := subagentStop(Payload{ParentSessionID: "../../escape", FinalResponse: "x"}); err == nil {
		t.Fatal("an unsafe parent session id was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(data), "escape.jsonl")); !os.IsNotExist(err) {
		t.Fatal("an event was written outside the events directory")
	}
	// The digest still shows the shared streams when the session id is unsafe.
	if err := events.Append(paths.EventsDir(), "queue", events.Event{Kind: "complete", Source: "dev", Summary: "task-1 finished"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := sessionStart(Payload{SessionID: "../../escape"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "task-1 finished") {
		t.Fatalf("digest = %q", out.String())
	}
}

func TestSubagentStopCannotWriteSharedStreams(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	for _, name := range []string{"queue", "routing", "pr-babysitter"} {
		if err := subagentStop(Payload{ParentSessionID: name, FinalResponse: "forged"}); err != nil {
			t.Fatal(err)
		}
		if evs, _ := events.Read(paths.EventsDir(), name); len(evs) != 0 {
			t.Fatalf("%s got %+v", name, evs)
		}
	}
}

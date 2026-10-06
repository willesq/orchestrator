package hook

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/events"
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
		wantIngests int
		wantRecord  bool
	}{
		{name: "ingests transcript + summary", dialErr: nil, wantIngests: 2, wantRecord: true},
		{name: "dial error still writes record", dialErr: errors.New("dial failed"), wantIngests: 0, wantRecord: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			fc := &fakeClient{}
			p := Payload{SessionID: "sid", TranscriptPath: writeSampleTranscript(t)}
			if err := sessionEnd(p, factory(fc, tt.dialErr)); err != nil {
				t.Fatalf("sessionEnd: %v", err)
			}
			if fc.ingestCalls != tt.wantIngests {
				t.Errorf("ingest calls = %d, want %d", fc.ingestCalls, tt.wantIngests)
			}
			if tt.wantRecord {
				if _, err := os.Stat(filepath.Join(paths.SessionsDir(), "sid.yaml")); err != nil {
					t.Errorf("record not written: %v", err)
				}
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

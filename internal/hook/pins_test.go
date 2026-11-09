package hook

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func (f *fakeClient) PinSession(context.Context, *orchdv1.PinSessionRequest, ...grpc.CallOption) (*orchdv1.SessionPin, error) {
	return &orchdv1.SessionPin{}, f.err
}

func (f *fakeClient) EndSession(context.Context, *orchdv1.EndSessionRequest, ...grpc.CallOption) (*orchdv1.EndSessionResponse, error) {
	return &orchdv1.EndSessionResponse{}, f.err
}

// pinClient records the pin calls a hook makes.
type pinClient struct {
	*fakeClient
	pins []*orchdv1.PinSessionRequest
	ends []*orchdv1.EndSessionRequest
}

func (c *pinClient) PinSession(_ context.Context, req *orchdv1.PinSessionRequest, _ ...grpc.CallOption) (*orchdv1.SessionPin, error) {
	c.pins = append(c.pins, req)
	return &orchdv1.SessionPin{}, c.err
}

func (c *pinClient) EndSession(_ context.Context, req *orchdv1.EndSessionRequest, _ ...grpc.CallOption) (*orchdv1.EndSessionResponse, error) {
	c.ends = append(c.ends, req)
	return &orchdv1.EndSessionResponse{}, c.err
}

func pinTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv(agentrun.TaskIDEnv, "")
}

// TestSessionStartPinsEverySource: each SessionStart source sends the same
// PinSession, naming its source; orchd decides to keep the existing pin
// (SP-010.2.a1) or, for a resume, to move it (SP-017.1.a4).
func TestSessionStartPinsEverySource(t *testing.T) {
	for _, source := range []string{"startup", "resume", "clear", "compact"} {
		t.Run(source, func(t *testing.T) {
			pinTestEnv(t)
			c := &pinClient{fakeClient: &fakeClient{}}
			p := Payload{SessionID: "sid", Cwd: "/work/repo", Source: source}
			if err := dispatch("session-start", p, &bytes.Buffer{}, factory(c, nil)); err != nil {
				t.Fatal(err)
			}
			if len(c.pins) != 1 || c.pins[0].GetSessionId() != "sid" || c.pins[0].GetCwd() != "/work/repo" || c.pins[0].GetParentSessionId() != "" ||
				c.pins[0].GetSource() != source {
				t.Fatalf("pin requests = %v", c.pins)
			}
		})
	}
}

// TestSessionStartPinsSubagentWithParent: a subagent's SessionStart names its
// parent, so orchd gives it the parent's pin (SP-010.2.a2).
func TestSessionStartPinsSubagentWithParent(t *testing.T) {
	pinTestEnv(t)
	c := &pinClient{fakeClient: &fakeClient{}}
	p := Payload{SessionID: "sub", ParentSessionID: "parent", Cwd: "/elsewhere"}
	if err := dispatch("session-start", p, &bytes.Buffer{}, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if len(c.pins) != 1 || c.pins[0].GetParentSessionId() != "parent" {
		t.Fatalf("pin requests = %v", c.pins)
	}
}

// TestSessionEndEndsPin: SessionEnd tells orchd the session ended, even with
// no transcript to record (SP-010.2.a3).
func TestSessionEndEndsPin(t *testing.T) {
	pinTestEnv(t)
	c := &pinClient{fakeClient: &fakeClient{}}
	if err := dispatch("session-end", Payload{SessionID: "sid"}, &bytes.Buffer{}, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if len(c.ends) != 1 || c.ends[0].GetSessionId() != "sid" {
		t.Fatalf("end requests = %v", c.ends)
	}
}

// TestPinHooksNeverBlock: a refused or unreachable orchd changes nothing the
// session sees, and queued tasks and ID-less payloads send nothing.
func TestPinHooksNeverBlock(t *testing.T) {
	pinTestEnv(t)
	refused := &pinClient{fakeClient: &fakeClient{err: errors.New("refused")}}
	for _, event := range []string{"session-start", "session-end"} {
		if err := dispatch(event, Payload{SessionID: "sid"}, &bytes.Buffer{}, factory(refused, nil)); err != nil {
			t.Errorf("%s with a refusing orchd = %v", event, err)
		}
		if err := dispatch(event, Payload{SessionID: "sid"}, &bytes.Buffer{}, factory(nil, errors.New("down"))); err != nil {
			t.Errorf("%s with orchd down = %v", event, err)
		}
	}
	if len(refused.pins) != 1 || len(refused.ends) != 1 {
		t.Fatalf("refused client saw %d pins, %d ends", len(refused.pins), len(refused.ends))
	}

	quiet := &pinClient{fakeClient: &fakeClient{}}
	_ = dispatch("session-start", Payload{}, &bytes.Buffer{}, factory(quiet, nil))
	_ = dispatch("session-end", Payload{}, &bytes.Buffer{}, factory(quiet, nil))
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	_ = dispatch("session-start", Payload{SessionID: "sid"}, &bytes.Buffer{}, factory(quiet, nil))
	_ = dispatch("session-end", Payload{SessionID: "sid"}, &bytes.Buffer{}, factory(quiet, nil))
	if len(quiet.pins) != 0 || len(quiet.ends) != 0 {
		t.Fatalf("pins = %v, ends = %v; want none", quiet.pins, quiet.ends)
	}
}

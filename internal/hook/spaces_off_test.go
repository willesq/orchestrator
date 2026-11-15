package hook

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// offClient records every spaces call a hook makes.
type offClient struct {
	*fakeClient
	events []*orchdv1.RecordToolEventRequest
	pins   []*orchdv1.PinSessionRequest
	ends   []*orchdv1.EndSessionRequest
}

func (c *offClient) RecordToolEvent(_ context.Context, req *orchdv1.RecordToolEventRequest, _ ...grpc.CallOption) (*orchdv1.RecordToolEventResponse, error) {
	c.events = append(c.events, req)
	return &orchdv1.RecordToolEventResponse{}, nil
}

func (c *offClient) PinSession(_ context.Context, req *orchdv1.PinSessionRequest, _ ...grpc.CallOption) (*orchdv1.SessionPin, error) {
	c.pins = append(c.pins, req)
	return &orchdv1.SessionPin{}, nil
}

func (c *offClient) EndSession(_ context.Context, req *orchdv1.EndSessionRequest, _ ...grpc.CallOption) (*orchdv1.EndSessionResponse, error) {
	c.ends = append(c.ends, req)
	return &orchdv1.EndSessionResponse{}, nil
}

// withSpacesOff makes the hooks read memory spaces as off (true) or on.
func withSpacesOff(t *testing.T, off bool) {
	t.Helper()
	old := spacesOff
	spacesOff = func() bool { return off }
	t.Cleanup(func() { spacesOff = old })
}

// TestSpacesOffFrom: orchd's snapshot decides; without a readable one,
// orch.db does, and a store that cannot be read is on (fail closed).
func TestSpacesOffFrom(t *testing.T) {
	on := func() (bool, error) { return true, nil }
	off := func() (bool, error) { return false, nil }
	broken := func() (bool, error) { return false, errors.New("locked") }
	write := func(t *testing.T, s spaces.GuardSnapshot) string {
		dir := t.TempDir()
		if err := spaces.WriteGuardSnapshot(dir, s); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	damaged := t.TempDir()
	if err := os.WriteFile(spaces.GuardSnapshotPath(damaged), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		dir     string
		enabled func() (bool, error)
		want    bool
	}{
		{"snapshot off", write(t, spaces.GuardSnapshot{SpacesOff: true}), on, true},
		{"snapshot on", write(t, spaces.GuardSnapshot{}), off, false},
		{"no snapshot, store off", t.TempDir(), off, true},
		{"no snapshot, store on", t.TempDir(), on, false},
		{"no snapshot, store unreadable", t.TempDir(), broken, false},
		{"damaged snapshot, store off", damaged, off, true},
		{"damaged snapshot, store unreadable", damaged, broken, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spacesOffFrom(tc.dir, tc.enabled); got != tc.want {
				t.Fatalf("spacesOffFrom = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpacesOffHooksArePreSpaces is the hook half of "disabled = pre-spaces
// behaviour": with a policy kept whose guard would refuse the call, and a
// guard that enforces, the tool hooks print nothing, call nothing and report
// no refusal; session-start pins nothing and adds no enforcement lines, and
// session-end does not end a pin.
func TestSpacesOffHooksArePreSpaces(t *testing.T) {
	guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside")
	setEnforcementFake(t, relaxedState(), nil)
	refused := 0
	old := appendRefusal
	appendRefusal = func(events.Event) error { refused++; return nil }
	t.Cleanup(func() { appendRefusal = old })
	withSpacesOff(t, true)

	c := &offClient{fakeClient: &fakeClient{}}
	for _, slug := range []string{"pre-tool-use", "cwd-changed"} {
		in := `{"session_id":"sess","cwd":"/scratch/project","old_cwd":"/scratch/project","new_cwd":"/scratch/outside",` +
			`"tool_name":"Bash","tool_input":{"command":"cat /scratch/outside/x"}}`
		var out bytes.Buffer
		if err := run([]string{slug}, strings.NewReader(in), &out, factory(c, nil)); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Errorf("%s printed %q", slug, out.String())
		}
	}
	if len(c.events) != 0 || refused != 0 {
		t.Fatalf("tool hooks handed %d event(s) and reported %d refusal(s)", len(c.events), refused)
	}

	p := Payload{SessionID: "sess", Cwd: "/scratch/project", Source: "startup"}
	var got, want bytes.Buffer
	if err := dispatch("session-start", p, &got, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(p, &want); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("session-start printed\n%s\nwant the plain digest\n%s", got.String(), want.String())
	}
	if len(c.pins) != 0 || enforcementFake.calls != 0 {
		t.Fatalf("session-start pinned %d time(s) and read the switches %d time(s)", len(c.pins), enforcementFake.calls)
	}
	if err := dispatch("session-end", p, &bytes.Buffer{}, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if len(c.ends) != 0 {
		t.Fatalf("session-end ended %d pin(s)", len(c.ends))
	}
}

// TestSpacesOnHooksStillGuard: the same calls with spaces on keep today's
// behaviour, so the switch is what changes it.
func TestSpacesOnHooksStillGuard(t *testing.T) {
	guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside")
	setEnforcementFake(t, relaxedState(), nil)
	old := appendRefusal
	appendRefusal = func(events.Event) error { return nil }
	t.Cleanup(func() { appendRefusal = old })
	withSpacesOff(t, false)
	c := &offClient{fakeClient: &fakeClient{}}
	in := `{"session_id":"sess","cwd":"/scratch/project","tool_name":"Bash","tool_input":{"command":"cat /scratch/outside/x"}}`
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, strings.NewReader(in), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := denyOf(t, out.Bytes()); !ok || len(c.events) != 1 {
		t.Fatalf("with spaces on: output %q, %d event(s)", out.String(), len(c.events))
	}
	if err := dispatch("session-start", Payload{SessionID: "sess", Cwd: "/scratch/project"}, &bytes.Buffer{}, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if len(c.pins) != 1 {
		t.Fatalf("with spaces on, session-start pinned %d time(s)", len(c.pins))
	}
}

// TestSpacesOffReadsOrchdsSnapshot: the real switch reads the snapshot orchd
// writes while spaces are off, so the hook does nothing even though
// orch.db cannot be read.
func TestSpacesOffReadsOrchdsSnapshot(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{SpacesOff: true, Home: "/Users/user"}); err != nil {
		t.Fatal(err)
	}
	old := spacesOff
	t.Cleanup(func() { spacesOff = old })
	spacesOff = func() bool {
		return spacesOffFrom(paths.GuardDir(), func() (bool, error) { return true, errors.New("unreadable") })
	}
	c := &offClient{fakeClient: &fakeClient{}}
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, strings.NewReader(`{"session_id":"s","tool_name":"Bash"}`), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || len(c.events) != 0 {
		t.Fatalf("output %q, %d event(s)", out.String(), len(c.events))
	}
}

// TestReadSpacesOffDefault: with no guard snapshot and no orch.db, memory
// spaces read as off, the default.
func TestReadSpacesOffDefault(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if !readSpacesOff() {
		t.Fatal("a fresh install reads spaces as on")
	}
}

package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// refusalMarker is in every refused call's command or content, so a test
// can tell whether any of the call's text reached the stream.
const refusalMarker = "marker-7f3a"

// spacesStream returns the operator's spaces stream, its raw bytes and
// its file mode (0 when it does not exist).
func spacesStream(t *testing.T) ([]events.Event, []byte, os.FileMode) {
	t.Helper()
	evs, err := events.Read(paths.EventsDir(), events.SpacesStream)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.EventsDir(), events.SpacesStream+".jsonl")
	raw, _ := os.ReadFile(path)
	var mode os.FileMode
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return evs, raw, mode
}

// hookCall runs one hook event with payload and returns whether a deny was
// printed.
func hookCall(t *testing.T, event string, payload any) bool {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{event}, bytes.NewReader(b), &out, factory(&toolEventClient{fakeClient: &fakeClient{}}, nil)); err != nil {
		t.Fatal(err)
	}
	_, refused := denyOf(t, out.Bytes())
	return refused
}

func bash(session, cwd, cmd string) map[string]any {
	return map[string]any{"session_id": session, "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]string{"command": cmd}}
}

// TestRefusalEventPerRefusal is SP-048.1.a1: every call the guard or the
// outbound terms check refuses puts exactly one event on the operator's
// spaces stream, naming the session, its space, the tool and the rule
// (and, for a term, the term's space, line and hash); the stream never
// holds the call's command, paths or content, nor the term. Calls that are
// allowed, or refused only in the log (guard logging, cwd-changed), emit
// none.
func TestRefusalEventPerRefusal(t *testing.T) {
	type want struct {
		session, space, tool string
		rule                 spaces.GuardRule
		term                 bool
	}
	cases := []struct {
		name  string
		setup func(t *testing.T) any // returns the payload
		want  *want                  // nil: no event
	}{
		{"another job's folder", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "/scratch/outside", "sess")
			return bash("sess", "/scratch/project", "cat /scratch/outside/"+refusalMarker+".txt")
		}, &want{"sess", "job", "Bash", spaces.RuleOtherJobPath, false}},
		{"another job's repo, strict", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside", "sess")
			return bash("sess", "/scratch/project", "gh api repos/otherorg/"+refusalMarker)
		}, &want{"sess", "job", "Bash", spaces.RuleOtherJobRepo, false}},
		{"self-protection, Write tool", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "", "sess")
			return map[string]any{"session_id": "sess", "cwd": "/scratch/project", "tool_name": "Write",
				"tool_input": map[string]string{"file_path": filepath.Join(paths.GuardDir(), "pin.json"), "content": refusalMarker}}
		}, &want{"sess", "job", "Write", spaces.RuleSelfProtection, false}},
		{"no pin", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "")
			return bash("unpinned", "/scratch/project", "echo "+refusalMarker)
		}, &want{"unpinned", "", "Bash", spaces.RulePinMissing, false}},
		{"unreadable policy", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchStrict, "/scratch/project", "", "sess")
			if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 7, Broken: true,
				Guard: spaces.GuardSwitch{State: spaces.SwitchStrict}}); err != nil {
				t.Fatal(err)
			}
			return bash("sess", "/scratch/project", "echo "+refusalMarker)
		}, &want{"sess", "", "Bash", spaces.RulePolicyUnreadable, false}},
		{"timeout", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "", "sess")
			stallDecision(t, spaces.GuardBlockOtherJobs)
			return bash("sess", "/scratch/project", "echo "+refusalMarker)
		}, &want{"sess", "", "Bash", spaces.RuleGuardTimeout, false}},
		{"outbound term", func(t *testing.T) any {
			dir := termsSetup(t, spaces.SwitchBlockOtherJobs)
			stageTerm(t, dir)
			return bash("s", dir, `git commit -m "`+refusalMarker+`"`)
		}, &want{"s", "acme", "Bash", spaces.RuleOutboundTerm, true}},
		{"outbound unchecked", func(t *testing.T) any {
			dir := termsSetup(t, spaces.SwitchLog)
			return bash("s", dir, `git commit -m "$(echo `+refusalMarker+`)"`)
		}, &want{"s", "acme", "Bash", spaces.RuleOutboundUnchecked, false}},
		{"allowed", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside", "sess")
			return bash("sess", "/scratch/project", "ls")
		}, nil},
		{"guard logging only", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchLog, "/scratch/project", "/scratch/outside", "sess")
			return bash("sess", "/scratch/project", "cat /scratch/outside/"+refusalMarker)
		}, nil},
		{"guard off", func(t *testing.T) any {
			guardSetup(t, spaces.SwitchOff, "/scratch/project", "/scratch/outside", "sess")
			return bash("sess", "/scratch/project", "cat /scratch/outside/"+refusalMarker)
		}, nil},
	}
	refusals := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.setup(t)
			refused := hookCall(t, "pre-tool-use", payload)
			if refused != (tc.want != nil) {
				t.Fatalf("refused = %v, want %v", refused, tc.want != nil)
			}
			evs, raw, mode := spacesStream(t)
			if tc.want == nil {
				if len(evs) != 0 {
					t.Fatalf("events %+v, want none", evs)
				}
				return
			}
			refusals++
			if len(evs) != 1 {
				t.Fatalf("%d events, want 1: %s", len(evs), raw)
			}
			e := evs[0]
			r := e.Refusal
			if e.Kind != events.KindRefusal || e.Source != "guard" || e.Timestamp == "" || r == nil {
				t.Fatalf("event %+v", e)
			}
			w := tc.want
			if r.Session != w.session || r.Space != w.space || r.Tool != w.tool || r.Rule != string(w.rule) {
				t.Errorf("refusal %+v, want %+v", *r, *w)
			}
			if w.term {
				if r.Term == nil || r.Term.Space != "globex" || r.Term.Line != 1 || len(r.Term.Hash) != 12 {
					t.Errorf("term %+v, want globex line 1 with its hash", r.Term)
				}
			} else if r.Term != nil {
				t.Errorf("term %+v on a %s refusal", r.Term, r.Rule)
			}
			for _, banned := range []string{refusalMarker, "/scratch", "zephyr", "Zephyr", "git commit", "echo", "cat ", "otherorg", "pin.json"} {
				if bytes.Contains(raw, []byte(banned)) {
					t.Errorf("stream holds %q: %s", banned, raw)
				}
			}
			if !strings.Contains(e.Summary, string(w.rule)) || !strings.Contains(e.Summary, w.tool) {
				t.Errorf("summary %q", e.Summary)
			}
			if mode != 0o600 {
				t.Errorf("stream mode %v, want 0600", mode)
			}
		})
	}
	if refusals < 8 {
		t.Errorf("%d refusals checked, want 8", refusals)
	}
}

// TestRefusalEventsAccumulate: refusals in a row each add their own event,
// and a cwd-changed into another job, which is never refused, adds none.
func TestRefusalEventsAccumulate(t *testing.T) {
	guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside", "sess")
	for i := 0; i < 3; i++ {
		if !hookCall(t, "pre-tool-use", bash("sess", "/scratch/project", "cat /scratch/outside/x")) {
			t.Fatal("not refused")
		}
	}
	hookCall(t, "cwd-changed", map[string]any{"session_id": "sess", "cwd": "/scratch/outside",
		"old_cwd": "/scratch/project", "new_cwd": "/scratch/outside"})
	if evs, raw, _ := spacesStream(t); len(evs) != 3 {
		t.Errorf("%d events, want 3: %s", len(evs), raw)
	}
}

// TestRefusalReportNeverChangesTheDeny: a stream that cannot be written,
// or a report that panics, leaves the refusal as it was.
func TestRefusalReportNeverChangesTheDeny(t *testing.T) {
	for name, fail := range map[string]func(events.Event) error{
		"error": func(events.Event) error { return os.ErrPermission },
		"panic": func(events.Event) error { panic("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "/scratch/outside", "sess")
			old := appendRefusal
			t.Cleanup(func() { appendRefusal = old })
			appendRefusal = fail
			if !hookCall(t, "pre-tool-use", bash("sess", "/scratch/project", "cat /scratch/outside/x")) {
				t.Error("not refused")
			}
		})
	}
}

// TestRefusalReportBounded: a refusal event write that stalls does not
// hold the hook past refusalReportTimeout, and the deny stands.
func TestRefusalReportBounded(t *testing.T) {
	guardSetup(t, spaces.SwitchBlockOtherJobs, "/scratch/project", "/scratch/outside", "sess")
	old := appendRefusal
	release := make(chan struct{})
	t.Cleanup(func() { close(release); appendRefusal = old })
	appendRefusal = func(events.Event) error { <-release; return nil }
	start := time.Now()
	if !hookCall(t, "pre-tool-use", bash("sess", "/scratch/project", "cat /scratch/outside/x")) {
		t.Error("not refused")
	}
	if took := time.Since(start); took > refusalReportTimeout+refusalEventTimeout+time.Second {
		t.Errorf("the hook took %v", took)
	}
}

// TestSessionNamedLikeSharedStream: a session whose ID is "spaces" (or
// another shared stream) never reads that stream as its own, so refusals
// of other sessions stay the operator's.
func TestSessionNamedLikeSharedStream(t *testing.T) {
	guardSetup(t, spaces.SwitchStrict, "/scratch/project", "/scratch/outside", "sess")
	if !hookCall(t, "pre-tool-use", bash("sess", "/scratch/project", "cat /scratch/outside/x")) {
		t.Fatal("not refused")
	}
	for id := range sharedStreams {
		if slices.Contains(updateStreams(id), id) && id != taskEventStream {
			t.Errorf("updateStreams(%q) = %v", id, updateStreams(id))
		}
	}
	var out bytes.Buffer
	if err := sessionStartWith(Payload{SessionID: events.SpacesStream}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "refused") {
		t.Errorf("digest of session %q shows the spaces stream: %s", events.SpacesStream, out.String())
	}
	st := sessionState{}
	markSeen(&st, events.SpacesStream)
	if err := events.AppendSpaces(paths.EventsDir(), events.NewRefusal(events.Refusal{Tool: "Bash", Rule: "other_job_path"}, time.Now())); err != nil {
		t.Fatal(err)
	}
	if lines := takeUpdates(&st, events.SpacesStream); len(lines) != 0 {
		t.Errorf("updates of session %q: %v", events.SpacesStream, lines)
	}
}

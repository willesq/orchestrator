package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// spacedEvents writes guard files for two job spaces and a public one, pins
// session "sid" to job, and returns after appending one event per space to
// the shared streams.
func spacedEvents(t *testing.T) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "job", Kind: spaces.Isolated, GitHubOrgs: []string{"joborg"}},
		{Name: "other", Kind: spaces.Isolated, GitHubOrgs: []string{"otherorg"}},
		{Name: "pub", Kind: spaces.Public, GitHubOrgs: []string{"puborg"}},
	}}
	if err := spaces.WriteGuardSnapshot(paths.GuardDir(), spaces.GuardSnapshot{Revision: 3, Policy: &p, Home: "/Users/user",
		Protection: spaces.Protection{Write: []string{paths.GuardDir()}}}); err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteGuardPin(paths.GuardDir(), spaces.GuardPinFile{SessionID: "sid", Space: "job", Revision: 3}); err != nil {
		t.Fatal(err)
	}
}

func emitIn(t *testing.T, stream, space, summary string) {
	t.Helper()
	if err := events.Append(paths.EventsDir(), stream, events.Event{Kind: "info", Source: "x", Summary: summary, Space: space}); err != nil {
		t.Fatal(err)
	}
}

// emitEverySpace appends to each shared stream an event of each space, one
// with none, and one routing event.
func emitEverySpace(t *testing.T, tag string) {
	t.Helper()
	for _, stream := range []string{"pr-babysitter", "queue"} {
		for _, sp := range []string{"job", "other", "pub", spaces.Quarantine, ""} {
			emitIn(t, stream, sp, tag+" "+stream+" of space-"+sp+"-end")
		}
	}
	emitIn(t, "routing", "", tag+" routing change")
}

// TestDigestShowsOnlyTheSessionsSpaces is SP-026.1.a1 for the SessionStart
// digest: of the shared streams it shows a session only the PRs and tasks
// of spaces it reads; orchd's routing changes, which belong to no space,
// and the session's own stream show as before.
func TestDigestShowsOnlyTheSessionsSpaces(t *testing.T) {
	spacedEvents(t)
	emitEverySpace(t, "old")
	emitIn(t, "sid", "", "my subagent finished")
	var out bytes.Buffer
	if err := sessionStart(Payload{SessionID: "sid"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"pr-babysitter of space-job-end", "queue of space-pub-end", "old routing change", "my subagent finished"} {
		if !strings.Contains(got, want) {
			t.Errorf("digest is missing %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"space-other-end", "space-quarantine-end", "space--end"} {
		if strings.Contains(got, leak) {
			t.Errorf("digest shows %q:\n%s", leak, got)
		}
	}
	// The last five per stream are the session's: another space's events
	// after them do not push them out.
	for range 6 {
		emitIn(t, "pr-babysitter", "other", "later other PR")
	}
	out.Reset()
	if err := sessionStart(Payload{SessionID: "sid2"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "later other PR") {
		t.Errorf("an unpinned session sees another space's PRs:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pr-babysitter of space-pub-end") || strings.Contains(out.String(), "space-job-end") {
		t.Errorf("an unpinned session reads public spaces only:\n%s", out.String())
	}
}

// TestUpdatesShowOnlyTheSessionsSpaces is SP-026.1.a1 for per-prompt
// updates: a finished task of another space never reaches the session.
func TestUpdatesShowOnlyTheSessionsSpaces(t *testing.T) {
	spacedEvents(t)
	start(t, "sid")
	emitEverySpace(t, "new")
	emitIn(t, "sid", "", "my subagent finished")
	got := prompt(t, "sid")
	for _, want := range []string{"queue of space-job-end", "queue of space-pub-end", "my subagent finished"} {
		if !strings.Contains(got, want) {
			t.Errorf("updates are missing %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"space-other-end", "space-quarantine-end", "space--end"} {
		if strings.Contains(got, leak) {
			t.Errorf("updates show %q:\n%s", leak, got)
		}
	}
	// What was hidden is not shown later either.
	if again := prompt(t, "sid"); again != "" {
		t.Errorf("a second prompt shows:\n%s", again)
	}
}

// TestEventsFailClosedUnderAnUnreadablePolicy: a policy that may be
// applied but whose snapshot cannot be read shows no shared event; with no
// policy ever applied every event shows (INV-10).
func TestEventsFailClosedUnderAnUnreadablePolicy(t *testing.T) {
	spacedEvents(t)
	if err := os.WriteFile(spaces.GuardSnapshotPath(paths.GuardDir()), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	emitEverySpace(t, "x")
	var out bytes.Buffer
	if err := sessionStart(Payload{SessionID: "sid"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "space-") {
		t.Errorf("an unreadable policy shows:\n%s", out.String())
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "fresh"))
	emitEverySpace(t, "y")
	out.Reset()
	if err := sessionStart(Payload{SessionID: "sid"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "space-other-end") {
		t.Errorf("with no policy the digest hides events:\n%s", out.String())
	}
}

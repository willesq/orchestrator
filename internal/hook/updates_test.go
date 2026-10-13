package hook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
)

func emit(t *testing.T, stream, summary string) {
	t.Helper()
	if err := events.Append(paths.EventsDir(), stream, events.Event{Kind: "complete", Source: "dev", Summary: summary}); err != nil {
		t.Fatal(err)
	}
}

// prompt runs the prompt hook with auto-RAG off, so only updates are printed.
func prompt(t *testing.T, sid string) string {
	t.Helper()
	var out bytes.Buffer
	if err := userPromptSubmit(config.AutoRAGSettings{}, Payload{SessionID: sid, Prompt: "carry on"}, &out, factory(&fakeClient{}, nil)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func start(t *testing.T, sid string) {
	t.Helper()
	if err := sessionStart(Payload{SessionID: sid}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatesSurfaceOnceAfterTheDigest(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	emit(t, "queue", "task-old finished complete") // covered by the digest
	start(t, "sid")
	if got := prompt(t, "sid"); got != "" {
		t.Fatalf("first prompt repeated the digest:\n%s", got)
	}

	emit(t, "sid", "review of #12 done")
	emit(t, "queue", "task-new finished failed")
	got := prompt(t, "sid")
	for _, want := range []string{"## Orchestrator updates", "review of #12 done", "task-new finished failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("updates missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "task-old") {
		t.Errorf("an event the digest showed came back:\n%s", got)
	}
	if again := prompt(t, "sid"); again != "" {
		t.Errorf("updates repeated on the next prompt:\n%s", again)
	}
	// Another session's own stream is not this session's business.
	emit(t, "other", "someone else's subagent")
	if got := prompt(t, "sid"); got != "" {
		t.Errorf("another session's events leaked in:\n%s", got)
	}
}

func TestUpdatesBaselineWithoutACursor(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	for i := range 3 {
		emit(t, "queue", fmt.Sprintf("history %d", i))
	}
	// No SessionStart (a session that began before this hook existed).
	if got := prompt(t, "sid"); got != "" {
		t.Fatalf("history was replayed:\n%s", got)
	}
	emit(t, "queue", "fresh")
	if got := prompt(t, "sid"); !strings.Contains(got, "fresh") || strings.Contains(got, "history") {
		t.Errorf("want only the fresh event:\n%s", got)
	}
}

func TestUpdatesSummarizeOverflow(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	start(t, "sid")
	for i := range maxUpdateLines + 4 {
		emit(t, "queue", fmt.Sprintf("task-%02d", i))
	}
	got := prompt(t, "sid")
	if !strings.Contains(got, "(4 earlier update(s) not shown)") {
		t.Errorf("overflow not summarized:\n%s", got)
	}
	if strings.Contains(got, "task-03") || !strings.Contains(got, "task-04") || !strings.Contains(got, "task-13") {
		t.Errorf("want the newest %d updates:\n%s", maxUpdateLines, got)
	}
}

func TestUpdatesReBaselineAShrunkStream(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	emit(t, "queue", "a")
	emit(t, "queue", "b")
	start(t, "sid")
	if err := os.Remove(filepath.Join(paths.EventsDir(), "queue.jsonl")); err != nil {
		t.Fatal(err)
	}
	emit(t, "queue", "after rotation")
	if got := prompt(t, "sid"); got != "" {
		t.Errorf("a shrunk stream should re-baseline silently:\n%s", got)
	}
	emit(t, "queue", "next")
	if got := prompt(t, "sid"); !strings.Contains(got, "next") {
		t.Errorf("updates stopped after re-baselining:\n%s", got)
	}
}

func TestUpdatesSkipQueuedTaskSessions(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	start(t, "sid")
	emit(t, "queue", "task-2 finished complete")
	if got := prompt(t, "sid"); got != "" {
		t.Errorf("a queued task's session was shown other tasks' updates:\n%s", got)
	}
}

func TestUpdatesPrecedeEnrichment(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv(agentrun.TaskIDEnv, "")
	start(t, "sid")
	emit(t, "queue", "task-9 finished complete")
	var out bytes.Buffer
	if err := userPromptSubmit(defaultAutoRAG(), Payload{SessionID: "sid", Prompt: "how do retries work?"}, &out, factory(&fakeClient{search: hitsResp(0.9)}, nil)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	u, r := strings.Index(got, "## Orchestrator updates"), strings.Index(got, "## Auto-injected RAG context")
	if u < 0 || r < 0 || u > r {
		t.Errorf("want updates then RAG context:\n%s", got)
	}
}

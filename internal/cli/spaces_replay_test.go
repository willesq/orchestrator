package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

var updateReplayGolden = flag.Bool("update-replay-golden", false, "rewrite testdata/replay/fixtures.golden")

// replayFake is what fakeClient's ReplaySpaces answers and records.
var replayFake struct {
	req  *orchdv1.ReplaySpacesRequest
	resp *orchdv1.ReplaySpacesResponse
	err  error
}

func (f *fakeClient) ReplaySpaces(_ context.Context, req *orchdv1.ReplaySpacesRequest, _ ...grpc.CallOption) (*orchdv1.ReplaySpacesResponse, error) {
	replayFake.req = req
	return replayFake.resp, replayFake.err
}

const fixturePayloads = "../hook/testdata/payloads"

// TestSpacesReplayFixtures replays the SP-001 hook payload fixtures under a
// policy in which the project and the outside folder are two jobs, and
// compares every verdict with the committed golden output. CI runs it with
// the rest of the tests, which is how the fixtures are replayed in CI.
func TestSpacesReplayFixtures(t *testing.T) {
	out, err := run(factory(&fakeClient{}, errors.New("no orchd needed")), "",
		"spaces", "replay", fixturePayloads, "--policy", "testdata/replay/fixtures-policy.json", "--all")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "replay", "fixtures.golden")
	if *updateReplayGolden {
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("replay of the fixtures differs from %s (rerun with -update-replay-golden after checking):\n%s", golden, out)
	}
	for _, s := range []string{"refuse (other_job_path)", "allow (within_space)", "claude-code.jsonl", "codex.jsonl"} {
		if !strings.Contains(out, s) {
			t.Errorf("replay output lacks %q", s)
		}
	}
	// One file, judged strict, lists only what changed: nothing, as payload
	// files carry no recorded verdict.
	quiet, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", fixturePayloads+"/codex.jsonl",
		"--policy", "testdata/replay/fixtures-policy.json", "--mode", "strict")
	if err != nil || strings.Contains(quiet, "SEQ") || !strings.Contains(quiet, "mode strict") {
		t.Errorf("strict replay without --all = %q, %v", quiet, err)
	}
}

// TestSpacesReplayFixturesSelfProtection: a payload replay protects what
// orchd's guard protects (RC-110), so writes into a checkout's git hooks,
// harness settings or orch's data directory replay as self_protection
// refusals, and an ordinary call in the space is allowed.
func TestSpacesReplayFixturesSelfProtection(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("HOME", home)
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("ORCH_DATA_DIR", data)
	call := func(tool, key, value string) string {
		b, _ := json.Marshal(map[string]any{"payload": map[string]any{"hook_event_name": "PreToolUse", "session_id": "s",
			"cwd": "/scratch/project", "tool_name": tool, "tool_input": map[string]string{key: value}}})
		return string(b) + "\n"
	}
	payloads := filepath.Join(t.TempDir(), "self.jsonl")
	lines := call("Bash", "command", "echo x > /scratch/project/.git/hooks/pre-commit") +
		call("Write", "file_path", filepath.Join(home, ".claude", "settings.json")) +
		call("Bash", "command", "echo x >> "+filepath.Join(data, "orch.db")) +
		call("Bash", "command", "echo x > /scratch/project/out.txt")
	if err := os.WriteFile(payloads, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", payloads,
		"--policy", "testdata/replay/fixtures-policy.json", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "refuse (self_protection)"); n != 3 || strings.Count(out, "allow (within_space)") != 1 {
		t.Errorf("%d self_protection refusals, want 3, and one allowed call:\n%s", n, out)
	}
}

func TestSpacesReplayFixtureErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"spaces":[{"name":"a","kind":"weird"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	unparsable := filepath.Join(dir, "unparsable.json")
	if err := os.WriteFile(unparsable, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := "testdata/replay/fixtures-policy.json"
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	many := filepath.Join(dir, "many.jsonl")
	line := `{"payload":{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/w","tool_name":"Bash","tool_input":{"command":"ls"}}}` + "\n"
	if err := os.WriteFile(many, []byte(strings.Repeat(line, 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	old := maxFixtureEvents
	maxFixtureEvents = 3
	t.Cleanup(func() { maxFixtureEvents = old })
	for name, args := range map[string][]string{
		"fifo policy":      {fixturePayloads, "--policy", fifo},
		"fifo payloads":    {fifo, "--policy", policy},
		"too many events":  {many, "--policy", policy},
		"no policy":        {fixturePayloads},
		"revision":         {fixturePayloads, "--policy", "3"},
		"bad mode":         {fixturePayloads, "--policy", policy, "--mode", "lenient"},
		"missing policy":   {fixturePayloads, "--policy", filepath.Join(dir, "none.json")},
		"invalid policy":   {fixturePayloads, "--policy", invalid},
		"unparsable":       {fixturePayloads, "--policy", unparsable},
		"missing path":     {filepath.Join(dir, "none.jsonl"), "--policy", policy},
		"empty folder":     {dir + "/", "--policy", policy},
		"bad payload line": {bad, "--policy", policy},
	} {
		if name == "empty folder" {
			args[0] = t.TempDir()
		}
		if _, err := run(factory(&fakeClient{}, nil), "", append([]string{"spaces", "replay"}, args...)...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestSpacesReplaySession sends a session replay to orchd with the policy
// named and prints each changed verdict with its rule.
func TestSpacesReplaySession(t *testing.T) {
	t.Cleanup(func() { replayFake.req, replayFake.resp, replayFake.err = nil, nil, nil })
	replayFake.resp = &orchdv1.ReplaySpacesResponse{
		Owner: "sess-a", Mode: "block-other-jobs", PinSpace: "acme", Pinned: true,
		Events: 3, Compared: 2, Changed: 1, Unrecorded: 1, Problems: 1, Truncated: true,
		Rows: []*orchdv1.ReplayedEvent{
			{Seq: 2, Event: "pre-tool-use", Tool: "Bash", PolicyRevision: 1, Recorded: true, RecordedVerdict: "refuse", RecordedRule: "other_job_path", Verdict: "allow", Rule: "within_space", Changed: true},
			{Seq: 3, Event: "pre-tool-use", Tool: "Read", PolicyRevision: 2, Problem: "revision 2 was changed outside orchd"},
		},
	}
	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(`{"spaces":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", "sess-a", "--policy", policy, "--mode", "strict", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if r := replayFake.req; r.GetOwner() != "sess-a" || r.GetPolicyJson() != `{"spaces":[]}` || r.GetMode() != "strict" || !r.GetAll() {
		t.Errorf("request = %v", r)
	}
	for _, s := range []string{
		"sess-a (pinned to acme), mode block-other-jobs: 3 calls replayed, 2 with a recorded verdict, 1 changed",
		"refuse (other_job_path)", "allow (within_space)", "yes",
		"not decided: revision 2 was changed outside orchd", "stop there",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", "sess-a", "--policy", "7"); err != nil || replayFake.req.GetPolicyRevision() != 7 {
		t.Errorf("revision policy: %v %v", replayFake.req, err)
	}
	replayFake.resp = &orchdv1.ReplaySpacesResponse{Owner: "task:t"}
	if out, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", "task:t"); err != nil || !strings.Contains(out, "task:t (no pin)") {
		t.Errorf("no pin: %q %v", out, err)
	}
	for _, p := range []string{"0", filepath.Join(t.TempDir(), "none.json")} {
		if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", "sess-a", "--policy", p); err == nil {
			t.Errorf("--policy %s: no error", p)
		}
	}
	replayFake.err = errors.New("only the operator can replay")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "replay", "sess-a"); err == nil {
		t.Error("orchd's refusal was not returned")
	}
}

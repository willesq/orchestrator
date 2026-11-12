package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestRecordToolEvent: orchd takes every valid tool event and attributes it
// to the caller it resolved, whatever session the request names, and refuses
// malformed ones.
func TestRecordToolEvent(t *testing.T) {
	s := &server{}
	var got []toolEvent
	s.toolEvents.setSink(func(ev toolEvent) { got = append(got, ev) })
	pin := sessionPin{SessionID: "sess-a", Harness: "claude"}
	ctx := context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerSession, pin: pin})

	ok := []*orchdv1.RecordToolEventRequest{
		{Event: "pre-tool-use", SessionId: "sess-b", ToolName: "Bash", ToolInput: []byte(`{"command":"ls"}`)},
		{Event: "cwd-changed", SessionId: "sess-a", OldCwd: "/a", NewCwd: "/b"},
		{Event: "pre-tool-use"}, // a payload the hook could not parse
		{Event: "pre-tool-use", ToolInput: make([]byte, maxToolEventInput)},
	}
	for _, req := range ok {
		if _, err := s.RecordToolEvent(ctx, req); err != nil {
			t.Fatalf("RecordToolEvent(%v) = %v", req, err)
		}
	}
	if len(got) != len(ok) || s.toolEvents.count(callerSession) != int64(len(ok)) {
		t.Fatalf("took %d events, counted %d; want %d", len(got), s.toolEvents.count(callerSession), len(ok))
	}
	for i, ev := range got {
		if ev.caller.kind != callerSession || ev.caller.pin.SessionID != "sess-a" || ev.req != ok[i] || ev.received.IsZero() {
			t.Errorf("event %d = %+v", i, ev)
		}
	}

	bad := []*orchdv1.RecordToolEventRequest{
		{Event: "post-tool-use"},
		{},
		{Event: "pre-tool-use", SessionId: "../etc"},
		{Event: "pre-tool-use", ToolInput: make([]byte, maxToolEventInput+1)},
	}
	for _, req := range bad {
		if _, err := s.RecordToolEvent(ctx, req); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("RecordToolEvent(event %q, session %q, %d bytes) = %v, want InvalidArgument",
				req.GetEvent(), req.GetSessionId(), len(req.GetToolInput()), err)
		}
	}
	if len(got) != len(ok) {
		t.Errorf("refused events reached the sink")
	}

	// Without a sink, events are counted and dropped.
	var unresolved server
	if _, err := unresolved.RecordToolEvent(context.Background(), ok[0]); err != nil {
		t.Fatal(err)
	}
	if unresolved.toolEvents.count(callerNone) != 1 {
		t.Errorf("an unresolved call was not counted as callerNone")
	}
}

const (
	hookCostHelperEnv = "ORCH_TEST_HOOK_COST_HELPER"
	hookCostBinEnv    = "ORCH_TEST_HOOK_COST_BIN"
	hookCostRuns      = 1000
	hookCostP99       = 500 * time.Millisecond
)

// TestToolHookCostHelper is the fake harness TestToolHookCost starts as a
// program named claude: it runs orch-hook pre-tool-use hookCostRuns times, as
// Claude Code would before each tool call, and prints each run's wall time.
func TestToolHookCostHelper(t *testing.T) {
	if os.Getenv(hookCostHelperEnv) == "" {
		t.Skip("only runs as TestToolHookCost's child")
	}
	fmt.Printf("ready\n")
	// Wait until the parent has pinned this process.
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"sess-hook","cwd":"/scratch/project","hook_event_name":"PreToolUse",` +
		`"tool_name":"Bash","tool_input":{"command":"git -C /scratch/outside status"},"tool_use_id":"toolu_1"}`
	bin := os.Getenv(hookCostBinEnv)
	for range hookCostRuns {
		cmd := exec.Command(bin, "pre-tool-use")
		cmd.Stdin = strings.NewReader(payload)
		start := time.Now()
		out, err := cmd.Output()
		took := time.Since(start)
		if err != nil || len(out) != 0 {
			fmt.Printf("fail %v %q\n", err, out)
			return
		}
		fmt.Printf("took %d\n", took.Nanoseconds())
	}
}

// TestToolHookCost is SP-012.1.a2: the wall time of the real orch-hook
// binary, from process start to exit, handing a PreToolUse event to orchd's
// real server (peer identification and caller resolution included). Each
// event must reach orchd attributed to the fake harness's pinned session.
func TestToolHookCost(t *testing.T) {
	if os.Getenv(hookCostHelperEnv) != "" {
		t.Skip("running as the helper")
	}
	if testing.Short() {
		t.Skip("builds orch-hook and runs it 1,000 times")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process identity is not supported on " + runtime.GOOS)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "orch-hook")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/willesq/orchestrator/cmd/orch-hook").CombinedOutput(); err != nil {
		t.Fatalf("build orch-hook: %v\n%s", err, out)
	}
	dir, err := os.MkdirTemp(os.TempDir(), "orch-hookcost-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	pins := newPinRegistry(testDB(t))
	res := &callerResolver{pins: pins}
	// self is no real process, so the fake harness is not orchd's child (a
	// queued task), as newGRPCServer would see it with this test as orchd.
	gs := grpc.NewServer(
		grpc.Creds(peerCredentials{self: -1, parent: procstat.Parent, program: procstat.ReadProgram, started: procstat.StartTime}),
		grpc.ChainUnaryInterceptor(guardQueuedCallers, res.unary),
	)
	s := &server{pins: pins}
	var mu sync.Mutex
	var sessions, verdicts []string
	s.toolEvents.setSink(func(ev toolEvent) {
		mu.Lock()
		sessions = append(sessions, ev.caller.pin.SessionID)
		verdicts = append(verdicts, ev.req.GetGuardVerdict()+" "+ev.req.GetGuardRule())
		mu.Unlock()
	})
	orchdv1.RegisterOrchdServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(tmp, "claude")
	if err := os.Symlink(exe, harness); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(harness, "-test.run=^TestToolHookCostHelper$")
	cmd.Env = append(os.Environ(), hookCostHelperEnv+"=1", hookCostBinEnv+"="+bin, "ORCH_SOCKET="+sock,
		"ORCH_DATA_DIR="+filepath.Join(tmp, "data"), "ORCH_CONFIG_DIR="+filepath.Join(tmp, "config"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "ready" {
		t.Fatalf("helper said %q", lines.Text())
	}
	started, err := procstat.StartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := pins.insert(context.Background(), sessionPin{SessionID: "sess-hook", Harness: "claude",
		Process: procstat.Identity{PID: cmd.Process.Pid, Started: started}, FirstCwd: "/", PinnedAt: started}); err != nil {
		t.Fatal(err)
	}
	// Reason: the hook decides each call against these, as the enforcing
	// guard does (SP-041.1.a3): the cost includes reading them.
	guardDir := filepath.Join(tmp, "data", "guard")
	pol := spaces.Policy{Spaces: []spaces.Space{{Name: "job", Kind: spaces.Isolated, Roots: []string{"/scratch"}},
		{Name: "other", Kind: spaces.Isolated, Roots: []string{filepath.Join(tmp, "other")}}}}
	prot := newGuardWriter(guardDir, tmp, filepath.Join(tmp, "config"), filepath.Join(tmp, "data"), "").protection(nil)
	if err := spaces.WriteGuardSnapshot(guardDir, spaces.GuardSnapshot{Revision: 1, Policy: &pol, Home: tmp, Protection: prot,
		Guard: spaces.GuardSwitch{State: spaces.SwitchBlockOtherJobs}}); err != nil {
		t.Fatal(err)
	}
	if err := spaces.WriteGuardPin(guardDir, spaces.GuardPinFile{SessionID: "sess-hook", Space: "job", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}

	var took []time.Duration
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) == 2 && f[0] == "took" {
			ns, _ := strconv.ParseInt(f[1], 10, 64)
			took = append(took, time.Duration(ns))
			continue
		}
		if len(f) > 0 && (f[0] == "fail" || f[0] == "---" || f[0] == "FAIL") {
			t.Fatalf("helper: %s", lines.Text())
		}
	}
	_ = cmd.Wait()
	if len(took) != hookCostRuns {
		t.Fatalf("helper ran %d hooks, want %d", len(took), hookCostRuns)
	}
	// The hook does not wait for the event to be processed, but orchd takes
	// it before answering. A hook gives up after toolEventTimeout rather than
	// slow the harness, so on a loaded machine (CI's shared runners) a few
	// events are dropped by design; all that arrive must be attributed.
	mu.Lock()
	got := slices.Clone(sessions)
	decided := slices.Compact(slices.Sorted(slices.Values(verdicts)))
	mu.Unlock()
	if len(decided) != 1 || decided[0] != "allow within_space" {
		t.Errorf("the guard decided %v, want every call allowed within its space", decided)
	}
	if len(got) < hookCostRuns*99/100 || len(got) > hookCostRuns ||
		slices.IndexFunc(got, func(id string) bool { return id != "sess-hook" }) >= 0 {
		t.Fatalf("orchd took %d events (want at least %d of %d), attributed to %v", len(got), hookCostRuns*99/100,
			hookCostRuns, slices.Compact(slices.Sorted(slices.Values(got))))
	}
	slices.Sort(took)
	q := func(p float64) time.Duration { return took[int(p*float64(len(took)-1))] }
	t.Logf("orch-hook pre-tool-use wall time: p50 %v, p90 %v, p99 %v, max %v (n=%d, %s/%s)",
		q(0.5), q(0.9), q(0.99), q(1), len(took), runtime.GOOS, runtime.GOARCH)
	// A regression guard, not the 30 ms target a Mac meets (p99 about 13 ms):
	// CI's shared runners measured over 30 ms under the parallel suite.
	if p99 := q(0.99); p99 >= hookCostP99 {
		t.Errorf("p99 = %v, want < %v", p99, hookCostP99)
	}
}

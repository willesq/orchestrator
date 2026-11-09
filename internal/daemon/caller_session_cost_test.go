package daemon

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

const costHelperEnv = "ORCH_TEST_RESOLVE_HELPER"

// testDB opens a migrated orch.db in a temporary data directory, never the
// user's own.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestResolveCostHelper is the fake harness TestResolveCost starts as a
// program named claude: it starts a child, prints its PID and waits.
func TestResolveCostHelper(t *testing.T) {
	if os.Getenv(costHelperEnv) == "" {
		t.Skip("only runs as TestResolveCost's child")
	}
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("child=%d\n", child.Process.Pid)
	_ = child.Wait()
}

// TestResolveCost is SP-011.2: the time to identify a new connection over
// the real process table and resolve it to its pin, and the time for later
// calls on that connection, which hit its cache.
func TestResolveCost(t *testing.T) {
	if os.Getenv(costHelperEnv) != "" {
		t.Skip("running as the helper")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process identity is not supported on " + runtime.GOOS)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(exe, harness); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(harness, "-test.run=^TestResolveCostHelper$")
	cmd.Env = append(os.Environ(), costHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "child="))
	if err != nil {
		t.Fatalf("helper said %q", line)
	}
	started, err := procstat.StartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	pins := newPinRegistry(testDB(t))
	// A few hundred other sessions, as weeks of retained pins would hold.
	for i := range 300 {
		if err := pins.insert(context.Background(), sessionPin{SessionID: fmt.Sprintf("old-%d", i), Harness: "claude",
			Process: procstat.Identity{PID: 100000 + i, Started: started.Add(-time.Hour)}, PinnedAt: started}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pins.insert(context.Background(), sessionPin{SessionID: "sess-real", Harness: "claude",
		Process: procstat.Identity{PID: cmd.Process.Pid, Started: started}, FirstCwd: "/", PinnedAt: started}); err != nil {
		t.Fatal(err)
	}
	r := &callerResolver{pins: pins}
	// self is no real process, so the sleep is not orchd's child.
	creds := peerCredentials{self: -1, parent: procstat.Parent, program: procstat.ReadProgram, started: procstat.StartTime,
		peerPID: func(net.Conn) (int, error) { return child, nil }}

	const n = 1000
	cold, lookup, warm := make([]time.Duration, 0, n), make([]time.Duration, 0, n), make([]time.Duration, 0, n)
	for range n {
		t0 := time.Now()
		ctx := callerCtx(creds.identify(nil))
		t1 := time.Now()
		got := r.resolve(ctx)
		cold, lookup = append(cold, time.Since(t0)), append(lookup, time.Since(t1))
		if got.kind != callerSession || got.pin.SessionID != "sess-real" {
			t.Fatalf("resolve(%d) = %+v, want sess-real", child, got)
		}
		t2 := time.Now()
		r.resolve(ctx)
		warm = append(warm, time.Since(t2))
	}
	p := func(ds []time.Duration, q float64) time.Duration {
		slices.Sort(ds)
		return ds[int(q*float64(len(ds)-1))]
	}
	t.Logf("new connection (identify + resolve): p50 %v, p99 %v, max %v; of which resolve: p50 %v, p99 %v; cached call: p50 %v, p99 %v (n=%d, %s/%s)",
		p(cold, 0.5), p(cold, 0.99), p(cold, 1), p(lookup, 0.5), p(lookup, 0.99), p(warm, 0.5), p(warm, 0.99), n, runtime.GOOS, runtime.GOARCH)
	// Reason: SP-011.2's 5 ms bound is for the Macs orchd runs on. A shared
	// Linux CI runner, testing other packages in parallel, measured p99 6.5 ms
	// there; elsewhere the bound only catches a gross regression.
	limit := 5 * time.Millisecond
	if runtime.GOOS != "darwin" {
		limit = 50 * time.Millisecond
	}
	if p99 := p(cold, 0.99); p99 >= limit {
		t.Errorf("p99 per connection = %v, want < %v", p99, limit)
	}
}

// pingRecorder serves Ping and records whom the call was resolved to.
type pingRecorder struct {
	orchdv1.UnimplementedOrchdServer
	seen chan resolvedCaller
}

func (p pingRecorder) Ping(ctx context.Context, _ *orchdv1.PingRequest) (*orchdv1.PingResponse, error) {
	p.seen <- callerOf(ctx)
	return &orchdv1.PingResponse{}, nil
}

// TestResolverOnSocket: orchd's server resolves every call it serves over a
// real socket.
func TestResolverOnSocket(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "orch-resolve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := newGRPCServer(&callerResolver{pins: newPinRegistry(testDB(t))})
	rec := pingRecorder{seen: make(chan resolvedCaller, 1)}
	orchdv1.RegisterOrchdServer(gs, rec)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cc, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := orchdv1.NewOrchdClient(cc).Ping(context.Background(), &orchdv1.PingRequest{}); err != nil {
		t.Fatal(err)
	}
	got := <-rec.seen
	// This test may itself run inside an agent session, which has no pin in
	// the test's store; otherwise it is the operator.
	if got.why == "the call was not resolved" || (got.kind != callerOperator && got.kind != callerNone) {
		t.Fatalf("resolved %+v", got)
	}
}

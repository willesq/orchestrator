package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestDescendsFrom(t *testing.T) {
	// orchd is 100; 200 is its child and 300 its grandchild; 400 is a terminal
	// shell; 600 and 700 are each other's parent.
	tree := map[int]int{100: 1, 200: 100, 300: 200, 400: 50, 50: 1, 500: 500, 600: 700, 700: 600}
	parent := func(pid int) (int, error) {
		if pp, ok := tree[pid]; ok {
			return pp, nil
		}
		return 0, errors.New("no such process")
	}
	tests := []struct {
		name    string
		pid     int
		want    bool
		wantErr bool
	}{
		{name: "orchd itself is the operator", pid: 100},
		{name: "a session orchd started", pid: 200, want: true},
		{name: "a command that session ran", pid: 300, want: true},
		{name: "a terminal outside orchd", pid: 400},
		{name: "its own parent", pid: 500},
		{name: "a process that is gone", pid: 999, wantErr: true},
		{name: "a cycle is bounded", pid: 600, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := descendsFrom(tt.pid, 100, parent)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("descendsFrom(%d) = %v, want %v", tt.pid, got, tt.want)
			}
		})
	}
}

func TestQueuedCaller(t *testing.T) {
	withPeer := func(a callerAuth) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: a})
	}
	tests := []struct {
		name     string
		ctx      context.Context
		want     bool
		wantName string
	}{
		{name: "no peer", ctx: context.Background(), want: true, wantName: "unidentified process"},
		{name: "the operator's own process", ctx: withPeer(callerAuth{pid: 42})},
		{name: "a process orchd started", ctx: withPeer(callerAuth{pid: 42, spawned: true}), want: true, wantName: "process 42, which orchd started"},
		{name: "an unidentified process", ctx: withPeer(callerAuth{err: errors.New("boom")}), want: true, wantName: "unidentified process"},
		{name: "a platform without peer lookup", ctx: withPeer(callerAuth{err: errPeerUnsupported})},
		{
			name:     "a client that reports its task",
			ctx:      metadata.NewIncomingContext(withPeer(callerAuth{pid: 42}), metadata.Pairs(orchdclient.TaskMetadataKey, "task-1")),
			want:     true,
			wantName: "task task-1",
		},
		{
			name: "an empty task report is not a task",
			ctx:  metadata.NewIncomingContext(withPeer(callerAuth{pid: 42}), metadata.Pairs(orchdclient.TaskMetadataKey, "")),
		},
		{name: "another transport's auth info", ctx: peer.NewContext(context.Background(), &peer.Peer{AuthInfo: insecureAuth{}}),
			want: true, wantName: "unidentified process"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, got := queuedCaller(tt.ctx)
			if got != tt.want || name != tt.wantName {
				t.Errorf("queuedCaller = %q, %v; want %q, %v", name, got, tt.wantName, tt.want)
			}
		})
	}
}

type insecureAuth struct{}

func (insecureAuth) AuthType() string { return "insecure" }

func TestGuardQueuedCallers(t *testing.T) {
	operator := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: callerAuth{pid: 42}})
	task := metadata.NewIncomingContext(operator, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	tests := []struct {
		name     string
		ctx      context.Context
		method   string
		wantCode codes.Code
		wantMsg  string
	}{
		{name: "the operator may dispatch", ctx: operator, method: orchdv1.Orchd_Dispatch_FullMethodName},
		{name: "the operator may call an RPC the tables do not know", ctx: operator, method: "/orchd.v1.Orchd/Future"},
		{name: "a task cannot call an RPC the tables do not know", ctx: task, method: "/orchd.v1.Orchd/Future",
			wantCode: codes.PermissionDenied, wantMsg: "cannot call /orchd.v1.Orchd/Future"},
		{name: "a task cannot force a docs sync", ctx: task, method: orchdv1.Orchd_SyncDocs_FullMethodName,
			wantCode: codes.PermissionDenied, wantMsg: "cannot sync docs"},
		{name: "a task may search", ctx: task, method: orchdv1.Orchd_Search_FullMethodName},
		{name: "a task may read its tasks", ctx: task, method: orchdv1.Orchd_GetTask_FullMethodName},
		{name: "a task cannot dispatch", ctx: task, method: orchdv1.Orchd_Dispatch_FullMethodName,
			wantCode: codes.PermissionDenied, wantMsg: "queue tasks cannot dispatch further tasks (caller: task task-1)"},
		{name: "a task cannot ingest", ctx: task, method: orchdv1.Orchd_Ingest_FullMethodName,
			wantCode: codes.PermissionDenied, wantMsg: "cannot write to the RAG store"},
		{name: "a task cannot rate", ctx: task, method: orchdv1.Orchd_RateTask_FullMethodName,
			wantCode: codes.PermissionDenied, wantMsg: "cannot rate tasks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }
			_, err := guardQueuedCallers(tt.ctx, nil, &grpc.UnaryServerInfo{FullMethod: tt.method}, handler)
			if got := grpcstatus.Code(err); got != tt.wantCode {
				t.Fatalf("code = %v, want %v (err %v)", got, tt.wantCode, err)
			}
			if called != (tt.wantCode == codes.OK) {
				t.Errorf("handler called = %v", called)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want %q", err, tt.wantMsg)
			}
		})
	}
}

// Every RPC must be classed as callable by tasks or operator-only (with the
// action its refusal names), and every classed name must be a real RPC, or a
// rename would silently change the guard.
func TestEveryRPCIsClassified(t *testing.T) {
	methods := map[string]bool{}
	for _, m := range orchdv1.Orchd_ServiceDesc.Methods {
		methods["/"+orchdv1.Orchd_ServiceDesc.ServiceName+"/"+m.MethodName] = true
	}
	for _, m := range orchdv1.Orchd_ServiceDesc.Streams {
		methods["/"+orchdv1.Orchd_ServiceDesc.ServiceName+"/"+m.StreamName] = true
	}
	for name := range methods {
		if taskCallable[name] == (operatorActions[name] != "") {
			t.Errorf("%s must be in exactly one of taskCallable and operatorActions", name)
		}
	}
	for name := range taskCallable {
		if !methods[name] {
			t.Errorf("taskCallable names %s, which Orchd does not serve", name)
		}
	}
	for name := range operatorActions {
		if !methods[name] {
			t.Errorf("operatorActions names %s, which Orchd does not serve", name)
		}
	}
}

// fakeStream is a server stream with a fixed context.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

func TestGuardQueuedStreams(t *testing.T) {
	operator := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: callerAuth{pid: 42}})
	task := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: callerAuth{pid: 43, spawned: true}})
	tests := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{name: "the operator may stream", ctx: operator},
		{name: "a task may not", ctx: task, wantCode: codes.PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			err := guardQueuedStreams(nil, fakeStream{ctx: tt.ctx}, &grpc.StreamServerInfo{FullMethod: "/orchd.v1.Orchd/Watch"},
				func(any, grpc.ServerStream) error { called = true; return nil })
			if grpcstatus.Code(err) != tt.wantCode || called != (tt.wantCode == codes.OK) {
				t.Errorf("err = %v, handler called %v", err, called)
			}
		})
	}
}

func TestIdentify(t *testing.T) {
	// orchd is 100 and 200 its child; 300's parent lookup fails once, as
	// when an ancestor exits mid-walk.
	failures := 0
	parent := func(pid int) (int, error) {
		switch pid {
		case 200:
			return 100, nil
		case 300:
			if failures == 0 {
				failures++
				return 0, errors.New("gone")
			}
			return 1, nil
		}
		return 0, errors.New("no such process")
	}
	tests := []struct {
		name        string
		pid         int
		wantSpawned bool
		wantErr     error
	}{
		{name: "a child of orchd", pid: 200, wantSpawned: true},
		{name: "a walk that fails once is retried", pid: 300},
		{name: "a peer in another PID namespace", pid: 0, wantErr: errPeerUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds := peerCredentials{self: 100, parent: parent,
				peerPID: func(net.Conn) (int, error) { return tt.pid, nil }}
			a := creds.identify(nil)
			if a.spawned != tt.wantSpawned || !errors.Is(a.err, tt.wantErr) || (tt.wantErr == nil && a.err != nil) {
				t.Errorf("identify = %+v, want spawned %v err %v", a, tt.wantSpawned, tt.wantErr)
			}
		})
	}
}

func TestPrintableID(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "task-1.a_b", want: "task-1.a_b"},
		{in: "x\nforged log line", want: "x?forged?log?line"},
		{in: strings.Repeat("a", 70), want: strings.Repeat("a", 64) + "…"},
	}
	for _, tt := range tests {
		if got := printableID(tt.in); got != tt.want {
			t.Errorf("printableID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPeerCredentialsOnAConnWithoutASocket(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	creds := peerCredentials{self: os.Getpid(), parent: procstat.Parent}
	conn, info, err := creds.ServerHandshake(a)
	if err != nil || conn != a {
		t.Fatalf("handshake = %v, %v", conn, err)
	}
	auth, ok := info.(callerAuth)
	if !ok || auth.err == nil {
		t.Fatalf("auth = %+v; a pipe has no peer and must be unidentified", info)
	}
	if auth.AuthType() != "orchd-peer" || creds.Info().SecurityProtocol != "orchd-peer" || creds.Clone() == nil {
		t.Error("credentials describe themselves wrongly")
	}
	if creds.OverrideServerName("x") != nil {
		t.Error("OverrideServerName failed")
	}
	if _, _, err := creds.ClientHandshake(context.Background(), "", a); err == nil {
		t.Error("server-only credentials accepted a client handshake")
	}
}

// dispatchOK accepts every dispatch, so only the guard can refuse one.
type dispatchOK struct {
	orchdv1.UnimplementedOrchdServer
}

func (dispatchOK) Dispatch(context.Context, *orchdv1.DispatchRequest) (*orchdv1.DispatchResponse, error) {
	return &orchdv1.DispatchResponse{InstanceId: "task-new"}, nil
}

const helperSocketEnv = "ORCH_TEST_GUARD_SOCKET"

// TestGuardedSocket serves the guard over a real socket. This process plays
// orchd: its own calls are the operator's, and a child it starts plays a task
// session that has unset ORCH_TASK_ID before dialing.
func TestGuardedSocket(t *testing.T) {
	if os.Getenv(helperSocketEnv) != "" {
		t.Skip("running as the helper")
	}
	dir, err := os.MkdirTemp(os.TempDir(), "orch-guard-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(
		grpc.Creds(peerCredentials{self: os.Getpid(), parent: procstat.Parent}),
		grpc.UnaryInterceptor(guardQueuedCallers),
	)
	orchdv1.RegisterOrchdServer(gs, dispatchOK{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	t.Run("the operator may dispatch", func(t *testing.T) {
		if code := dialDispatch(sock); code != codes.OK {
			t.Fatalf("code = %v, want OK", code)
		}
	})
	t.Run("a process orchd started may not", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGuardHelper$")
		cmd.Env = append(withoutEnv(os.Environ(), agentrun.TaskIDEnv), helperSocketEnv+"="+sock)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "code="+codes.PermissionDenied.String()) {
			t.Fatalf("helper was not refused:\n%s", out)
		}
	})
}

// TestGuardHelper is the child TestGuardedSocket starts.
func TestGuardHelper(t *testing.T) {
	sock := os.Getenv(helperSocketEnv)
	if sock == "" {
		t.Skip("only runs as TestGuardedSocket's child")
	}
	fmt.Printf("code=%s\n", dialDispatch(sock))
}

func dialDispatch(sock string) codes.Code {
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return codes.Unknown
	}
	defer conn.Close()
	_, err = orchdv1.NewOrchdClient(conn).Dispatch(context.Background(), &orchdv1.DispatchRequest{Body: "x"})
	return grpcstatus.Code(err)
}

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func TestSocketPeerPIDOnABadDescriptor(t *testing.T) {
	if _, err := socketPeerPID(-1); err == nil {
		t.Error("read a peer from no socket")
	}
}

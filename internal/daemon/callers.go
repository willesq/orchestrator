package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Queued sessions run as the user, so the owner-only socket admits them along
// with the operator's own CLI, hooks and app. orchd tells them apart itself
// rather than trusting each client to check ORCH_TASK_ID, which a session can
// unset: every connection is attributed to the process that opened it, and a
// process descended from orchd is one orchd started — a task session, its MCP
// servers, hooks and shell commands, or a verification run. Clients also
// report the task they run in, which names it in the refusal.

// taskCallable lists the RPCs a queued task may make: they read state or
// serve the session's own work. Every other RPC changes what later sessions
// see or run (further tasks, routing evidence, stored memory, the sources
// orchd ingests, orchd's subsystems), so a new RPC is refused to tasks until
// it is added here.
var taskCallable = map[string]bool{
	orchdv1.Orchd_Ping_FullMethodName:                  true,
	orchdv1.Orchd_Search_FullMethodName:                true,
	orchdv1.Orchd_ListSources_FullMethodName:           true,
	orchdv1.Orchd_ListTasks_FullMethodName:             true,
	orchdv1.Orchd_GetTask_FullMethodName:               true,
	orchdv1.Orchd_Status_FullMethodName:                true,
	orchdv1.Orchd_RecommendModel_FullMethodName:        true,
	orchdv1.Orchd_ListPRs_FullMethodName:               true,
	orchdv1.Orchd_GetGitHubIdentity_FullMethodName:     true,
	orchdv1.Orchd_ListGitHubScopes_FullMethodName:      true,
	orchdv1.Orchd_ListGitHubRepos_FullMethodName:       true,
	orchdv1.Orchd_ListGitHubItems_FullMethodName:       true,
	orchdv1.Orchd_TailLogs_FullMethodName:              true,
	orchdv1.Orchd_GetSystemMetrics_FullMethodName:      true,
	orchdv1.Orchd_ListDocs_FullMethodName:              true,
	orchdv1.Orchd_GetDocument_FullMethodName:           true,
	orchdv1.Orchd_GetDocsSync_FullMethodName:           true,
	orchdv1.Orchd_ListRegisteredSources_FullMethodName: true,
	orchdv1.Orchd_GetSafetyNet_FullMethodName:          true,
	orchdv1.Orchd_ListSettings_FullMethodName:          true,
}

// operatorActions says what each operator-only RPC would do, for the refusal.
var operatorActions = map[string]string{
	orchdv1.Orchd_Dispatch_FullMethodName:         "dispatch further tasks",
	orchdv1.Orchd_RateTask_FullMethodName:         "rate tasks",
	orchdv1.Orchd_RetryTask_FullMethodName:        "retry tasks",
	orchdv1.Orchd_CancelTask_FullMethodName:       "cancel tasks",
	orchdv1.Orchd_WatchTasks_FullMethodName:       "watch tasks (use ListTasks or GetTask)",
	orchdv1.Orchd_Ingest_FullMethodName:           "write to the RAG store",
	orchdv1.Orchd_DeleteSource_FullMethodName:     "delete from the RAG store",
	orchdv1.Orchd_SummarizeSession_FullMethodName: "write to the RAG store",
	orchdv1.Orchd_MigrateHybrid_FullMethodName:    "migrate the RAG store",
	orchdv1.Orchd_DropBackup_FullMethodName:       "migrate the RAG store",
	orchdv1.Orchd_ReindexKeywords_FullMethodName:  "migrate the RAG store",
	orchdv1.Orchd_BackfillSessions_FullMethodName: "write to the RAG store",
	orchdv1.Orchd_SaveSource_FullMethodName:       "change registered sources",
	orchdv1.Orchd_RemoveSource_FullMethodName:     "change registered sources",
	orchdv1.Orchd_RefreshSource_FullMethodName:    "refresh registered sources",
	orchdv1.Orchd_SyncDocs_FullMethodName:         "sync docs",
	orchdv1.Orchd_SetGitHubScope_FullMethodName:   "change GitHub monitoring",
	// Reason: every sweep spends the user's GitHub rate limit, and a task can
	// read the cache instead. Tasks' gh pr mutations still request sweeps
	// through the hook, within the same one-a-minute limit.
	orchdv1.Orchd_RequestGitHubSweep_FullMethodName: "ask orchd to search GitHub",
	// Reason: a watch can approve PRs, and verdicts carry text drawn from
	// other people's code, so neither belongs in a task's reach.
	orchdv1.Orchd_WatchPRReview_FullMethodName: "change PR review watches",
	orchdv1.Orchd_StopPRReview_FullMethodName:  "change PR review watches",
	orchdv1.Orchd_ListPRReviews_FullMethodName: "read PR review verdicts",
	orchdv1.Orchd_GetPRDetail_FullMethodName:   "read PR details through orchd (use gh)",
	// Reason: landing is the user's gate on what an agent proposed, so an
	// agent must not reach it; a transcript may quote other tasks' secrets.
	orchdv1.Orchd_GetTaskActivity_FullMethodName:  "read other sessions' activity",
	orchdv1.Orchd_GetPRLanding_FullMethodName:     "read PR landings",
	orchdv1.Orchd_ListPRLandings_FullMethodName:   "read PR landings",
	orchdv1.Orchd_LandPR_FullMethodName:           "push proposed commits",
	orchdv1.Orchd_UndoPRLanding_FullMethodName:    "push proposed commits",
	orchdv1.Orchd_DiscardPRLanding_FullMethodName: "decide PR landings",
	orchdv1.Orchd_Restart_FullMethodName:          "restart orchd subsystems",
	orchdv1.Orchd_InstallSafetyNet_FullMethodName: "install or upgrade cc-safety-net",
	// Reason: a hook is a command Claude Code runs and a permission rule
	// widens what agents may do, so changing either is the operator's call.
	orchdv1.Orchd_SetSettingEnabled_FullMethodName: "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	orchdv1.Orchd_AddSetting_FullMethodName:        "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	orchdv1.Orchd_RemoveSetting_FullMethodName:     "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
}

// refuseQueued returns the refusal for method when ctx's caller is a queued
// task and method is not one tasks may call, else nil.
func refuseQueued(ctx context.Context, method string) error {
	if taskCallable[method] {
		return nil
	}
	who, queued := queuedCaller(ctx)
	if !queued {
		return nil
	}
	action, ok := operatorActions[method]
	if !ok {
		action = "call " + method
	}
	log.Printf("orchd: refused %s from %s", method, who)
	return grpcstatus.Errorf(codes.PermissionDenied,
		"queue tasks cannot %s (caller: %s); report it in the task result instead", action, who)
}

// guardQueuedCallers refuses operator-only RPCs from queued tasks.
func guardQueuedCallers(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := refuseQueued(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// guardQueuedStreams holds streaming RPCs (WatchTasks) to the same rule as
// unary ones, so a stream cannot slip past the guard.
func guardQueuedStreams(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := refuseQueued(ss.Context(), info.FullMethod); err != nil {
		return err
	}
	return handler(srv, ss)
}

// queuedCaller reports whether ctx's caller is a queued task, and names it.
func queuedCaller(ctx context.Context) (string, bool) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if ids := md.Get(orchdclient.TaskMetadataKey); len(ids) > 0 && ids[0] != "" {
			return "task " + printableID(ids[0]), true
		}
	}
	// Every connection to orchd's socket carries callerAuth, so a call without
	// it came some other way and is not trusted as the operator's.
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "unidentified process", true
	}
	a, ok := p.AuthInfo.(callerAuth)
	if !ok {
		return "unidentified process", true
	}
	switch {
	case a.err != nil && !errors.Is(a.err, errPeerUnsupported):
		// A caller orchd cannot place might be a task, so it gets a task's
		// limits.
		return "unidentified process", true
	case a.spawned:
		return fmt.Sprintf("process %d, which orchd started", a.pid), true
	}
	return "", false
}

// printableID bounds a client-supplied task ID before it reaches a log line.
func printableID(id string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '?'
	}, id)
	if len(clean) > 64 {
		clean = clean[:64] + "…"
	}
	return clean
}

// errPeerUnsupported reports a platform where orchd cannot read a socket's
// peer, leaving the client's own report as the only signal.
var errPeerUnsupported = errors.New("socket peer credentials are not supported on this platform")

// callerAuth is what orchd learned about a connection's process.
type callerAuth struct {
	credentials.CommonAuthInfo
	pid int
	// spawned reports that the process descends from orchd.
	spawned bool
	// err says why the process could not be identified.
	err error
}

func (callerAuth) AuthType() string { return "orchd-peer" }

// peerCredentials attributes each connection to the process that opened it.
// It is server-only and exchanges nothing on the wire, so clients keep
// dialing with insecure credentials.
type peerCredentials struct {
	// self is orchd's PID.
	self int
	// parent returns a process's parent PID.
	parent func(pid int) (int, error)
	// peerPID returns a connection's peer PID; nil reads the socket.
	peerPID func(net.Conn) (int, error)
}

func (p peerCredentials) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return c, p.identify(c), nil
}

// identify looks up c's process and whether orchd started it. It runs when
// orchd accepts the connection, so a caller that lets its parent exit first is
// orphaned out of orchd's tree; see docs/internals/daemon.md for the limits.
func (p peerCredentials) identify(c net.Conn) callerAuth {
	lookup := p.peerPID
	if lookup == nil {
		lookup = connPeerPID
	}
	pid, err := lookup(c)
	if err != nil {
		return callerAuth{err: err}
	}
	if pid == 0 {
		// Linux reports 0 for a peer in a PID namespace orchd cannot see, which
		// a process orchd started cannot be.
		log.Printf("orchd: socket peer is in another PID namespace; trusting its client's report")
		return callerAuth{err: errPeerUnsupported}
	}
	spawned, err := descendsFrom(pid, p.self, p.parent)
	if err != nil {
		// An ancestor that exits mid-walk breaks it; a second walk sees the
		// reparented tree instead of refusing the operator.
		spawned, err = descendsFrom(pid, p.self, p.parent)
	}
	return callerAuth{pid: pid, spawned: spawned, err: err}
}

func (peerCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("orchd peer credentials are server-only")
}

func (peerCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "orchd-peer"}
}

func (p peerCredentials) Clone() credentials.TransportCredentials { return p }

func (peerCredentials) OverrideServerName(string) error { return nil }

// connPeerPID returns the PID of the process at the other end of c.
func connPeerPID(c net.Conn) (int, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return 0, fmt.Errorf("%T has no socket to inspect", c)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var perr error
	if err := raw.Control(func(fd uintptr) { pid, perr = socketPeerPID(int(fd)) }); err != nil {
		return 0, err
	}
	return pid, perr
}

// maxAncestry bounds the walk up a process's parents.
const maxAncestry = 256

// descendsFrom reports whether pid is a descendant of ancestor. A process is
// not its own descendant, so orchd calling itself is the operator.
func descendsFrom(pid, ancestor int, parent func(int) (int, error)) (bool, error) {
	if pid == ancestor {
		return false, nil
	}
	for range maxAncestry {
		ppid, err := parent(pid)
		if err != nil {
			return false, fmt.Errorf("parent of process %d: %w", pid, err)
		}
		switch {
		case ppid == ancestor:
			return true, nil
		case ppid <= 1 || ppid == pid:
			return false, nil
		}
		pid = ppid
	}
	return false, fmt.Errorf("process ancestry deeper than %d", maxAncestry)
}

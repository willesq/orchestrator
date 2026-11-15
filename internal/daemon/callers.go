package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
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
	// ExplainPlacement answers agents only about public placements.
	orchdv1.Orchd_ExplainPlacement_FullMethodName: true,
	// Reason: a queued task's tool calls belong in the command log like any
	// session's; the event is attributed to the task orchd resolved, never to
	// the session it names.
	orchdv1.Orchd_RecordToolEvent_FullMethodName: true,
	// ListToolEvents gives a task only its own task's command log.
	orchdv1.Orchd_ListToolEvents_FullMethodName: true,
	// Reason: every session's start digest names a switched-off layer,
	// queued tasks' included (INV-16).
	orchdv1.Orchd_GetEnforcement_FullMethodName: true,
	// Reason: rules are listed by id and kind only, never a value
	// (INV-07); the history and who changed them go to the operator only.
	orchdv1.Orchd_ListOutboundRules_FullMethodName: true,
}

// operatorActions says what each operator-only RPC would do, for the refusal.
var operatorActions = map[string]string{
	orchdv1.Orchd_Dispatch_FullMethodName:           "dispatch further tasks",
	orchdv1.Orchd_RateTask_FullMethodName:           "rate tasks",
	orchdv1.Orchd_RetryTask_FullMethodName:          "retry tasks",
	orchdv1.Orchd_CancelTask_FullMethodName:         "cancel tasks",
	orchdv1.Orchd_WatchTasks_FullMethodName:         "watch tasks (use ListTasks or GetTask)",
	orchdv1.Orchd_Ingest_FullMethodName:             "write to the RAG store",
	orchdv1.Orchd_DeleteSource_FullMethodName:       "delete from the RAG store",
	orchdv1.Orchd_SummarizeSession_FullMethodName:   "write to the RAG store",
	orchdv1.Orchd_MigrateHybrid_FullMethodName:      "migrate the RAG store",
	orchdv1.Orchd_DropBackup_FullMethodName:         "migrate the RAG store",
	orchdv1.Orchd_ReindexKeywords_FullMethodName:    "migrate the RAG store",
	orchdv1.Orchd_BackfillSessions_FullMethodName:   "write to the RAG store",
	orchdv1.Orchd_SaveSource_FullMethodName:         "change registered sources",
	orchdv1.Orchd_RemoveSource_FullMethodName:       "change registered sources",
	orchdv1.Orchd_RefreshSource_FullMethodName:      "refresh registered sources",
	orchdv1.Orchd_SyncDocs_FullMethodName:           "sync docs",
	orchdv1.Orchd_SetGitHubScope_FullMethodName:     "change GitHub monitoring",
	orchdv1.Orchd_ListDocsOwners_FullMethodName:     "read docs owners (use ListDocs)",
	orchdv1.Orchd_ListDocsOwnerRepos_FullMethodName: "ask orchd to list repos on GitHub",
	orchdv1.Orchd_SaveDocsOwner_FullMethodName:      "change which docs are synced",
	orchdv1.Orchd_RemoveDocsOwner_FullMethodName:    "change which docs are synced",
	orchdv1.Orchd_PurgeDocs_FullMethodName:          "delete from the RAG store",
	// Reason: config sets what tasks may run and how many, so a task must not
	// be able to widen its own limits or permissions.
	orchdv1.Orchd_GetConfig_FullMethodName:          "read orch's configuration",
	orchdv1.Orchd_SetConfig_FullMethodName:          "change orch's configuration",
	orchdv1.Orchd_RemoveLegacyConfig_FullMethodName: "change orch's configuration",
	// Reason: every sweep spends the user's GitHub rate limit, and a task can
	// read the cache instead. Tasks' gh pr mutations still request sweeps
	// through the hook, within the same one-a-minute limit.
	orchdv1.Orchd_RequestGitHubSweep_FullMethodName: "ask orchd to search GitHub",
	// Reason: a watch can approve PRs, and verdicts carry text drawn from
	// other people's code, so neither belongs in a task's reach.
	orchdv1.Orchd_WatchPRReview_FullMethodName:            "change PR review watches",
	orchdv1.Orchd_SetPRReviewUpdateInPlace_FullMethodName: "change PR review watches",
	orchdv1.Orchd_StopPRReview_FullMethodName:             "change PR review watches",
	orchdv1.Orchd_ListPRReviews_FullMethodName:            "read PR review verdicts",
	orchdv1.Orchd_GetPRDetail_FullMethodName:              "read PR details through orchd (use gh)",
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
	// Reason: the policy names every space's orgs and roots, which a task in
	// one job must not learn about another; ApplySpaces is also refused to
	// interactive harness sessions (refuseNonOperator).
	orchdv1.Orchd_SetSpacesDraft_FullMethodName: "change the spaces policy",
	orchdv1.Orchd_DiffSpaces_FullMethodName:     "read the spaces policy",
	orchdv1.Orchd_ApplySpaces_FullMethodName:    "apply the spaces policy",
	orchdv1.Orchd_VerifyToolLog_FullMethodName:  "read the command log",
	// Reason: the summary names every space and its activity.
	orchdv1.Orchd_SummarizeToolLog_FullMethodName: "summarize the command log",
	// Reason: a switch turns off a layer that keeps one job's sessions out
	// of another's; SetEnforcement is also refused to interactive harness
	// sessions (refuseNonOperator).
	orchdv1.Orchd_SetEnforcement_FullMethodName: "switch enforcement",
	// Reason (D-61): the setting decides whether orch writes the sandbox
	// rules; it is also refused to interactive harness sessions
	// (refuseNonOperator).
	orchdv1.Orchd_SetSandboxSetting_FullMethodName: "change the sandbox setting",
	// Reason (SP-075): the rules decide what the outbound terms check
	// refuses; also refused to interactive harness sessions.
	orchdv1.Orchd_ChangeOutboundRule_FullMethodName: "change the outbound rules",
	// Reason (SP-027): a relabel moves every space's content and its report
	// names spaces; it is also refused to interactive harness sessions.
	orchdv1.Orchd_Relabel_FullMethodName: "relabel the RAG store",
	// Reason: a queued task's identity comes from its task record, so a pin
	// would only give it a second, self-reported one. Interactive sessions'
	// hooks still pin (PinSession checks the harness process itself).
	orchdv1.Orchd_PinSession_FullMethodName: "pin a session",
	orchdv1.Orchd_EndSession_FullMethodName: "end a session's pin",
	// Reason: a task has no session row, and the list names other sessions'
	// spaces and directories (ListSessions also answers a task with none).
	orchdv1.Orchd_ListSessions_FullMethodName: "list sessions",
	// Reason (SP-047): a replay reads any session's command log and every
	// policy revision; it is also refused to interactive harness sessions
	// (refuseNonOperator).
	orchdv1.Orchd_ReplaySpaces_FullMethodName: "replay the command log",
	// Reason (SP-050): like DiffSpaces, these name every job's orgs, roots
	// and checkouts; both are also refused to interactive harness sessions
	// (refuseNonOperator).
	orchdv1.Orchd_ListSpaces_FullMethodName: "list the spaces",
	orchdv1.Orchd_ScanSpaces_FullMethodName: "scan folders for spaces",
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
	// spawned reports that the process descends from orchd; child is then
	// orchd's own child on its path, which a queued task's agent CLI is,
	// with its start time read at accept (zero when unread).
	spawned bool
	child   procstat.Identity
	// err says why the process could not be identified.
	err error
	// harness names the nearest agent harness process (claude, codex) at or
	// above the caller, empty when there is none; harnessProc is that
	// process. Its start time is zero when it was not read.
	harness     string
	harnessProc procstat.Identity
	// outerHarnesses are the harness processes above harnessProc, nearest
	// first: set when the caller's harness was started inside another
	// session.
	outerHarnesses []procstat.Identity
	// harnessOrphan reports that the harness's parent is launchd or init: it
	// was left behind by an exited parent, as a process an agent detaches
	// from its session is.
	harnessOrphan bool
	// harnessErr says why the harness walk failed; such a caller is treated
	// as if it ran in a harness.
	harnessErr error
	// conn caches what this connection resolves to (caller_session.go); nil
	// resolves on every call.
	conn *connCaller
	// app checks, once and only when a policy needs it, whether the caller
	// is Orch.app by its code signature (D-71); nil for a caller in a
	// harness or started by orchd, which never is.
	app *appCheck
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
	// program returns what a process runs; nil means no lookup, so every
	// caller counts as possibly in a harness.
	program func(pid int) (procstat.Program, error)
	// started returns when a process started; nil leaves a harness's start
	// time unread, so its session cannot be pinned.
	started func(pid int) (time.Time, error)
	// samePeer checks that pid still names c's peer (peer_token.go); nil
	// reads the socket's kernel token, or skips the check when peerPID is
	// set.
	samePeer func(c net.Conn, pid int) error
	// app recognizes Orch.app by its code signature (D-71); nil
	// recognizes nothing.
	app *appVerifier
}

func (p peerCredentials) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return c, p.identify(c), nil
}

// walk looks up c's process and whether orchd started it. It runs when
// orchd accepts the connection, so a caller that lets its parent exit first is
// orphaned out of orchd's tree; see docs/internals/daemon.md for the limits.
// identify checks its result against the peer's kernel token.
func (p peerCredentials) walk(c net.Conn) callerAuth {
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
	child, err := childOnPath(pid, p.self, p.parent)
	if err != nil {
		// An ancestor that exits mid-walk breaks it; a second walk sees the
		// reparented tree instead of refusing the operator.
		child, err = childOnPath(pid, p.self, p.parent)
	}
	spawned := child > 0
	// Reason: unlike the orchd walk above, a failed harness walk is not
	// retried. A session that exits mid-walk reparents its children to
	// launchd, and a second walk would then find no harness.
	harness, hpid, herr := harnessAncestor(pid, p.parent, p.program)
	a := callerAuth{pid: pid, spawned: spawned, err: err, harness: harness, harnessErr: herr, conn: &connCaller{}}
	if spawned {
		// Reason: the PID alone may name a later task's CLI by the time the
		// connection's first call asks for its task.
		a.child = procstat.Identity{PID: child}
		if p.started != nil {
			a.child.Started, _ = p.started(child)
		}
	}
	if harness == "" {
		if !spawned && err == nil && herr == nil && p.app != nil {
			// Reason: a full signature check costs a read of the bundle, so
			// it runs only when a policy asks for evidence (caller_session.go).
			a.app = &appCheck{v: p.app, c: c, pid: pid}
		}
		return a
	}
	// Reason: read at accept, while the caller still runs under the
	// harness, so a later process taking over a PID does not match it.
	if a.harnessProc, err = p.harnessIdentity(hpid); err != nil {
		a.harnessErr = err
		return a
	}
	if a.outerHarnesses, err = p.outerHarnesses(hpid); err != nil {
		a.harnessErr = err
		return a
	}
	if ppid, err := p.parent(hpid); err != nil {
		a.harnessErr = fmt.Errorf("parent of harness process %d: %w", hpid, err)
	} else {
		a.harnessOrphan = ppid <= 1
	}
	return a
}

// harnessIdentity names harness process pid. Without a start-time reader
// its start time stays zero, so its session cannot be pinned.
func (p peerCredentials) harnessIdentity(pid int) (procstat.Identity, error) {
	id := procstat.Identity{PID: pid}
	if p.started == nil {
		return id, nil
	}
	started, err := p.started(pid)
	if err != nil {
		return id, fmt.Errorf("start time of harness process %d: %w", pid, err)
	}
	id.Started = started
	return id, nil
}

// outerHarnesses walks on from harness process pid and returns every
// harness above it, nearest first. A harness an agent starts inside its own
// session (or any program it names claude) is found here, so it cannot take
// a pin of its own.
func (p peerCredentials) outerHarnesses(pid int) ([]procstat.Identity, error) {
	var out []procstat.Identity
	for range maxAncestry {
		ppid, err := p.parent(pid)
		if err != nil {
			return nil, fmt.Errorf("parent of process %d: %w", pid, err)
		}
		if ppid <= 1 || ppid == pid {
			return out, nil
		}
		h, hpid, err := harnessAncestor(ppid, p.parent, p.program)
		if err != nil || h == "" {
			return out, err
		}
		id, err := p.harnessIdentity(hpid)
		if err != nil {
			return nil, err
		}
		out, pid = append(out, id), hpid
	}
	return nil, fmt.Errorf("process ancestry deeper than %d", maxAncestry)
}

// harnessNames are the program names of the agent harnesses spaces cover
// (D-20).
var harnessNames = map[string]bool{"claude": true, "codex": true}

// harnessPaths mark a harness by where it is installed, for installs whose
// program name says nothing: npm packages run under node, and Claude Code's
// native installer runs a binary named for its version.
var harnessPaths = map[string]string{
	"/@anthropic-ai/claude-code/": "claude",
	"/claude/versions/":           "claude",
	"/@openai/codex/":             "codex",
}

// interpreters run a script named in their arguments.
var interpreters = map[string]bool{"node": true, "bun": true, "deno": true}

// harnessOf names the harness prog is, or "". It looks at the kernel name,
// the executable, and the first two arguments (an interpreter and its script).
func harnessOf(prog procstat.Program) string {
	cands := []string{prog.Name, prog.Path}
	if len(prog.Args) > 2 {
		cands = append(cands, prog.Args[:2]...)
	} else {
		cands = append(cands, prog.Args...)
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if base := filepath.Base(c); harnessNames[base] {
			return base
		}
		for marker, name := range harnessPaths {
			if strings.Contains(c, marker) {
				return name
			}
		}
	}
	return ""
}

// harnessAncestor returns the name and PID of the nearest harness process at
// or above pid, or "" when the walk reaches launchd or init without one.
func harnessAncestor(pid int, parent func(int) (int, error), program func(int) (procstat.Program, error)) (string, int, error) {
	if program == nil {
		return "", 0, errors.New("process programs cannot be read")
	}
	for range maxAncestry {
		prog, err := program(pid)
		if err != nil {
			return "", 0, fmt.Errorf("program of process %d: %w", pid, err)
		}
		if h := harnessOf(prog); h != "" {
			return h, pid, nil
		}
		// An interpreter's script says what it runs; without its arguments
		// it could be a harness, so the caller cannot be cleared.
		if interpreters[prog.Name] && len(prog.Args) < 2 {
			return "", 0, fmt.Errorf("process %d runs %s, whose script cannot be read", pid, prog.Name)
		}
		ppid, err := parent(pid)
		if err != nil {
			return "", 0, fmt.Errorf("parent of process %d: %w", pid, err)
		}
		if ppid <= 1 || ppid == pid {
			return "", 0, nil
		}
		pid = ppid
	}
	return "", 0, fmt.Errorf("process ancestry deeper than %d", maxAncestry)
}

// operator reports whether ctx's caller is the operator, else describes it.
// Queued callers are refused separately, by refuseQueued.
func operator(ctx context.Context) (who string, ok bool) {
	if _, queued := queuedCaller(ctx); queued {
		return "a queued task", false
	}
	p, found := peer.FromContext(ctx)
	a, isCaller := callerAuth{}, false
	if found {
		a, isCaller = p.AuthInfo.(callerAuth)
	}
	switch {
	case !isCaller:
		return "unidentified process", false
	case a.err != nil:
		return fmt.Sprintf("a process orchd cannot identify (%v)", a.err), false
	case a.harnessErr != nil:
		return fmt.Sprintf("process %d, whose ancestry orchd cannot read", a.pid), false
	case a.harness != "":
		return fmt.Sprintf("process %d, inside a %s session", a.pid, a.harness), false
	}
	// Reason (D-71): under a spaces policy a process tree is no evidence of
	// the user, since an agent can detach a process from its session; only
	// Orch.app, recognized by its code signature, is the operator.
	if c, ok := ctx.Value(resolvedKey{}).(resolvedCaller); ok && c.evidenceNeeded && c.operatorBy == "" {
		return fmt.Sprintf("process %d, which is not Orch.app, while a spaces policy is applied", a.pid), false
	}
	return "", true
}

// refuseNonOperator refuses method unless ctx's caller is the operator: a
// process orchd identified, that orchd did not start and that does not run
// inside an agent harness. It is stricter than refuseQueued, which lets
// interactive sessions through, and fails closed when orchd cannot tell.
func refuseNonOperator(ctx context.Context, method, action string) error {
	if err := refuseQueued(ctx, method); err != nil {
		return err
	}
	who, ok := operator(ctx)
	if ok {
		return nil
	}
	log.Printf("orchd: refused %s from %s", method, who)
	return grpcstatus.Errorf(codes.PermissionDenied,
		"only the operator can %s: use Orch.app (while a spaces policy is applied, only Orch.app is the operator), or with no policy a terminal outside any agent session (caller: %s)", action, who)
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

// childOnPath returns ancestor's child on the walk up from pid (pid itself
// when ancestor is its parent), or 0 when pid does not descend from ancestor.
// A process is not its own descendant, so orchd calling itself is the
// operator.
func childOnPath(pid, ancestor int, parent func(int) (int, error)) (int, error) {
	if pid == ancestor {
		return 0, nil
	}
	for range maxAncestry {
		ppid, err := parent(pid)
		if err != nil {
			return 0, fmt.Errorf("parent of process %d: %w", pid, err)
		}
		switch {
		case ppid == ancestor:
			return pid, nil
		case ppid <= 1 || ppid == pid:
			return 0, nil
		}
		pid = ppid
	}
	return 0, fmt.Errorf("process ancestry deeper than %d", maxAncestry)
}

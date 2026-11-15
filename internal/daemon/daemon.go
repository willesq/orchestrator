// Package daemon implements orchd: it opens the SQLite store, constructs the
// (lazily-loaded) embedder and RAG service, and serves the Orchd gRPC service
// over a Unix-domain socket. Run takes a context so shutdown is testable; signal
// handling lives in the thin cmd/orchd entrypoint.
//
// orchd's long-lived pieces — the native qdrant engine, the workflow worker, the
// sources and docs schedulers, the PR babysitter — run as named supervisor units
// rather than bare goroutines, so `orch status` can report each one's liveness
// and `orch restart <name>` can bounce one without restarting the process.
package daemon

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/embed"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/landing"
	"github.com/willesq/orchestrator/internal/logs"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prunecap"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
	"github.com/willesq/orchestrator/internal/reviews"
	"github.com/willesq/orchestrator/internal/routing"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/selfupdate"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/supervisor"
	"github.com/willesq/orchestrator/internal/toollog"
	"github.com/willesq/orchestrator/internal/version"
	"github.com/willesq/orchestrator/internal/workflows"
)

// ragService is the subset of *rag.Service the gRPC handlers depend on.
type ragService interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
	Search(ctx context.Context, query string, opts rag.SearchOpts) ([]rag.Hit, string, error)
	ListSources(ctx context.Context, opts rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error)
	DeleteSource(ctx context.Context, uri string) (int, error)
}

// copyReader reads what is stored for a source (implemented by *rag.Service).
type copyReader interface {
	Copy(ctx context.Context, uri string) (rag.StoredCopy, bool, error)
}

// dispatcher enqueues durable agent-task workflows.
type dispatcher interface {
	Dispatch(ctx context.Context, agent, body, cwd string, rt workflows.Route) (string, error)
}

// taskReader reads the dispatch ledger. Read-only on purpose: task state is
// owned by the workflow engine, and letting callers write it would let the
// ledger disagree with the durable workflow it is supposed to describe.
type taskReader interface {
	List(ctx context.Context, f queue.ListFilter) ([]queue.Task, error)
	Get(ctx context.Context, id string) (queue.Task, bool, error)
}

// supervisorAPI is the subset of *supervisor.Supervisor the Status and Restart
// handlers depend on, so they are testable without live subsystems.
type supervisorAPI interface {
	Status() []supervisor.Status
	Restart(name string) error
	RestartAll() error
	Names() []string
}

type server struct {
	orchdv1.UnimplementedOrchdServer
	start time.Time
	rag   ragService
	disp  dispatcher
	sup   supervisorAPI
	tasks taskReader
	// quarantineCounts caches what quarantine holds for Status (SP-028).
	quarantineCounts quarantineCountCache
	// spaceCounts caches what the spaces hold for Status (SP-031).
	spaceCounts spaceCountCache
	// workerEnabled reports whether queued tasks will run, so a caller can say
	// plainly when a dispatch will sit pending instead.
	workerEnabled bool
	// ctl retries, cancels and reconciles tasks; live is the worker's current
	// runner, for liveness and cancel; home is where transcripts live. Each
	// is nil or empty when unavailable.
	ctl  *queue.Control
	live *liveRunner
	home string
	// hub fans task changes out to WatchTasks streams; nil when unavailable.
	hub *taskHub
	// stopping closes when orchd begins shutting down, ending open streams:
	// GracefulStop waits for every in-flight RPC, so a stream the app holds
	// open would otherwise keep orchd from exiting. nil never closes.
	stopping <-chan struct{}
	// summaries receives finished sessions for summarizing; nil when off.
	summaries *summaryQueue
	// router resolves dispatch routes and recommendations; nil, with routerErr
	// saying why, when the routing policy in config is invalid.
	router    modelRouter
	routerErr error
	// rater records task verdicts; nil when unavailable.
	rater taskRater
	// migrator moves the RAG store to the hybrid layout; nil when unavailable.
	migrator hybridMigrator
	// codexEnabled lets dispatch route to Codex, which the worker then runs.
	codexEnabled bool
	// prStateFile is the PR babysitter's cache, served by ListPRs.
	prStateFile string
	// github holds the GitHub scopes and identity; nil when unavailable.
	github *github.Service
	// reviews watches PRs for review and details reads PR metadata; each is
	// nil when unavailable.
	reviews *reviews.Watcher
	details *detailCache
	// landing decides what happens to commits tasks propose; nil when
	// unavailable.
	landing *landing.Service
	// handoffDir is where SetGitHubScope asks the PR watcher to sweep now.
	handoffDir string
	// system serves TailLogs and GetSystemMetrics; nil when unavailable.
	system *systemProbe
	// docsReg and sourcesReg back the Docs and Sources RPCs, and documents
	// rebuilds indexed sources; each is nil when unavailable.
	docsReg *docs.Registry
	// docsGH lists an owner's repos; docsRefresh is a new owner's cadence.
	docsGH      docs.GitHub
	docsRefresh int64
	// config serves the settings in orch.db.
	config *configService
	// spaces holds the enforced spaces policy; nil when unavailable.
	spaces *spaces.Engine
	// pins records each session's pin; nil when unavailable.
	pins *pinRegistry
	// files records the files sessions write outside every root (SP-029);
	// nil records none.
	files *sessionFiles
	// enforcement holds the enforcement switches (SP-070); nil without a
	// store, when every layer enforces.
	enforcement *enforcement
	// unlocks are the operator unlocks confirmed in Orch.app (SP-077).
	unlocks *unlockRegistry
	// checkouts keeps per-checkout harness settings (SP-046); nil when not
	// running.
	checkouts *checkoutSettings
	// guardVerdict is the guard's verdict on a tool event, recorded while
	// the guard is off or logging; nil until the guard decides (SP-040).
	guardVerdict guardVerdictFunc
	// terms caches the spaces' outbound terms, which the command log
	// keeps out of its records (SP-044).
	terms termsCache
	// guard writes the files the enforcing tool guard in orch-hook reads
	// (SP-041); nil writes none.
	guard *guardWriter
	// termsRules are the operator's outbound rules (SP-075).
	termsRules *termsRules
	// toolEvents takes the events pre-tool-use and cwd-changed hooks send.
	toolEvents toolEventIntake
	// toolLog stores them (SP-014); nil when it could not be opened.
	toolLog *toolLog
	// ghProfiles picks the gh login of each space's GitHub calls; nil runs
	// every call as the default login.
	ghProfiles *ghProfiles
	// claudeLogins checks the spaces' Claude Code configurations' logins
	// (SP-074.3); nil reports them unchecked.
	claudeLogins *claudeLogins
	// repos looks up a repo's owner and visibility for placement.
	repos      *repoLookups
	sourcesReg *sources.Registry
	documents  documentReader
	// nudgeMu guards lastNudge, when RefreshSource last restarted the
	// sources scheduler.
	nudgeMu   sync.Mutex
	lastNudge time.Time
	// docsSync runs docs syncs one at a time; nil when unavailable.
	docsSync docsSyncer
	// safetyNet runs cc-safety-net install actions one at a time; nil when
	// the user's home cannot be resolved.
	safetyNet *safetyNetManager
	// settings reconciles the Claude Code and cc-safety-net settings orch
	// manages; nil when the user's home cannot be resolved.
	settings *settings.Service
	// prune caps deletions by the docs and sources syncs; operators confirm
	// a larger prune through RefreshSource and SyncDocs.
	prune *prunecap.Guard
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_Ping_FullMethodName,
	spaceDecision{spaceNone, "orchd's version"}))

func (s *server) Ping(_ context.Context, _ *orchdv1.PingRequest) (*orchdv1.PingResponse, error) {
	return &orchdv1.PingResponse{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
	}, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_Ingest_FullMethodName, "write to the RAG store",
	spaceDecision{spaceNone, "a write, placed by orchd (SP-023); a copy in a space the caller does not read gets one refusal (SP-026, RC-701)"}))

func (s *server) Ingest(ctx context.Context, req *orchdv1.IngestRequest) (*orchdv1.IngestResponse, error) {
	origin := clientOrigin(ctx, req.GetSourceUri())
	// A source a sync keeps is not a client's to replace: the replacement
	// would hide the synced text until the sync next rewrote it.
	if c, ok := s.rag.(copyReader); ok {
		cp, found, err := c.Copy(ctx, req.GetSourceUri())
		if err == nil && found && (cp.Writer == rag.WriterDocs || cp.Writer == rag.WriterSources) {
			// Reason: under a policy a caller is not told which sync keeps
			// a copy in a space it does not read: it gets the refusal a
			// copy of another space gets (SP-026, RC-701).
			if !s.recordScope(ctx, orchdv1.Orchd_Ingest_FullMethodName).seesSpace(cp.Space) {
				return nil, errReplaceRefused
			}
			return nil, grpcstatus.Errorf(codes.FailedPrecondition,
				"%s is kept by the %s sync; store the note under another source_uri", req.GetSourceUri(), cp.Writer)
		}
		if err == nil && found && cp.Space != "" {
			if err := s.mayReplace(ctx, req.GetSourceUri(), origin, cp.Space); err != nil {
				return nil, err
			}
		}
	}
	// A URI, scope or tag is shown in every pointer and log line, and rewriting one
	// would change which source it names, so one holding a key is refused.
	for _, field := range append([]string{req.GetSourceUri(), req.GetScope(), req.GetSourceType()}, req.GetTags()...) {
		if _, n := redact.Redact(field); n > 0 {
			return nil, grpcstatus.Error(codes.InvalidArgument, "source_uri, scope, type and tags must not hold a secret")
		}
	}
	// Whatever a client was given to remember, a key in it is not worth keeping:
	// memory is replayed into later sessions' context.
	text, redacted := redact.Redact(req.GetText())
	if redacted > 0 {
		log.Printf("ingest: %d secret-shaped string(s) redacted from %s", redacted, logs.Clean(req.GetSourceUri()))
	}
	n, err := s.rag.IngestText(ctx, text, rag.Meta{
		SourceURI:  req.GetSourceUri(),
		SourceType: orDefault(req.GetSourceType(), "manual"),
		Scope:      req.GetScope(),
		Tags:       req.GetTags(),
		StartedAt:  fromUnix(req.GetStartedAt()),
		EndedAt:    fromUnix(req.GetEndedAt()),
		Timeline:   req.GetTimeline(),
		// Whatever type or URI a client declares, what it stores is a
		// client's: fenced as data wherever it is shown.
		Writer: rag.WriterClient,
		// Placed in the caller's own space, as orchd resolved it from the
		// caller's process; no request field names a space (SP-023).
		Origin: origin,
	})
	if err != nil {
		return nil, err
	}
	return &orchdv1.IngestResponse{Chunks: count32(n)}, nil
}

// maxSearchTopK bounds how many hits one search may ask for.
const maxSearchTopK = 100

var _ = registerRPCs(taskRPC(orchdv1.Orchd_Search_FullMethodName,
	spaceDecision{spaceFiltered, "memory search, read only from the caller's readable spaces' collections"}))

func (s *server) Search(ctx context.Context, req *orchdv1.SearchRequest) (*orchdv1.SearchResponse, error) {
	if m := req.GetMode(); m != "" && m != rag.ModeHybrid && m != rag.ModeDense {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "search mode %q must be hybrid or dense", m)
	}
	if k := req.GetTopK(); k < 0 || k > maxSearchTopK {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "top_k %d must be between 0 and %d", k, maxSearchTopK)
	}
	if r := req.GetRanking(); r != "" && r != rag.RankingWeighted && r != rag.RankingPlain {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "ranking %q must be weighted or plain", r)
	}
	if n := req.GetPerSource(); n > maxSearchTopK {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "per_source %d must be at most %d", n, maxSearchTopK)
	}
	if err := checkTags(req.GetTags(), req.GetAllTags()); err != nil {
		return nil, err
	}
	tr, err := timeRange(req.GetSince(), req.GetUntil())
	if err != nil {
		return nil, err
	}
	// The caller's spaces choose what is searched; the request's filters
	// only narrow within them (SP-025, INV-02).
	scope := s.readScope(ctx, orchdv1.Orchd_Search_FullMethodName)
	hits, fallback, err := s.rag.Search(ctx, req.GetQuery(), rag.SearchOpts{
		Spaces:     scope.spaces,
		LabelsV:    scope.labelsV,
		PerSource:  int(req.GetPerSource()),
		Ranking:    req.GetRanking(),
		TopK:       int(req.GetTopK()),
		Scope:      req.GetScope(),
		SourceType: req.GetSourceType(),
		Tags:       req.GetTags(),
		AllTags:    req.GetAllTags(),
		SourceURI:  req.GetSourceUri(),
		Mode:       req.GetMode(),
		Range:      tr,
	})
	if errors.Is(err, rag.ErrHybridUnavailable) {
		if scope.filtered() {
			// Reason: the detail names a collection, and a collection's name
			// holds its space's (RC-350).
			log.Printf("orchd: Search: %v", err)
			return nil, grpcstatus.Error(codes.FailedPrecondition, rag.ErrHybridUnavailable.Error()+" (see orchd log)")
		}
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, scope.storeError("Search", err)
	}
	out := &orchdv1.SearchResponse{Hits: make([]*orchdv1.SearchHit, 0, len(hits)), Fallback: fallback}
	for _, h := range hits {
		out.Hits = append(out.Hits, &orchdv1.SearchHit{
			Text:         h.Text,
			SourceUri:    h.SourceURI,
			SourceType:   h.SourceType,
			Score:        h.Score,
			Scope:        h.Scope,
			Tags:         h.Tags,
			KeywordScore: h.Keyword,
			Exact:        h.Exact,
			StartedAt:    toUnix(h.StartedAt),
			EndedAt:      toUnix(h.EndedAt),
			IngestedAt:   toUnix(h.IngestedAt),
			Heading:      h.Heading,
			Trust:        h.Trust,
		})
	}
	s.describeSearch(ctx, scope, out)
	return out, nil
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_ListSources_FullMethodName,
	spaceDecision{spaceFiltered, "memory inventory, listed only from the caller's readable spaces"}))

func (s *server) ListSources(ctx context.Context, req *orchdv1.ListSourcesRequest) (*orchdv1.ListSourcesResponse, error) {
	tr, err := timeRange(req.GetSince(), req.GetUntil())
	if err != nil {
		return nil, err
	}
	scope := s.readScope(ctx, orchdv1.Orchd_ListSources_FullMethodName)
	sources, distinct, chunks, err := s.rag.ListSources(ctx, rag.ListSourcesOpts{
		Spaces:     scope.spaces,
		LabelsV:    scope.labelsV,
		SourceType: req.GetSourceType(),
		Scope:      req.GetScope(),
		URIPrefix:  req.GetUriPrefix(),
		Limit:      int(req.GetLimit()),
		Range:      tr,
	})
	if err != nil {
		return nil, scope.storeError("ListSources", err)
	}
	out := &orchdv1.ListSourcesResponse{
		Sources:      make([]*orchdv1.SourceSummary, 0, len(sources)),
		TotalSources: count32(distinct),
		TotalChunks:  count32(chunks),
	}
	for _, src := range sources {
		out.Sources = append(out.Sources, &orchdv1.SourceSummary{
			SourceUri:  src.SourceURI,
			SourceType: src.SourceType,
			Scope:      src.Scope,
			Chunks:     count32(src.Chunks),
			Tags:       src.Tags,
			StartedAt:  toUnix(src.StartedAt),
			EndedAt:    toUnix(src.EndedAt),
			IngestedAt: toUnix(src.IngestedAt),
		})
	}
	return out, nil
}

// timeRange reads a request's since and until (Unix seconds; 0 = open).
func timeRange(since, until int64) (rag.TimeRange, error) {
	if since < 0 || until < 0 {
		return rag.TimeRange{}, grpcstatus.Error(codes.InvalidArgument, "since and until must not be negative")
	}
	if since > 0 && until > 0 && since > until {
		return rag.TimeRange{}, grpcstatus.Errorf(codes.InvalidArgument, "since %d must not be after until %d", since, until)
	}
	return rag.TimeRange{Since: fromUnix(since), Until: fromUnix(until)}, nil
}

func fromUnix(s int64) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_DeleteSource_FullMethodName, "delete from the RAG store",
	spaceDecision{spaceNone, "a delete; under a policy a session deletes only from its own space and is told only that count (D-77)"}))

// DeleteSource permanently removes a source's chunks.
//
// This is the only unrecoverable operation orchd exposes, so it is gated and
// logged here rather than in any one client: a gate that lives in the CLI only
// protects the caller that already chose to use the CLI, and an agent reaching
// the same RPC over MCP would bypass it entirely.
func (s *server) DeleteSource(ctx context.Context, req *orchdv1.DeleteSourceRequest) (*orchdv1.DeleteSourceResponse, error) {
	uri := req.GetSourceUri()
	if uri == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "source_uri is required")
	}
	if !req.GetConfirm() {
		return nil, grpcstatus.Error(codes.FailedPrecondition,
			"refusing to delete without confirm=true: this is permanent and cannot be undone")
	}
	if s.spacesConfigured() {
		if c := callerOf(ctx); c.kind != callerOperator || c.operatorBy == "" {
			return s.deleteInOwnSpace(ctx, c, uri)
		}
	}
	log.Printf("orchd: DELETE source %q — permanent, no undo", uri)
	n, err := s.rag.DeleteSource(ctx, uri)
	if err != nil {
		log.Printf("orchd: DELETE source %q failed: %v", uri, err)
		return nil, err
	}
	log.Printf("orchd: DELETE source %q removed %d chunk(s)", uri, n)
	return &orchdv1.DeleteSourceResponse{Deleted: count32(n)}, nil
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_ListTasks_FullMethodName,
	spaceDecision{spaceRecords, "the task ledger, by the space each task recorded at dispatch (SP-026)"}))

func (s *server) ListTasks(ctx context.Context, req *orchdv1.ListTasksRequest) (*orchdv1.ListTasksResponse, error) {
	if s.tasks == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "task ledger unavailable")
	}
	list, err := s.tasks.List(ctx, queue.ListFilter{
		Status: req.GetStatus(),
		Agent:  req.GetAgent(),
		Limit:  int(req.GetLimit()),
	})
	if err != nil {
		return nil, err
	}
	// Reason: the limit applies before the filter, so a caller sees at
	// most limit tasks of its spaces, possibly fewer (SP-026).
	list = s.tasksFor(ctx, s.recordScope(ctx, orchdv1.Orchd_ListTasks_FullMethodName), list)
	return &orchdv1.ListTasksResponse{Tasks: s.tasksPB(ctx, list)}, nil
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_GetTask_FullMethodName,
	spaceDecision{spaceRecords, "one task's record and result; another space's task is not found (SP-026)"}))

func (s *server) GetTask(ctx context.Context, req *orchdv1.GetTaskRequest) (*orchdv1.GetTaskResponse, error) {
	if s.tasks == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "task ledger unavailable")
	}
	t, found, err := s.tasks.Get(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	// Reason: another space's task reads as one that does not exist
	// (SP-026), so its record and result stay in its space.
	if !found || !s.seesTask(ctx, s.recordScope(ctx, orchdv1.Orchd_GetTask_FullMethodName), t.ID) {
		return &orchdv1.GetTaskResponse{Found: false}, nil
	}
	pb := taskPB(t)
	pb.Run = s.taskRun(ctx, t)
	return &orchdv1.GetTaskResponse{Task: pb, Found: true}, nil
}

// count32 narrows a store count for the wire without wrapping. The counts are
// our own and realistically small, but a silent wrap would turn "too many" into
// a negative or zero — a worse answer than a saturated one.
func count32(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

func taskPB(t queue.Task) *orchdv1.Task {
	return &orchdv1.Task{
		Id: t.ID, Agent: t.Agent, Body: t.Body,
		Result: t.Result, Status: t.Status, UpdatedAt: t.UpdatedAt,
		CostUsd: t.CostUSD, DurationMs: t.DurationMS, Turns: int32(t.Turns),
		SessionId: t.SessionID, Models: t.Models, Prs: t.PRs, Verification: t.Verification,
		Route: taskRoutePB(t), Rating: t.Rating, PrOutcome: t.PROutcome,
	}
}

func taskRoutePB(t queue.Task) *orchdv1.Route {
	if t.Kind == "" && t.Model == "" {
		return nil
	}
	return &orchdv1.Route{TaskKind: t.Kind, Provider: t.Provider, Model: t.Model, Effort: t.Effort}
}

// maxTaskBody bounds a dispatched task's prompt. A task is a brief, not a
// payload; the limit keeps one request from bloating the ledger and every
// queue_read of it.
const maxTaskBody = 64 << 10

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_Dispatch_FullMethodName, "dispatch further tasks",
	spaceDecision{spaceNone, "queues a task"}))

func (s *server) Dispatch(ctx context.Context, req *orchdv1.DispatchRequest) (*orchdv1.DispatchResponse, error) {
	agent := orDefault(req.GetAgent(), "dev")
	// Validated here as well as in the runner so a bad request fails at dispatch,
	// visibly, rather than becoming a durable task that can only fail later.
	if !agentrun.ValidAgent(agent) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid agent name %q", agent)
	}
	if strings.TrimSpace(req.GetBody()) == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "body is required")
	}
	if len(req.GetBody()) > maxTaskBody {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "body is %d bytes; the limit is %d", len(req.GetBody()), maxTaskBody)
	}
	if cwd := req.GetCwd(); cwd != "" && !filepath.IsAbs(cwd) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "cwd %q must be absolute", cwd)
	}
	rt, routePB, err := s.route(ctx, req)
	if err != nil {
		return nil, err
	}
	// The task reads the memory of the space recorded now (D-46).
	id, err := s.disp.Dispatch(queue.WithReadSpace(ctx, s.dispatchSpace(ctx, req.GetCwd())), agent, req.GetBody(), req.GetCwd(), rt)
	if errors.Is(err, queue.ErrQueueFull) {
		return nil, grpcstatus.Error(codes.ResourceExhausted, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &orchdv1.DispatchResponse{InstanceId: id, WorkerEnabled: s.workerEnabled, Route: routePB}, nil
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_Status_FullMethodName,
	spaceDecision{spaceNone, "orchd's subsystems"}))

// Status reports orchd's own liveness plus one entry per supervised subsystem.
func (s *server) Status(ctx context.Context, _ *orchdv1.StatusRequest) (*orchdv1.StatusResponse, error) {
	resp := &orchdv1.StatusResponse{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
		Migration:     s.migrationProgress(),
		Spaces:        s.spacesStatus(ctx),
	}
	if s.sup == nil {
		return resp, nil
	}
	for _, st := range s.sup.Status() {
		resp.Subsystems = append(resp.Subsystems, &orchdv1.SubsystemStatus{
			Name:      st.Name,
			State:     st.State,
			Detail:    st.Detail,
			Error:     st.Err,
			StartedAt: unixOrZero(st.StartedAt),
			LastBeat:  unixOrZero(st.LastBeat),
			Restarts:  clampInt32(st.Restarts),
		})
	}
	return resp, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_Restart_FullMethodName, "restart orchd subsystems",
	spaceDecision{spaceNone, "restarts subsystems"}))

// Restart bounces one supervised subsystem in place, or every subsystem when no
// name is given. The orchd process itself is never restarted here — that is the
// service manager's job (`orch restart` with no component).
func (s *server) Restart(_ context.Context, req *orchdv1.RestartRequest) (*orchdv1.RestartResponse, error) {
	if s.sup == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "orchd is running without supervised subsystems")
	}
	if name := req.GetName(); name != "" {
		if err := s.sup.Restart(name); err != nil {
			if errors.Is(err, supervisor.ErrUnknownUnit) {
				return nil, grpcstatus.Errorf(codes.NotFound, "%v (known subsystems: %s)",
					err, strings.Join(s.sup.Names(), ", "))
			}
			return nil, grpcstatus.Error(codes.Internal, err.Error())
		}
		return &orchdv1.RestartResponse{Restarted: []string{name}}, nil
	}
	if err := s.sup.RestartAll(); err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	return &orchdv1.RestartResponse{Restarted: s.sup.Names()}, nil
}

// unixOrZero renders a timestamp for the wire, mapping the zero time to 0 so a
// client can tell "never" apart from "at the epoch".
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// clampInt32 saturates rather than wrapping, so an absurd restart count reports
// as a very large number instead of a negative one.
func clampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return int32(n)
	}
}

// Run serves orchd until ctx is canceled.
// logRingSize is how many recent log lines orchd keeps for TailLogs.
const logRingSize = 5000

func Run(ctx context.Context) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	// Reason: launchd (and `brew services`) start orchd with PATH=/usr/bin:/bin:
	// /usr/sbin:/sbin, which hides Homebrew-installed CLIs that subsystems shell
	// out to — gh most importantly, without which docs sync and PR polling fail.
	if err := paths.EnsureToolPATH(); err != nil {
		return err
	}
	// Keep recent log lines in memory for TailLogs, alongside wherever log
	// already writes (the service manager's log file).
	ring := logs.NewRing(logRingSize)
	prevOutput := log.Writer()
	log.SetOutput(io.MultiWriter(prevOutput, ring.LogWriter()))
	defer log.SetOutput(prevOutput)
	system := newSystemProbe(ring)

	store, err := db.Open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	importLegacyConfig(ctx, store)
	v, err := config.LoadStore(ctx, store)
	if err != nil {
		return err
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		return err
	}

	// What this process runs with, so GetConfig can say what a restart would change.
	configStart, err := config.Snapshot(v)
	if err != nil {
		return err
	}
	cfg := newConfigService(store, configStart)
	ghp := newGHProfiles(s.GitHub.IsolatedAccountHourlyBudget)
	sp, err := spaces.Open(ctx, store, spacesEnv(ghp))
	if err != nil {
		return err
	}
	ghp.spaces = sp
	// Reason: only the load result is logged here. Status would validate the
	// newest draft, which anyone may save, and must not hold up startup.
	if _, err := sp.Current(); err != nil {
		log.Printf("orchd: spaces: %v; no space is readable until a revision is applied again", err)
	}
	// Reason: from here a failure is most often a setting (a model folder, a
	// port, a storage path), and settings can only be changed through orchd.
	// Exiting would leave nothing to fix them with, so orchd serves them alone.
	recovery := func(err error) error { return serveRecovery(ctx, cfg, err, policyApplied(sp)) }

	// Resolve the qdrant API key once (persisted, generated on first run) so the
	// engine and the client share it independent of any ambient env var.
	apiKey, err := resolveQdrantAPIKey(s)
	if err != nil {
		return recovery(err)
	}

	// First run downloads the embedding model so search/ingest work out of the box.
	if err := ensureModel(ctx, s); err != nil {
		return recovery(err)
	}

	svc, emb := buildRAG(s, s.QdrantURL(), apiKey)
	defer emb.Close()
	// Every write is placed from here on (SP-023); set before any writer
	// starts, so nothing is stored unplaced.
	pins := newPinRegistry(store)
	repos := newRepoLookups(ghp.Routed())
	files := newSessionFiles(store)
	placer := &writePlacer{spaces: sp, pins: pins, repos: repos, files: files}
	svc.Place = placer.place

	// Durable workflow engine: the worker unit drains AgentTask instances; the
	// dispatcher (used by the RPC handler, not a subsystem) enqueues them.
	b, err := queue.NewBackend()
	if err != nil {
		return recovery(err)
	}
	defer func() { _ = b.Close() }()
	tasks := queue.NewSQLTaskStore(store)
	hub := newTaskHub()
	tasks.OnChange = hub.publish
	system.tasks = tasks.CountByStatus
	system.data = []dataPath{
		{name: "database", path: paths.DBFile()},
		{name: "workflows", path: queue.BackendFile()},
		{name: "qdrant", path: s.QdrantStorageDir()},
		{name: "models", path: resolveModelDir(s)},
	}
	workflows.SetStore(tasks)
	workflows.SetNotifier(notifyTaskDone)
	// The run index backs retry, cancel, liveness and the worker's
	// reconcile-on-start; without it those are unavailable, not fatal.
	var ctl *queue.Control
	live := &liveRunner{}
	if runs, err := queue.OpenRuns(queue.BackendFile(), b); err != nil {
		log.Printf("orchd: task run index unavailable: %v", err)
	} else {
		defer func() { _ = runs.Close() }()
		ctl = &queue.Control{Store: tasks, Runs: runs, CancelLive: live.cancel, MaxActive: s.Queue.MaxActive}
	}
	disp := queue.NewDispatcher(b, tasks)
	disp.MaxActive = s.Queue.MaxActive
	disp.Snapshot = agentrun.Snapshot

	// Reason: the self-update unit restarts orchd by ending this process, and it
	// must take the same shutdown path as SIGTERM. Serving therefore hangs off a
	// context orchd can cancel itself, derived from the caller's.
	runCtx, stopSelf := context.WithCancel(ctx)
	defer stopSelf()
	restart := func(reason string) {
		log.Printf("orchd %s: %s — exiting so the service manager starts the new one",
			version.Version, reason)
		stopSelf()
	}

	watcher, supervised := selfUpdateWatch()

	var summaries *summaryQueue
	if s.Sessions.Summarize.Enabled {
		summaries = newSummaryQueue()
	}

	// One GitHub service, so the server and the PR watcher share the identity
	// cache.
	ghService := github.NewService(github.NewStore(store), ghp.Routed())
	ghService.Accounts = ghp
	reviewWatcher := newReviewWatcher(reviews.NewStore(store), disp, tasks, ctl, ghService, ghp)
	reviewWatcher.Poll = reviewsPoll(s.PRReviews.PollInterval)

	docsSched := newDocsScheduler(s, store, svc, sp, ghp, placer)
	settingsSvc := settingsFor(store)
	guardFiles := defaultGuardWriter()
	sup := supervisor.New(runCtx)
	for _, u := range subsystems(deps{
		settings: s, apiKey: apiKey, rag: svc, backend: b, store: store,
		selfUpdate: watcher, supervised: supervised, restart: restart,
		summaries: summaries, github: ghService, ghProfiles: ghp, system: system, docs: docsSched,
		settingsSvc: settingsSvc, taskCtl: ctl, live: live, taskHub: hub, reviews: reviewWatcher, spaces: sp, placer: placer,
		taskPins: &taskPinner{pins: pins, run: live.run, spaces: sp, guard: guardFiles},
	}) {
		if err := sup.Add(u); err != nil {
			return recovery(err)
		}
	}
	if err := sup.StartAll(); err != nil {
		// Units that did start would hold their ports and files meanwhile.
		_ = sup.StopAll()
		return recovery(err)
	}
	defer func() { _ = sup.StopAll() }()

	lis, closeSocket, err := listenSocket()
	if err != nil {
		return err
	}
	defer closeSocket()

	router, routerErr := newRouter(store, routing.PolicyFrom(s.Routing))
	if routerErr != nil {
		log.Printf("orchd: model routing is off: %v", routerErr)
	}

	// Reason: the resolver needs the server, which is built after the log;
	// it runs only once events arrive, after srv is set.
	var srv *server
	tlog, err := openToolLogCopying(paths.ToolLogFile(), func(k toollog.Key) toollog.Owner { return srv.resolveClaim(k) }, store)
	if err != nil {
		log.Printf("orchd: command log is off: %v", err)
	} else {
		defer func() { _ = tlog.Close() }()
		tlog.policy = logPolicy(s.Sessions.CommandLog)
		tlog.taskStatus = tasks.Status
	}
	unlocks := newUnlockRegistry()
	gs := newGRPCServer(&callerResolver{pins: pins, taskOf: live.taskOf, needsEvidence: policyApplied(sp), unlocks: unlocks})
	srv = &server{
		start: time.Now(), rag: svc, disp: disp, sup: sup, tasks: tasks, ctl: ctl, live: live, home: userHome(), hub: hub,
		stopping:      runCtx.Done(),
		workerEnabled: s.Queue.Worker.Runs(),
		summaries:     summaries,
		router:        router, routerErr: routerErr,
		rater:        dbRater{db: store},
		migrator:     svc,
		codexEnabled: codexRunnable(s.Queue.Worker),
		prStateFile:  paths.PRStateFile(),
		github:       ghService,
		reviews:      reviewWatcher,
		landing:      newLandingService(landing.NewStore(store), tasks, ghService, ghp),
		details:      newDetailCache(ghp.Routed()),
		handoffDir:   handoff.Dir(),
		system:       system,
		docsReg:      docs.NewRegistry(store),
		docsGH:       docs.NewGitHub(),
		docsRefresh:  refreshSecs(s.Docs.DefaultRefresh),
		config:       cfg,
		spaces:       sp,
		pins:         pins,
		files:        files,
		repos:        repos,
		ghProfiles:   ghp,
		claudeLogins: newClaudeLogins(),
		prune:        newPruneGuard(store),
		sourcesReg:   sources.NewRegistry(store),
		documents:    svc,
		docsSync:     docsSched,
		safetyNet:    safetyNetFor(s),
		settings:     settingsSvc,
		toolLog:      tlog,
		enforcement:  newEnforcement(ctx, store, settingsSvc),
		termsRules:   newTermsRules(ctx, store),
		guard:        guardFiles,
		unlocks:      unlocks,
	}
	srv.startGuardFiles(runCtx)
	if tlog != nil {
		srv.toolEvents.setSink(srv.logToolEvent)
	}
	go srv.runSessionSweeps(runCtx, sessionSweepEvery)
	go srv.runToolLogSweeps(runCtx, logSweepEvery)
	if srv.enforcement != nil {
		go srv.enforcement.run(runCtx, enforcementTick)
	}
	srv.checkouts = newCheckoutSettings()
	if srv.enforcement != nil {
		srv.enforcement.onSandbox = srv.checkouts.nudge
	}
	go srv.runCheckoutSettings(runCtx, checkoutSettingsEvery)
	go srv.runTaskAnchors(runCtx)
	go srv.resumeRetags(runCtx, resumeRetagsDelay)
	orchdv1.RegisterOrchdServer(gs, srv)
	go func() {
		<-runCtx.Done()
		stopGracefully(gs, gracefulStopTimeout)
	}()

	log.Printf("orchd %s listening on %s (subsystems: %s)",
		version.Version, lis.Addr(), strings.Join(sup.Names(), ", "))
	err = gs.Serve(lis)
	// Reason: a shutdown that begins before Serve starts (a new build
	// installed during startup) stops the server first, and Serve then
	// reports it stopped; that is a clean exit like any other shutdown.
	if errors.Is(err, grpc.ErrServerStopped) && runCtx.Err() != nil {
		return nil
	}
	return err
}

// selfUpdateWatch reports the binary orchd should watch for a new build of
// itself, and whether exiting would actually be picked up by a service manager.
//
// The path watched is the one orchd was launched from — the stable path the
// service manager will re-exec (the Homebrew opt symlink, or the built binary)
// rather than the versioned path it happens to resolve to today.
//
// A process that cannot identify or read its own executable simply goes without
// the self-update unit; every other subsystem still comes up. It is a variable
// so a test can drive the self-restart path against a binary it controls.
var selfUpdateWatch = func() (*selfupdate.Watcher, bool) {
	exe, err := os.Executable()
	if err == nil {
		var w *selfupdate.Watcher
		if w, err = selfupdate.NewWatcher(exe); err == nil {
			log.Printf("self-update: watching %s for new builds (%s)", w.Path(), w.Baseline())
			return w, selfupdate.Supervised()
		}
	}
	log.Printf("self-update: watch disabled: %v", err)
	return nil, false
}

// buildRAG constructs the embedder + RAG service from config against a qdrant URL.
func buildRAG(s config.Settings, qdrantURL, apiKey string) (*rag.Service, *embed.Embedder) {
	onnxDir := s.Embedder.OnnxLibDir
	if onnxDir == "" {
		onnxDir = resolveOnnxLibDir()
	}
	idle, _ := time.ParseDuration(s.Embedder.IdleUnload)

	emb := embed.New(embed.Config{
		ModelDir:   resolveModelDir(s),
		OnnxLibDir: onnxDir,
		IdleUnload: idle,
		Dimension:  384,
	})

	qs := rag.NewQdrantStore(qdrantURL, s.RAG.Qdrant.Collection, apiKey)
	svc := rag.NewService(emb, qs, s.Embedder.ChunkTokens, s.Embedder.ChunkOverlapTokens)
	svc.MarkdownSections = s.Embedder.MarkdownSections
	svc.StateDir = filepath.Join(paths.DataDir(), "migrations")
	if m := s.RAG.Search.Mode; m == rag.ModeHybrid || m == rag.ModeDense {
		svc.SearchMode = m
	} else if m != "" {
		log.Printf("orchd: rag.search.mode %q is not hybrid or dense; using hybrid", m)
	}
	if s.RAG.Search.RRFK > 0 {
		svc.RRFK = s.RAG.Search.RRFK
	}
	if w := s.RAG.Search.KeywordWeight; w > 0 {
		svc.KeywordWeight = w
	}
	svc.Ranking = rankingParams(s.RAG.Search)
	svc.PerSource = max(s.RAG.Search.PerSource, 0)
	return svc, emb
}

// resolveModelDir is the embedding model directory (config override or default).
func resolveModelDir(s config.Settings) string {
	if s.Embedder.ModelDir != "" {
		return s.Embedder.ModelDir
	}
	return filepath.Join(paths.DataDir(), "models", "minilm")
}

// ensureModel downloads the embedding model on first run if it is not present.
func ensureModel(ctx context.Context, s config.Settings) error {
	dir := resolveModelDir(s)
	mp := model.New(model.Config{Dir: dir})
	if mp.Present() {
		return nil
	}
	log.Printf("orchd: fetching embedding model into %s ...", dir)
	return mp.Ensure(ctx)
}

// resolveQdrantAPIKey returns the native engine's API key: an explicit env
// override if set, otherwise a key persisted under the data dir (generated on
// first run) so the engine and client always agree without an ambient env var.
func resolveQdrantAPIKey(s config.Settings) (string, error) {
	if k := os.Getenv(orDefault(s.RAG.Qdrant.APIKeyEnv, "QDRANT_API_KEY")); k != "" {
		return k, nil
	}
	return ensureQdrantAPIKey(filepath.Join(paths.DataDir(), "qdrant-api-key"))
}

// ensureQdrantAPIKey reads the persisted key, generating and storing one (0600)
// if it is absent or empty.
func ensureQdrantAPIKey(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	key := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, nil
}

// resolveOnnxLibDir finds the onnxruntime shared-library directory via Homebrew,
// falling back to the conventional Apple-silicon opt path.
func resolveOnnxLibDir() string {
	if out, err := exec.Command("brew", "--prefix", "onnxruntime").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return filepath.Join(p, "lib")
		}
	}
	return "/opt/homebrew/opt/onnxruntime/lib"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// expandRoots expands a leading ~ in each configured source root.
func expandRoots(roots []string) []string {
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if home != "" && (r == "~" || strings.HasPrefix(r, "~/")) {
			r = filepath.Join(home, strings.TrimPrefix(r, "~"))
		}
		out = append(out, r)
	}
	return out
}

// codexRunnable reports whether the worker will run Codex tasks: it must be on,
// Codex enabled for it, and the CLI findable.
func codexRunnable(w config.QueueWorkerSettings) bool {
	if !w.Enabled || !w.Codex.Enabled {
		return false
	}
	home, _ := os.UserHomeDir()
	return agentrun.FindCodex(w.Codex.Bin, home) != ""
}

func newPruneGuard(store *sql.DB) *prunecap.Guard {
	if store == nil {
		return nil
	}
	return prunecap.New(store)
}

// gracefulStopTimeout bounds how long shutdown waits for in-flight RPCs.
const gracefulStopTimeout = 5 * time.Second

// stopGracefully lets in-flight RPCs finish, then stops the server outright
// if they have not within timeout.
//
// Reason: GracefulStop waits for every RPC and cancels none, so one a client
// keeps open (a stream that missed the shutdown signal, a stuck handler)
// would otherwise keep orchd from exiting and stall a self-update.
func stopGracefully(gs interface {
	GracefulStop()
	Stop()
}, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("orchd: RPCs still open %s after shutdown began; stopping them", timeout)
		gs.Stop()
		<-done
	}
}

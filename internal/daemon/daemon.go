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
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/embed"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/routing"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/selfupdate"
	"github.com/willesq/orchestrator/internal/supervisor"
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
	// workerEnabled reports whether queued tasks will run, so a caller can say
	// plainly when a dispatch will sit pending instead.
	workerEnabled bool
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
	// handoffDir is where SetGitHubScope asks the PR watcher to sweep now.
	handoffDir string
}

func (s *server) Ping(_ context.Context, _ *orchdv1.PingRequest) (*orchdv1.PingResponse, error) {
	return &orchdv1.PingResponse{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
	}, nil
}

func (s *server) Ingest(ctx context.Context, req *orchdv1.IngestRequest) (*orchdv1.IngestResponse, error) {
	n, err := s.rag.IngestText(ctx, req.GetText(), rag.Meta{
		SourceURI:  req.GetSourceUri(),
		SourceType: orDefault(req.GetSourceType(), "manual"),
		Scope:      req.GetScope(),
		Tags:       req.GetTags(),
		StartedAt:  fromUnix(req.GetStartedAt()),
		EndedAt:    fromUnix(req.GetEndedAt()),
		Timeline:   req.GetTimeline(),
	})
	if err != nil {
		return nil, err
	}
	return &orchdv1.IngestResponse{Chunks: count32(n)}, nil
}

// maxSearchTopK bounds how many hits one search may ask for.
const maxSearchTopK = 100

func (s *server) Search(ctx context.Context, req *orchdv1.SearchRequest) (*orchdv1.SearchResponse, error) {
	if m := req.GetMode(); m != "" && m != rag.ModeHybrid && m != rag.ModeDense {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "search mode %q must be hybrid or dense", m)
	}
	if k := req.GetTopK(); k < 0 || k > maxSearchTopK {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "top_k %d must be between 0 and %d", k, maxSearchTopK)
	}
	tr, err := timeRange(req.GetSince(), req.GetUntil())
	if err != nil {
		return nil, err
	}
	hits, fallback, err := s.rag.Search(ctx, req.GetQuery(), rag.SearchOpts{
		TopK:       int(req.GetTopK()),
		Scope:      req.GetScope(),
		SourceType: req.GetSourceType(),
		Tags:       req.GetTags(),
		Mode:       req.GetMode(),
		Range:      tr,
	})
	if errors.Is(err, rag.ErrHybridUnavailable) {
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, err
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
		})
	}
	return out, nil
}

func (s *server) ListSources(ctx context.Context, req *orchdv1.ListSourcesRequest) (*orchdv1.ListSourcesResponse, error) {
	tr, err := timeRange(req.GetSince(), req.GetUntil())
	if err != nil {
		return nil, err
	}
	sources, distinct, chunks, err := s.rag.ListSources(ctx, rag.ListSourcesOpts{
		SourceType: req.GetSourceType(),
		Scope:      req.GetScope(),
		URIPrefix:  req.GetUriPrefix(),
		Limit:      int(req.GetLimit()),
		Range:      tr,
	})
	if err != nil {
		return nil, err
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
	log.Printf("orchd: DELETE source %q — permanent, no undo", uri)
	n, err := s.rag.DeleteSource(ctx, uri)
	if err != nil {
		log.Printf("orchd: DELETE source %q failed: %v", uri, err)
		return nil, err
	}
	log.Printf("orchd: DELETE source %q removed %d chunk(s)", uri, n)
	return &orchdv1.DeleteSourceResponse{Deleted: count32(n)}, nil
}

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
	out := &orchdv1.ListTasksResponse{Tasks: make([]*orchdv1.Task, 0, len(list))}
	for _, t := range list {
		out.Tasks = append(out.Tasks, taskPB(t))
	}
	return out, nil
}

func (s *server) GetTask(ctx context.Context, req *orchdv1.GetTaskRequest) (*orchdv1.GetTaskResponse, error) {
	if s.tasks == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "task ledger unavailable")
	}
	t, found, err := s.tasks.Get(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if !found {
		return &orchdv1.GetTaskResponse{Found: false}, nil
	}
	return &orchdv1.GetTaskResponse{Task: taskPB(t), Found: true}, nil
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
	id, err := s.disp.Dispatch(ctx, agent, req.GetBody(), req.GetCwd(), rt)
	if errors.Is(err, queue.ErrQueueFull) {
		return nil, grpcstatus.Error(codes.ResourceExhausted, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &orchdv1.DispatchResponse{InstanceId: id, WorkerEnabled: s.workerEnabled, Route: routePB}, nil
}

// Status reports orchd's own liveness plus one entry per supervised subsystem.
func (s *server) Status(_ context.Context, _ *orchdv1.StatusRequest) (*orchdv1.StatusResponse, error) {
	resp := &orchdv1.StatusResponse{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
		Migration:     s.migrationProgress(),
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
	store, err := db.Open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	v, err := config.Load()
	if err != nil {
		return err
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		return err
	}

	// Resolve the qdrant API key once (persisted, generated on first run) so the
	// engine and the client share it independent of any ambient env var.
	apiKey, err := resolveQdrantAPIKey(s)
	if err != nil {
		return err
	}

	// First run downloads the embedding model so search/ingest work out of the box.
	if err := ensureModel(ctx, s); err != nil {
		return err
	}

	svc, emb := buildRAG(s, s.QdrantURL(), apiKey)
	defer emb.Close()

	// Durable workflow engine: the worker unit drains AgentTask instances; the
	// dispatcher (used by the RPC handler, not a subsystem) enqueues them.
	b, err := queue.NewBackend()
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	tasks := queue.NewSQLTaskStore(store)
	workflows.SetStore(tasks)
	workflows.SetNotifier(notifyTaskDone)
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
	ghService := github.NewService(github.NewStore(store), github.GH)

	sup := supervisor.New(runCtx)
	for _, u := range subsystems(deps{
		settings: s, apiKey: apiKey, rag: svc, backend: b, store: store,
		selfUpdate: watcher, supervised: supervised, restart: restart,
		summaries: summaries, github: ghService,
	}) {
		if err := sup.Add(u); err != nil {
			return err
		}
	}
	if err := sup.StartAll(); err != nil {
		return err
	}
	defer func() { _ = sup.StopAll() }()

	sock := paths.SocketPath()
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(sock) }()
	// The socket is the only trust boundary: anything that can connect gets the
	// whole RPC surface (Dispatch, Restart, the RAG store). Left to the ambient
	// umask the mode is an accident, so pin it to owner-only rather than relying
	// on the caller's environment.
	if err := os.Chmod(sock, 0o600); err != nil {
		return fmt.Errorf("secure socket %s: %w", sock, err)
	}

	router, routerErr := newRouter(store, routing.PolicyFrom(s.Routing))
	if routerErr != nil {
		log.Printf("orchd: model routing is off: %v", routerErr)
	}

	gs := grpc.NewServer()
	orchdv1.RegisterOrchdServer(gs, &server{
		start: time.Now(), rag: svc, disp: disp, sup: sup, tasks: tasks,
		workerEnabled: s.Queue.Worker.Enabled,
		summaries:     summaries,
		router:        router, routerErr: routerErr,
		rater:        dbRater{db: store},
		migrator:     svc,
		codexEnabled: codexRunnable(s.Queue.Worker),
		prStateFile:  paths.PRStateFile(),
		github:       ghService,
		handoffDir:   handoff.Dir(),
	})
	go func() {
		<-runCtx.Done()
		gs.GracefulStop()
	}()

	log.Printf("orchd %s listening on %s (subsystems: %s)",
		version.Version, sock, strings.Join(sup.Names(), ", "))
	return gs.Serve(lis)
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
			log.Printf("orchd: watching %s for new builds (%s)", w.Path(), w.Baseline())
			return w, selfupdate.Supervised()
		}
	}
	log.Printf("orchd: self-update watch disabled: %v", err)
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

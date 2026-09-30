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

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/embed"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/supervisor"
	"github.com/willesq/orchestrator/internal/version"
	"github.com/willesq/orchestrator/internal/workflows"
)

// ragService is the subset of *rag.Service the gRPC handlers depend on.
type ragService interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
	Search(ctx context.Context, query string, opts rag.SearchOpts) ([]rag.Hit, error)
}

// dispatcher enqueues durable agent-task workflows.
type dispatcher interface {
	Dispatch(ctx context.Context, agent, body string) (string, error)
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
	})
	if err != nil {
		return nil, err
	}
	return &orchdv1.IngestResponse{Chunks: int32(n)}, nil
}

func (s *server) Search(ctx context.Context, req *orchdv1.SearchRequest) (*orchdv1.SearchResponse, error) {
	hits, err := s.rag.Search(ctx, req.GetQuery(), rag.SearchOpts{
		TopK:       int(req.GetTopK()),
		Scope:      req.GetScope(),
		SourceType: req.GetSourceType(),
		Tags:       req.GetTags(),
	})
	if err != nil {
		return nil, err
	}
	out := &orchdv1.SearchResponse{Hits: make([]*orchdv1.SearchHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, &orchdv1.SearchHit{
			Text:       h.Text,
			SourceUri:  h.SourceURI,
			SourceType: h.SourceType,
			Score:      h.Score,
			Scope:      h.Scope,
			Tags:       h.Tags,
		})
	}
	return out, nil
}

func (s *server) Dispatch(ctx context.Context, req *orchdv1.DispatchRequest) (*orchdv1.DispatchResponse, error) {
	id, err := s.disp.Dispatch(ctx, orDefault(req.GetAgent(), "dev"), req.GetBody())
	if err != nil {
		return nil, err
	}
	return &orchdv1.DispatchResponse{InstanceId: id}, nil
}

// Status reports orchd's own liveness plus one entry per supervised subsystem.
func (s *server) Status(_ context.Context, _ *orchdv1.StatusRequest) (*orchdv1.StatusResponse, error) {
	resp := &orchdv1.StatusResponse{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
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
	workflows.SetStore(queue.NewSQLTaskStore(store))
	disp := queue.NewDispatcher(b)

	sup := supervisor.New(ctx)
	for _, u := range subsystems(deps{settings: s, apiKey: apiKey, rag: svc, backend: b, store: store}) {
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

	gs := grpc.NewServer()
	orchdv1.RegisterOrchdServer(gs, &server{start: time.Now(), rag: svc, disp: disp, sup: sup})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()

	log.Printf("orchd %s listening on %s (subsystems: %s)",
		version.Version, sock, strings.Join(sup.Names(), ", "))
	return gs.Serve(lis)
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
	return rag.NewService(emb, qs, s.Embedder.ChunkSize, s.Embedder.ChunkOverlap), emb
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

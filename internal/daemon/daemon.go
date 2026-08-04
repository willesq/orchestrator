// Package daemon implements orchd: it opens the SQLite store, constructs the
// (lazily-loaded) embedder and RAG service, and serves the Orchd gRPC service
// over a Unix-domain socket. Run takes a context so shutdown is testable; signal
// handling lives in the thin cmd/orchd entrypoint.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/worker"
	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/embed"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/qdrant"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
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

type server struct {
	orchdv1.UnimplementedOrchdServer
	start time.Time
	rag   ragService
	disp  dispatcher
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

	// In binary mode, orchd owns the native (Docker-free) qdrant engine's lifecycle.
	qStop, qURL, err := startQdrant(ctx, s, apiKey)
	if err != nil {
		return err
	}
	defer qStop()

	svc, emb := buildRAG(s, qURL, apiKey)
	defer emb.Close()

	// Durable workflow engine: worker drains AgentTask instances; dispatcher enqueues.
	b, err := queue.NewBackend()
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	workflows.SetStore(queue.NewSQLTaskStore(store))
	w := worker.New(b, nil)
	if err := w.RegisterWorkflow(workflows.AgentTask); err != nil {
		return err
	}
	if err := w.RegisterActivity(workflows.ProcessActivity); err != nil {
		return err
	}
	if err := w.Start(ctx); err != nil {
		return err
	}
	disp := queue.NewDispatcher(b)

	// Source freshness scheduler: refresh registered web/repo/owner sources on cadence.
	scanInterval, _ := time.ParseDuration(s.Sources.ScanInterval)
	scheduler := sources.NewScheduler(
		sources.NewRegistry(store), svc,
		sources.RefreshOpts{KnownRoots: expandRoots(s.Sources.KnownRoots)},
		scanInterval,
	)
	go scheduler.Run(ctx)

	// PR babysitter: poll open PRs via gh and cache state + events on transitions.
	if s.PRBabysitter.Enabled {
		pollInterval, _ := time.ParseDuration(s.PRBabysitter.PollInterval)
		go runBabysitter(ctx, prs.New(paths.PRStateFile(), paths.EventsDir()), pollInterval)
	}

	sock := paths.SocketPath()
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(sock) }()

	gs := grpc.NewServer()
	orchdv1.RegisterOrchdServer(gs, &server{start: time.Now(), rag: svc, disp: disp})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()

	log.Printf("orchd %s listening on %s", version.Version, sock)
	return gs.Serve(lis)
}

// startQdrant brings up the vector store per config. In binary mode orchd runs
// and owns the native qdrant engine; otherwise it just dials the configured URL.
// Returns a stop func and the effective base URL the RAG store should use.
func startQdrant(ctx context.Context, s config.Settings, apiKey string) (func(), string, error) {
	if s.RAG.Qdrant.Mode != "binary" {
		return func() {}, s.RAG.Qdrant.URL, nil
	}
	qp := qdrant.New(qdrant.Config{
		Version:  s.RAG.Qdrant.Version,
		BinDir:   filepath.Join(paths.DataDir(), "bin"),
		Storage:  resolveStorageDir(s),
		HTTPPort: s.RAG.Qdrant.HTTPPort,
		GRPCPort: s.RAG.Qdrant.GRPCPort,
		APIKey:   apiKey,
	})
	if err := qp.Start(ctx); err != nil {
		return nil, "", fmt.Errorf("start qdrant: %w", err)
	}
	return func() { _ = qp.Stop() }, fmt.Sprintf("http://127.0.0.1:%d", s.RAG.Qdrant.HTTPPort), nil
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

// resolveStorageDir is the qdrant storage directory (config override or the
// default XDG data location, <data>/qdrant).
func resolveStorageDir(s config.Settings) string {
	if s.RAG.Qdrant.Storage != "" {
		return s.RAG.Qdrant.Storage
	}
	return filepath.Join(paths.DataDir(), "qdrant")
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

// runBabysitter sweeps PRs on a cadence until ctx is canceled. gh failures (e.g.
// not authenticated) are non-fatal — the cache simply stays as-is.
func runBabysitter(ctx context.Context, bb *prs.Babysitter, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	_ = bb.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = bb.Sweep(ctx)
		}
	}
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

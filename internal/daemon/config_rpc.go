package daemon

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// configService serves orch's settings from orch.db. The full server and the
// recovery server share it, so settings can be fixed even when one of them
// stopped orchd's services from starting.
type configService struct {
	db *sql.DB
	// start is the config orchd started with, to say what waits for a restart.
	start map[string]string
	// startupError is why orchd runs only this service; empty when it started.
	startupError string
	// legacyFile is the config.yaml older releases used.
	legacyFile string
	// mu keeps two writes from interleaving their read and write.
	mu sync.Mutex
}

func newConfigService(db *sql.DB, start map[string]string) *configService {
	return &configService{db: db, start: start, legacyFile: paths.LegacyConfigFile()}
}

func (s *server) GetConfig(ctx context.Context, _ *orchdv1.GetConfigRequest) (*orchdv1.ConfigState, error) {
	return s.config.state(ctx)
}

func (s *server) SetConfig(ctx context.Context, req *orchdv1.SetConfigRequest) (*orchdv1.ConfigState, error) {
	return s.config.set(ctx, req)
}

func (s *server) RemoveLegacyConfig(ctx context.Context, _ *orchdv1.RemoveLegacyConfigRequest) (*orchdv1.ConfigState, error) {
	return s.config.removeLegacy(ctx)
}

func (c *configService) set(ctx context.Context, req *orchdv1.SetConfigRequest) (*orchdv1.ConfigState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(req.GetValues()) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "no settings to change")
	}
	values := make(map[string]any, len(req.GetValues()))
	for key, raw := range req.GetValues() {
		f, ok := config.FieldFor(key)
		if !ok || f.Hidden {
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "unknown setting %q", key)
		}
		val, err := config.ParseJSON(f, raw)
		if err != nil {
			return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
		}
		values[key] = val
	}
	if err := config.ApplyStore(ctx, c.db, values, req.GetConfirmSensitive(), "app"); err != nil {
		code := codes.InvalidArgument
		if errors.Is(err, config.ErrConfirmSensitive) {
			code = codes.FailedPrecondition
		}
		return nil, grpcstatus.Error(code, err.Error())
	}
	// Names only: a value can be a path or a permission rule, and logs are
	// shown in the app.
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	log.Printf("orchd: config changed by the operator: %s", strings.Join(keys, ", "))
	return c.state(ctx)
}

// removeLegacy moves the old config.yaml out of the way, into orch's backups:
// its comments and any keys the import could not keep live only there. The
// path is fixed, never taken from the request, and only once the import ran.
func (c *configService) removeLegacy(ctx context.Context) (*orchdv1.ConfigState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	imported, err := config.LegacyImported(ctx, c.db)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "read config: %v", err)
	}
	if !imported {
		return nil, grpcstatus.Error(codes.FailedPrecondition,
			"its settings have not been imported yet; restart orchd, then remove it")
	}
	backup, err := backUpAndRemove(c.legacyFile, filepath.Join(paths.DataDir(), "backups"))
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "remove config.yaml: %v", err)
	}
	if backup != "" {
		log.Printf("orchd: removed the legacy config.yaml at the operator's request; a copy is at %s", backup)
	}
	return c.state(ctx)
}

// backUpAndRemove copies path into dir, owner-only, then removes it, returning
// the copy's path; a path already gone is no error. It copies rather than
// renames because the config and data directories may be on different volumes.
// A symlink is removed as a link: its target is neither copied nor touched.
func backUpAndRemove(path, dir string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", os.Remove(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	backup := filepath.Join(dir, "config.yaml."+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return "", err
	}
	return backup, os.Remove(path)
}

func (c *configService) state(ctx context.Context) (*orchdv1.ConfigState, error) {
	v, err := config.LoadStore(ctx, c.db)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "read config: %v", err)
	}
	entries, err := config.Entries(v, config.Defaults())
	if err != nil {
		return nil, err
	}
	now, err := config.Snapshot(v)
	if err != nil {
		return nil, err
	}
	pending := config.PendingRestart(c.start, now)
	out := &orchdv1.ConfigState{StartupError: c.startupError}
	if _, err := os.Stat(c.legacyFile); err == nil {
		out.LegacyFile = c.legacyFile
	}
	for _, e := range entries {
		waits := slices.Contains(pending, e.Key)
		out.RestartRequired = out.RestartRequired || waits
		out.Fields = append(out.Fields, &orchdv1.ConfigField{
			Key: e.Key, Section: e.Section, Label: e.Label, Help: e.Help, Type: string(e.Type),
			Value: e.Value, DefaultValue: e.Default, Options: e.Options,
			HasRange: e.HasRange, Min: e.Min, Max: e.Max,
			Sensitive: e.Sensitive, Live: e.Live, PendingRestart: waits, Overridden: e.Overridden(),
		})
	}
	return out, nil
}

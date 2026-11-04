package daemon

import (
	"context"
	"errors"
	"log"
	"slices"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/spf13/viper"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func (s *server) GetConfig(context.Context, *orchdv1.GetConfigRequest) (*orchdv1.ConfigState, error) {
	v, err := config.Load()
	if err != nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "read config: %v", err)
	}
	return s.configState(v)
}

func (s *server) SetConfig(_ context.Context, req *orchdv1.SetConfigRequest) (*orchdv1.ConfigState, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
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
	// Load fresh: orch config set may have written since the last call, and a
	// file that no longer parses must be reported, not overwritten.
	v, err := config.Load()
	if err != nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "read config: %v", err)
	}
	if err := config.Apply(v, values, req.GetConfirmSensitive()); err != nil {
		code := codes.InvalidArgument
		if errors.Is(err, config.ErrConfirmSensitive) || errors.Is(err, config.ErrEnvOverride) {
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
	return s.configState(v)
}

func (s *server) configState(v *viper.Viper) (*orchdv1.ConfigState, error) {
	entries, err := config.Entries(v, config.Defaults())
	if err != nil {
		return nil, err
	}
	now, err := config.Snapshot(v)
	if err != nil {
		return nil, err
	}
	pending := config.PendingRestart(s.configStart, now)
	out := &orchdv1.ConfigState{Path: paths.ConfigFile()}
	for _, e := range entries {
		waits := slices.Contains(pending, e.Key)
		out.RestartRequired = out.RestartRequired || waits
		out.Fields = append(out.Fields, &orchdv1.ConfigField{
			Key: e.Key, Section: e.Section, Label: e.Label, Help: e.Help, Type: string(e.Type),
			Value: e.Value, DefaultValue: e.Default, Options: e.Options,
			HasRange: e.HasRange, Min: e.Min, Max: e.Max,
			Sensitive: e.Sensitive, Live: e.Live, EnvOverride: e.EnvOverride, PendingRestart: waits,
		})
	}
	return out, nil
}

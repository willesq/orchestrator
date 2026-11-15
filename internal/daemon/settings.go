package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/settings/claudecode"
	"github.com/willesq/orchestrator/internal/settings/instructions"
	"github.com/willesq/orchestrator/internal/settings/safetynetrules"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// UnitSettings watches the settings files orch manages.
const UnitSettings = "settings-watcher"

// settingsPollInterval is how often the watcher looks at the managed files.
// A look is a few small reads and a hash, so the cadence is about how soon
// the app shows an edit made elsewhere.
var settingsPollInterval = 2 * time.Second

// settingsFor builds the settings reconciler for the user's home: Claude
// Code's settings.json, cc-safety-net's rules and orch's instruction
// modules. nil when the home cannot be resolved.
func settingsFor(store *sql.DB) *settings.Service {
	home, err := os.UserHomeDir()
	if err != nil || store == nil {
		return nil
	}
	reg, err := settings.NewRegistry(claudecode.New(home), safetynetrules.New(safetynet.Locate(home)),
		instructions.New(home))
	if err != nil {
		return nil
	}
	return settings.NewService(settings.NewStore(store), reg,
		install.BackupFunc(filepath.Join(paths.DataDir(), "backups")))
}

// settingsUnit imports changes made to the managed files outside orch, so
// the app reflects them without a restart. It writes only to put an
// authoritative provider's files (instruction modules) back to the stored
// state.
func settingsUnit(svc *settings.Service) supervisor.Unit {
	return supervisor.Unit{
		Name:     UnitSettings,
		Detail:   "imports edits to Claude Code and cc-safety-net settings; enforces instruction modules",
		Interval: settingsPollInterval,
		Tick:     svc.Sync,
	}
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_ListSettings_FullMethodName,
	spaceDecision{spaceRecords, "managed harness settings (RC-1250)"}))

func (s *server) ListSettings(ctx context.Context, req *orchdv1.ListSettingsRequest) (*orchdv1.ListSettingsResponse, error) {
	if s.settings == nil {
		return nil, settingsUnavailable
	}
	l, err := s.settings.List(ctx, req.GetProvider(), req.GetKind())
	if err != nil {
		return nil, settingsError(err)
	}
	out := &orchdv1.ListSettingsResponse{Revision: l.Revision}
	reg := s.settings.Registry()
	for _, p := range reg.Providers() {
		if req.GetProvider() != "" && p.Name() != req.GetProvider() {
			continue
		}
		st := l.Providers[p.Name()]
		pb := &orchdv1.SettingsProvider{
			Name: p.Name(), Title: p.Title(), Files: st.Files, Error: st.Error, ObservedAt: unixOrZero(st.ObservedAt),
		}
		for _, k := range p.Kinds() {
			pb.Kinds = append(pb.Kinds, settingKindPB(k))
		}
		out.Providers = append(out.Providers, pb)
	}
	for _, r := range l.Rows {
		if p := reg.Get(r.Provider); p != nil {
			out.Items = append(out.Items, settingItemPB(p, r))
		}
	}
	return out, nil
}

// Reason: a hook is a command Claude Code runs and a permission rule
// widens what agents may do, so changing either is the operator's call.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_SetSettingEnabled_FullMethodName, "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	spaceDecision{spaceNone, "changes a harness setting"}))

func (s *server) SetSettingEnabled(ctx context.Context, req *orchdv1.SetSettingEnabledRequest) (*orchdv1.SettingItem, error) {
	if s.settings == nil {
		return nil, settingsUnavailable
	}
	if err := refuseToolHookSetting(req.GetProvider(), req.GetKind(), req.GetIdentity()); err != nil {
		return nil, err
	}
	r, err := s.settings.SetEnabled(ctx, req.GetProvider(), req.GetKind(), req.GetIdentity(), req.GetEnabled())
	if err != nil {
		return nil, settingsError(err)
	}
	return settingItemPB(s.settings.Registry().Get(r.Provider), r), nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_AddSetting_FullMethodName, "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	spaceDecision{spaceNone, "adds a harness setting"}))

func (s *server) AddSetting(ctx context.Context, req *orchdv1.AddSettingRequest) (*orchdv1.SettingItem, error) {
	if s.settings == nil {
		return nil, settingsUnavailable
	}
	r, err := s.settings.Add(ctx, req.GetProvider(), req.GetKind(), req.GetFields())
	if err != nil {
		return nil, settingsError(err)
	}
	return settingItemPB(s.settings.Registry().Get(r.Provider), r), nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_RemoveSetting_FullMethodName, "change Claude Code, cc-safety-net or instruction, subagent and skill settings",
	spaceDecision{spaceNone, "removes a harness setting"}))

func (s *server) RemoveSetting(ctx context.Context, req *orchdv1.RemoveSettingRequest) (*orchdv1.RemoveSettingResponse, error) {
	if s.settings == nil {
		return nil, settingsUnavailable
	}
	if err := s.settings.Remove(ctx, req.GetProvider(), req.GetKind(), req.GetIdentity()); err != nil {
		return nil, settingsError(err)
	}
	return &orchdv1.RemoveSettingResponse{}, nil
}

var settingsUnavailable = grpcstatus.Error(codes.Unavailable, "settings management unavailable")

// settingsError maps the reconciler's errors to RPC codes, keeping its
// message, which says what to do.
func settingsError(err error) error {
	code := codes.Internal
	switch {
	case errors.Is(err, settings.ErrNotFound):
		code = codes.NotFound
	case errors.Is(err, settings.ErrInvalid):
		code = codes.InvalidArgument
	case errors.Is(err, settings.ErrNotAllowed), errors.Is(err, settings.ErrUnreadable):
		code = codes.FailedPrecondition
	case errors.Is(err, settings.ErrAlreadyExists):
		code = codes.AlreadyExists
	case errors.Is(err, settings.ErrConflict):
		code = codes.Aborted
	}
	return grpcstatus.Error(code, err.Error())
}

func settingKindPB(k settings.KindSpec) *orchdv1.SettingKind {
	pb := &orchdv1.SettingKind{
		Name: k.Name, Title: k.Title, GroupTitle: k.GroupTitle,
		Addable: k.Addable, Removable: k.Removable, StateInFile: k.StateInFile,
	}
	for _, f := range k.Fields {
		pb.Fields = append(pb.Fields, &orchdv1.SettingField{
			Name: f.Name, Label: f.Label, Type: f.Type, Required: f.Required,
			Options: f.Options, Help: f.Help, Placeholder: f.Placeholder,
		})
	}
	return pb
}

func settingItemPB(p settings.Provider, r settings.Row) *orchdv1.SettingItem {
	pb := &orchdv1.SettingItem{
		Provider: r.Provider, Kind: r.Kind, Identity: r.Identity, Enabled: r.Enabled,
		Origin: string(r.Origin), Status: string(r.Status()), PayloadJson: string(r.Payload),
		CreatedAt: unixOrZero(r.CreatedAt), UpdatedAt: unixOrZero(r.UpdatedAt),
	}
	if p != nil {
		pb.Group, pb.Summary = p.Describe(r.Kind, r.Payload)
	}
	return pb
}

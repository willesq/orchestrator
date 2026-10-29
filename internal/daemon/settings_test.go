package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/settings/claudecode"
	"github.com/willesq/orchestrator/internal/settings/safetynetrules"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// settingsServer serves the settings RPCs over a temporary home and store.
func settingsServer(t *testing.T) (*server, string) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	store, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/orch-hook stop"}]}]},
		"permissions":{"allow":["WebFetch"]},"enabledPlugins":{"p@m":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := settings.NewRegistry(claudecode.New(home), safetynetrules.New(safetynet.Locate(home)))
	if err != nil {
		t.Fatal(err)
	}
	return &server{settings: settings.NewService(settings.NewStore(store), reg, install.BackupFunc(t.TempDir()))}, path
}

func TestSettingsRPCs(t *testing.T) {
	s, path := settingsServer(t)
	ctx := context.Background()
	l, err := s.ListSettings(ctx, &orchdv1.ListSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.GetProviders()) != 2 || l.GetRevision() == 0 {
		t.Fatalf("providers = %v", l.GetProviders())
	}
	cc := l.GetProviders()[0]
	if cc.GetName() != "claude-code" || len(cc.GetKinds()) != 3 || len(cc.GetKinds()[0].GetFields()) == 0 ||
		cc.GetObservedAt() == 0 || cc.GetFiles()[0] != path {
		t.Errorf("claude-code provider = %v", cc)
	}
	byID := map[string]*orchdv1.SettingItem{}
	for _, it := range l.GetItems() {
		byID[it.GetIdentity()] = it
	}
	stop := byID["Stop/orch-hook stop"]
	if stop.GetOrigin() != "orch" || stop.GetStatus() != "active" || stop.GetGroup() != "Stop" ||
		stop.GetSummary() != "/x/orch-hook stop" || !strings.Contains(stop.GetPayloadJson(), "orch-hook stop") {
		t.Errorf("orch hook = %v", stop)
	}

	off, err := s.SetSettingEnabled(ctx, &orchdv1.SetSettingEnabledRequest{Provider: "claude-code", Kind: "hook", Identity: "Stop/orch-hook stop"})
	if err != nil || off.GetStatus() != "disabled" {
		t.Fatalf("disable = %v %v", off, err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "orch-hook stop") {
		t.Error("hook still in settings.json")
	}
	added, err := s.AddSetting(ctx, &orchdv1.AddSettingRequest{Provider: "claude-code", Kind: "permission",
		Fields: map[string]string{"list": "deny", "rule": "Bash(rm -rf *)"}})
	if err != nil || added.GetOrigin() != "you" || added.GetSummary() != "Bash(rm -rf *)" {
		t.Fatalf("add = %v %v", added, err)
	}
	if _, err := s.RemoveSetting(ctx, &orchdv1.RemoveSettingRequest{Provider: "claude-code", Kind: "permission", Identity: added.GetIdentity()}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListSettings(ctx, &orchdv1.ListSettingsRequest{Provider: "cc-safety-net"}); len(l.GetProviders()) != 1 {
		t.Errorf("filtered providers = %v", l.GetProviders())
	}

	for name, tt := range map[string]struct {
		err  error
		want codes.Code
	}{
		"unknown provider": {callErr(s.ListSettings(ctx, &orchdv1.ListSettingsRequest{Provider: "nope"})), codes.NotFound},
		"bad rule":         {callErr(s.AddSetting(ctx, &orchdv1.AddSettingRequest{Provider: "claude-code", Kind: "permission", Fields: map[string]string{"list": "allow", "rule": "rm -rf"}})), codes.InvalidArgument},
		"orch removal":     {callErr(s.RemoveSetting(ctx, &orchdv1.RemoveSettingRequest{Provider: "claude-code", Kind: "hook", Identity: "Stop/orch-hook stop"})), codes.FailedPrecondition},
		"duplicate":        {callErr(s.AddSetting(ctx, &orchdv1.AddSettingRequest{Provider: "claude-code", Kind: "permission", Fields: map[string]string{"list": "allow", "rule": "WebFetch"}})), codes.AlreadyExists},
		"unknown item":     {callErr(s.SetSettingEnabled(ctx, &orchdv1.SetSettingEnabledRequest{Provider: "claude-code", Kind: "hook", Identity: "x"})), codes.NotFound},
	} {
		if got := code(tt.err); got != tt.want {
			t.Errorf("%s: code %v (%v), want %v", name, got, tt.err, tt.want)
		}
	}
}

func callErr[T any](_ T, err error) error { return err }

func TestSettingsUnavailable(t *testing.T) {
	s := &server{}
	ctx := context.Background()
	for _, err := range []error{
		callErr(s.ListSettings(ctx, &orchdv1.ListSettingsRequest{})),
		callErr(s.SetSettingEnabled(ctx, &orchdv1.SetSettingEnabledRequest{})),
		callErr(s.AddSetting(ctx, &orchdv1.AddSettingRequest{})),
		callErr(s.RemoveSetting(ctx, &orchdv1.RemoveSettingRequest{})),
	} {
		if code(err) != codes.Unavailable {
			t.Errorf("err = %v, want Unavailable", err)
		}
	}
}

func TestSettingsErrorCodes(t *testing.T) {
	for err, want := range map[error]codes.Code{
		fmt.Errorf("x: %w", settings.ErrConflict):   codes.Aborted,
		fmt.Errorf("x: %w", settings.ErrUnreadable): codes.FailedPrecondition,
		errors.New("disk"):                          codes.Internal,
	} {
		if got := code(settingsError(err)); got != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
}

func TestSettingsForAndUnit(t *testing.T) {
	if settingsFor(nil) != nil {
		t.Error("a nil store built a service")
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	t.Setenv("HOME", t.TempDir())
	store, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := settingsFor(store)
	if svc == nil || len(svc.Registry().Providers()) != 3 {
		t.Fatalf("service = %v", svc)
	}
	u := settingsUnit(svc)
	if u.Name != UnitSettings || u.Interval <= 0 || u.Tick == nil {
		t.Errorf("unit = %+v", u)
	}
	u.Tick(context.Background())
	if units := subsystems(deps{settingsSvc: svc}); !hasUnit(units, UnitSettings) {
		t.Error("settings watcher not supervised")
	}
}

func hasUnit(units []supervisor.Unit, name string) bool {
	for _, u := range units {
		if u.Name == name {
			return true
		}
	}
	return false
}

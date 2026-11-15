package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func configServer(t *testing.T) *server {
	t.Helper()
	return &server{config: configSvc(t)}
}

// configSvc serves settings from a temporary orch.db, started on the defaults.
func configSvc(t *testing.T) *configService {
	t.Helper()
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	store, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started, err := config.Snapshot(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	return newConfigService(store, started)
}

func configField(t *testing.T, st *orchdv1.ConfigState, key string) *orchdv1.ConfigField {
	t.Helper()
	for _, f := range st.GetFields() {
		if f.GetKey() == key {
			return f
		}
	}
	t.Fatalf("no field %s", key)
	return nil
}

func TestGetConfig(t *testing.T) {
	s := configServer(t)
	st, err := s.GetConfig(context.Background(), &orchdv1.GetConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetRestartRequired() || st.GetLegacyFile() != "" || st.GetStartupError() != "" {
		t.Errorf("fresh state = %+v", st)
	}
	got := configField(t, st, "queue.worker.max_concurrent")
	if got.GetValue() != "1" || got.GetDefaultValue() != "1" || got.GetMax() != 50 || !got.GetHasRange() {
		t.Errorf("max_concurrent = %+v", got)
	}
	if !configField(t, st, "queue.worker.permission_mode").GetSensitive() {
		t.Error("permission_mode is not sensitive")
	}
	if !configField(t, st, "hooks.auto_rag.top_k").GetLive() {
		t.Error("top_k is not live")
	}
}

func TestSetConfig(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		values  map[string]string
		confirm bool
		want    codes.Code
	}{
		{name: "nothing", want: codes.InvalidArgument},
		{name: "unknown key", values: map[string]string{"nope": "1"}, want: codes.InvalidArgument},
		{name: "hidden key", values: map[string]string{"queue.worker.enabled": "false"}, want: codes.InvalidArgument},
		{name: "past the hard limit", values: map[string]string{"queue.worker.max_concurrent": "51"}, want: codes.InvalidArgument},
		{name: "queue past the hard limit", values: map[string]string{"queue.max_active": "51"}, want: codes.InvalidArgument},
		{name: "not JSON", values: map[string]string{"queue.worker.max_concurrent": "five"}, want: codes.InvalidArgument},
		{name: "sensitive unconfirmed", values: map[string]string{"queue.worker.permission_mode": `"plan"`}, want: codes.FailedPrecondition},
		{name: "refused even when confirmed", values: map[string]string{"queue.worker.permission_mode": `"bypassPermissions"`}, confirm: true, want: codes.InvalidArgument},
		{name: "sensitive confirmed", values: map[string]string{"queue.worker.permission_mode": `"plan"`}, confirm: true, want: codes.OK},
		{name: "ordinary", values: map[string]string{"queue.worker.max_concurrent": "5"}, want: codes.OK},
		{name: "off", values: map[string]string{"queue.worker.max_concurrent": "0"}, want: codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := configServer(t)
			_, err := s.SetConfig(ctx, &orchdv1.SetConfigRequest{Values: tt.values, ConfirmSensitive: tt.confirm})
			if got := grpcstatus.Code(err); got != tt.want {
				t.Errorf("code = %v (%v), want %v", got, err, tt.want)
			}
		})
	}

	t.Run("a change persists and asks for a restart", func(t *testing.T) {
		s := configServer(t)
		st, err := s.SetConfig(ctx, &orchdv1.SetConfigRequest{Values: map[string]string{"queue.worker.max_concurrent": "30"}})
		if err != nil {
			t.Fatal(err)
		}
		if !st.GetRestartRequired() {
			t.Error("no restart asked for")
		}
		parallel := configField(t, st, "queue.worker.max_concurrent")
		if parallel.GetValue() != "30" || !parallel.GetPendingRestart() {
			t.Errorf("parallel = %+v", parallel)
		}
		if got := configField(t, st, "queue.max_active").GetValue(); got != "30" {
			t.Errorf("queue limit = %s, want it raised to 30", got)
		}
		again, err := s.GetConfig(ctx, &orchdv1.GetConfigRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if got := configField(t, again, "queue.worker.max_concurrent").GetValue(); got != "30" {
			t.Errorf("after reread = %s", got)
		}
	})
	t.Run("a live setting needs no restart", func(t *testing.T) {
		s := configServer(t)
		st, err := s.SetConfig(ctx, &orchdv1.SetConfigRequest{Values: map[string]string{"hooks.auto_rag.top_k": "7"}})
		if err != nil {
			t.Fatal(err)
		}
		if st.GetRestartRequired() || configField(t, st, "hooks.auto_rag.top_k").GetValue() != "7" {
			t.Errorf("state = %+v", configField(t, st, "hooks.auto_rag.top_k"))
		}
	})
	t.Run("a change is overridden until set back to its default", func(t *testing.T) {
		s := configServer(t)
		st, err := s.SetConfig(ctx, &orchdv1.SetConfigRequest{Values: map[string]string{"hooks.auto_rag.top_k": "7"}})
		if err != nil {
			t.Fatal(err)
		}
		if !configField(t, st, "hooks.auto_rag.top_k").GetOverridden() || configField(t, st, "hooks.auto_rag.max_chars").GetOverridden() {
			t.Error("overridden not reported per field")
		}
		st, err = s.SetConfig(ctx, &orchdv1.SetConfigRequest{Values: map[string]string{"hooks.auto_rag.top_k": "3"}})
		if err != nil {
			t.Fatal(err)
		}
		if configField(t, st, "hooks.auto_rag.top_k").GetOverridden() {
			t.Error("still overridden after setting the default back")
		}
	})
}

func TestRemoveLegacyConfig(t *testing.T) {
	ctx := context.Background()
	s := configServer(t)
	legacy := paths.LegacyConfigFile()
	if err := os.WriteFile(legacy, []byte("hooks:\n  auto_rag:\n    top_k: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := s.GetConfig(ctx, &orchdv1.GetConfigRequest{})
	if err != nil || st.GetLegacyFile() != legacy {
		t.Fatalf("legacy file = %q, %v", st.GetLegacyFile(), err)
	}
	// Not imported yet: removing it would lose its settings.
	if _, err := s.RemoveLegacyConfig(ctx, &orchdv1.RemoveLegacyConfigRequest{}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("remove before import: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("removed before import: %v", err)
	}
	importLegacyConfig(ctx, s.config.db)
	st, err = s.RemoveLegacyConfig(ctx, &orchdv1.RemoveLegacyConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetLegacyFile() != "" {
		t.Errorf("legacy file still reported: %q", st.GetLegacyFile())
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still there: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(paths.DataDir(), "backups", "config.yaml.*"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one copy", backups)
	}
	if data, _ := os.ReadFile(backups[0]); !strings.Contains(string(data), "top_k: 9") {
		t.Errorf("backup = %q", data)
	}
	if got := configField(t, st, "hooks.auto_rag.top_k").GetValue(); got != "9" {
		t.Errorf("imported top_k = %s, want 9", got)
	}
	// Already gone is fine.
	if _, err := s.RemoveLegacyConfig(ctx, &orchdv1.RemoveLegacyConfigRequest{}); err != nil {
		t.Errorf("second remove: %v", err)
	}
}

func TestBackUpAndRemoveLeavesASymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	backup, err := backUpAndRemove(link, filepath.Join(dir, "backups"))
	if err != nil || backup != "" {
		t.Fatalf("backup = %q, %v", backup, err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("link still there: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Errorf("target = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a symlink's target was copied into backups")
	}
}

func TestRecoveryExitsToRetry(t *testing.T) {
	svc := configSvc(t)
	sockDir, err := os.MkdirTemp("", "orchd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Setenv("ORCH_SOCKET", filepath.Join(sockDir, "r.sock"))
	orig := recoveryRetry
	recoveryRetry = 50 * time.Millisecond
	t.Cleanup(func() { recoveryRetry = orig })
	done := make(chan error, 1)
	go func() { done <- serveRecovery(context.Background(), svc, errors.New("no network"), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveRecovery = %v, want a clean exit to retry", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovery never exited to retry the full start")
	}
	if svc.startupError != "no network" {
		t.Errorf("startup error = %q", svc.startupError)
	}
}

func TestImportLegacyConfigLogsAFailure(t *testing.T) {
	svc := configSvc(t)
	if err := os.WriteFile(paths.LegacyConfigFile(), []byte("hooks: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	importLegacyConfig(context.Background(), svc.db)
	if ok, err := config.LegacyImported(context.Background(), svc.db); err != nil || ok {
		t.Errorf("a file that failed to import was marked imported: %v, %v", ok, err)
	}
}

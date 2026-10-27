package claudecode

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/safetynet/fakeclaude"
	"github.com/willesq/orchestrator/internal/settings"
)

// TestInstallHonorsToggles is the end-to-end contract between the app and
// orch install: items turned off through the reconciler stay off across
// `orch install --apply` and are not reported as drift, and an unreadable
// store stops install rather than re-enabling them.
func TestInstallHonorsToggles(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("ORCH_DATA_DIR", data)
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	defer install.SetSafetyNetRunner(func(loc safetynet.Locations) safetynet.Runner {
		return fakeclaude.New(loc.ClaudeDir)
	})()
	home := t.TempDir()
	bins := install.Bins{Orch: "/opt/orch/orch", OrchHook: "/opt/orch/orch-hook", Orchd: "/opt/orch/orchd"}
	plan := install.BuildPlan(install.ClaudeCode{}, bins, home)
	backups := filepath.Join(data, "backups")
	if err := plan.Apply(io.Discard, backups); err != nil {
		t.Fatal(err)
	}

	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := New(home)
	reg, _ := settings.NewRegistry(p)
	svc := settings.NewService(settings.NewStore(conn), reg, install.BackupFunc(backups))
	ctx := context.Background()
	hook := install.HookIdentity("SessionStart", "session-start")
	deny := install.PermissionIdentity("deny", "Read(~/.netrc)")
	allow := install.PermissionIdentity("allow", "mcp__rag__*")
	for _, id := range []struct{ kind, id string }{{install.KindHook, hook}, {install.KindPermission, deny}, {install.KindPermission, allow}} {
		r, err := svc.SetEnabled(ctx, install.ProviderClaudeCode, id.kind, id.id, false)
		if err != nil {
			t.Fatalf("disable %s: %v", id.id, err)
		}
		if r.Origin != settings.OriginOrch {
			t.Errorf("%s origin = %s, want orch", id.id, r.Origin)
		}
	}
	read := func() string {
		b, err := os.ReadFile(p.Path())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := read(); strings.Contains(s, "orch-hook session-start") || strings.Contains(s, "Read(~/.netrc)") || strings.Contains(s, "mcp__rag__*") {
		t.Fatalf("disabled items still in settings.json:\n%s", s)
	}

	if n, err := plan.Pending(); err != nil || n != 0 {
		var out strings.Builder
		_ = plan.Print(&out)
		t.Fatalf("Pending = %d, %v; disabled items must not read as drift\n%s", n, err, out.String())
	}
	if err := plan.Apply(io.Discard, backups); err != nil {
		t.Fatal(err)
	}
	if s := read(); strings.Contains(s, "orch-hook session-start") || strings.Contains(s, "Read(~/.netrc)") {
		t.Fatalf("install put disabled items back:\n%s", s)
	}
	if !strings.Contains(read(), "orch-hook stop") || !strings.Contains(read(), "mcp__queue__*") {
		t.Error("install dropped items that were not turned off")
	}
	if n, err := install.DisabledCount(); err != nil || n != 3 {
		t.Errorf("DisabledCount = %d, %v", n, err)
	}
	var dry strings.Builder
	if err := plan.Print(&dry); err != nil || !strings.Contains(dry.String(), "UNCHANGED") {
		t.Errorf("dry run: %v\n%s", err, dry.String())
	}

	// Turned back on, the items return and install still has nothing to do.
	if _, err := svc.SetEnabled(ctx, install.ProviderClaudeCode, install.KindHook, hook, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), "orch-hook session-start") {
		t.Error("re-enabled hook not restored")
	}
	if n, err := plan.Pending(); err != nil || n != 0 {
		t.Errorf("Pending after re-enable = %d, %v", n, err)
	}

	// Fail safe: a store that cannot be read stops install.
	conn.Close()
	if err := os.WriteFile(filepath.Join(data, "orch.db"), []byte("garbage that is not a sqlite database......"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(data, "orch.db-wal"))
	_ = os.Remove(filepath.Join(data, "orch.db-shm"))
	if _, err := plan.Pending(); err == nil || !strings.Contains(err.Error(), "turned off in orch stay off") {
		t.Errorf("Pending with an unreadable store = %v, want a refusal", err)
	}
}

package daemon

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func configServer(t *testing.T) *server {
	t.Helper()
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	started, err := config.Snapshot(v)
	if err != nil {
		t.Fatal(err)
	}
	return &server{configStart: started}
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
	if st.GetRestartRequired() || st.GetPath() == "" {
		t.Errorf("fresh state = restart %v, path %q", st.GetRestartRequired(), st.GetPath())
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
	t.Run("an env override is refused", func(t *testing.T) {
		s := configServer(t)
		t.Setenv("ORCH_QUEUE_WORKER_TIMEOUT", "5m")
		_, err := s.SetConfig(ctx, &orchdv1.SetConfigRequest{Values: map[string]string{"queue.worker.timeout": `"7m"`}})
		if grpcstatus.Code(err) != codes.FailedPrecondition {
			t.Errorf("err = %v", err)
		}
	})
}

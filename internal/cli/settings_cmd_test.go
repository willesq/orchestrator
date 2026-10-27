package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// fakeSettings backs the settings RPCs of fakeClient.
var fakeSettings = struct {
	list *orchdv1.ListSettingsResponse
	set  []*orchdv1.SetSettingEnabledRequest
	err  error
}{list: &orchdv1.ListSettingsResponse{}}

func (f *fakeClient) ListSettings(_ context.Context, req *orchdv1.ListSettingsRequest, _ ...grpc.CallOption) (*orchdv1.ListSettingsResponse, error) {
	if fakeSettings.err != nil {
		return nil, fakeSettings.err
	}
	out := &orchdv1.ListSettingsResponse{Providers: fakeSettings.list.GetProviders()}
	for _, it := range fakeSettings.list.GetItems() {
		if req.GetKind() == "" || it.GetKind() == req.GetKind() {
			out.Items = append(out.Items, it)
		}
	}
	return out, nil
}

func (f *fakeClient) SetSettingEnabled(_ context.Context, req *orchdv1.SetSettingEnabledRequest, _ ...grpc.CallOption) (*orchdv1.SettingItem, error) {
	fakeSettings.set = append(fakeSettings.set, req)
	status := "disabled"
	if req.GetEnabled() {
		status = "active"
	}
	return &orchdv1.SettingItem{Provider: req.GetProvider(), Kind: req.GetKind(), Identity: req.GetIdentity(),
		Enabled: req.GetEnabled(), Status: status, Summary: "summary\x1b[2J"}, nil
}

func (f *fakeClient) AddSetting(context.Context, *orchdv1.AddSettingRequest, ...grpc.CallOption) (*orchdv1.SettingItem, error) {
	return &orchdv1.SettingItem{}, nil
}

func (f *fakeClient) RemoveSetting(context.Context, *orchdv1.RemoveSettingRequest, ...grpc.CallOption) (*orchdv1.RemoveSettingResponse, error) {
	return &orchdv1.RemoveSettingResponse{}, nil
}

func withFakeSettings(t *testing.T) {
	t.Helper()
	saved := fakeSettings
	t.Cleanup(func() { fakeSettings = saved })
	fakeSettings.err, fakeSettings.set = nil, nil
	fakeSettings.list = &orchdv1.ListSettingsResponse{
		Providers: []*orchdv1.SettingsProvider{
			{Name: "claude-code", Title: "Claude Code", Files: []string{"/h/.claude/settings.json"}, Kinds: []*orchdv1.SettingKind{
				{Name: "hook", Title: "Hooks", GroupTitle: "Event"},
				{Name: "permission", Title: "Permissions", GroupTitle: "List"},
				{Name: "plugin", Title: "Plugins", GroupTitle: "Marketplace"},
			}},
			{Name: "cc-safety-net", Title: "cc-safety-net", Error: "parse rule.json: bad", Kinds: []*orchdv1.SettingKind{
				{Name: "rule", Title: "Safety Net rules", GroupTitle: "Rulebook"},
			}},
			{Name: "other", Title: "Other", Kinds: []*orchdv1.SettingKind{{Name: "rule", Title: "Rules"}}},
		},
		Items: []*orchdv1.SettingItem{
			{Provider: "claude-code", Kind: "hook", Identity: "SessionStart/orch-hook session-start", Origin: "orch",
				Status: "active", Group: "SessionStart", Summary: "/bin/orch-hook session-start"},
			{Provider: "claude-code", Kind: "permission", Identity: "deny/Read(~/.netrc)", Origin: "orch",
				Status: "disabled", Group: "deny", Summary: "Read(~/.netrc)"},
			{Provider: "cc-safety-net", Kind: "rule", Identity: "mine/no-push", Origin: "found",
				Status: "removed", Group: "mine", Summary: "no-push: git push\x1b]0;x\x07 \u202eevil"},
			{Provider: "other", Kind: "rule", Identity: "mine/no-push", Origin: "found", Status: "missing"},
		},
	}
}

func TestSettingsList(t *testing.T) {
	withFakeSettings(t)
	out, err := run(factory(&fakeClient{}, nil), "", "settings", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Claude Code (/h/.claude/settings.json)", "  Hooks", "EVENT", "on     orch",
		"SessionStart/orch-hook session-start", "off    orch", "deny/Read(~/.netrc)",
		"error: parse rule.json: bad", "removed", "no-push: git push?]0;x?", "missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\u202e") || !strings.Contains(out, "?evil") || strings.Contains(out, "Plugins") {
		t.Errorf("control characters or an empty kind leaked:\n%q", out)
	}
	if out, _ = run(factory(&fakeClient{}, nil), "", "settings", "list", "--json"); !strings.Contains(out, `"identity":"deny/Read(~/.netrc)"`) {
		t.Errorf("json output:\n%s", out)
	}
	fakeSettings.list = &orchdv1.ListSettingsResponse{}
	if out, _ = run(factory(&fakeClient{}, nil), "", "settings", "list"); !strings.Contains(out, "no settings providers") {
		t.Errorf("empty output:\n%s", out)
	}
	fakeSettings.err = errors.New("orchd down")
	if _, err := run(factory(&fakeClient{}, nil), "", "settings", "list"); err == nil {
		t.Error("expected the RPC error")
	}
}

func TestSettingsToggle(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr string
		wantReq *orchdv1.SetSettingEnabledRequest
	}{
		{
			name: "disable finds the provider", args: []string{"disable", "hook", "SessionStart/orch-hook session-start"},
			want:    "off hook SessionStart/orch-hook session-start: summary?[2J",
			wantReq: &orchdv1.SetSettingEnabledRequest{Provider: "claude-code", Kind: "hook", Identity: "SessionStart/orch-hook session-start"},
		},
		{
			name: "enable with an explicit provider", args: []string{"enable", "rule", "mine/no-push", "--provider", "other"},
			want:    "on rule mine/no-push",
			wantReq: &orchdv1.SetSettingEnabledRequest{Provider: "other", Kind: "rule", Identity: "mine/no-push", Enabled: true},
		},
		{name: "ambiguous id", args: []string{"enable", "rule", "mine/no-push"}, wantErr: "pass --provider"},
		{name: "unknown id", args: []string{"enable", "hook", "nope"}, wantErr: `no hook "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeSettings(t)
			out, err := run(factory(&fakeClient{}, nil), "", append([]string{"settings"}, tt.args...)...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output %q missing %q", out, tt.want)
			}
			if len(fakeSettings.set) != 1 || fakeSettings.set[0].String() != tt.wantReq.String() {
				t.Errorf("requests = %v, want %v", fakeSettings.set, tt.wantReq)
			}
		})
	}
}

func TestSettingsToggleRefusedToTasks(t *testing.T) {
	withFakeSettings(t)
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	_, err := run(factory(&fakeClient{}, nil), "", "settings", "disable", "hook", "x")
	if err == nil || !strings.Contains(err.Error(), "queue tasks cannot change") {
		t.Fatalf("err = %v", err)
	}
	if len(fakeSettings.set) != 0 {
		t.Error("a task reached the RPC")
	}
	fakeSettings.err = errors.New("down")
	t.Setenv(agentrun.TaskIDEnv, "")
	if _, err := run(factory(&fakeClient{}, nil), "", "settings", "enable", "hook", "x"); err == nil {
		t.Error("expected the list error")
	}
}

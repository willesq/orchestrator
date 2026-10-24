package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func withFakeGitHub(t *testing.T) {
	t.Helper()
	saved := fakeGitHub
	t.Cleanup(func() { fakeGitHub = saved })
	fakeGitHub.err = nil
	fakeGitHub.set = nil
	fakeGitHub.identity = &orchdv1.GitHubIdentity{Login: "willesq", Teams: []*orchdv1.GitHubTeam{
		{Org: "WippData", Slug: "admins", Name: "Admins"},
	}}
	fakeGitHub.scopes = &orchdv1.ListGitHubScopesResponse{Scopes: []*orchdv1.GitHubScope{
		{Owner: "willesq", Kind: "user", Enabled: true, Items: "involving", RepoMode: "all"},
		{Owner: "WippData", Kind: "org", Items: "involving", RepoMode: "exclude", Repos: []string{"old"}},
	}}
	fakeGitHub.repos = &orchdv1.ListGitHubReposResponse{Repos: []*orchdv1.GitHubRepo{
		{Name: "api", Private: true}, {Name: "legacy", Archived: true}, {Name: "site"},
	}}
}

func TestGitHubScopesCommand(t *testing.T) {
	withFakeGitHub(t)
	out, err := run(factory(&fakeClient{}, nil), "", "github", "scopes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"on  willesq", "off WippData", "repos: all but old", "repos: all"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	fakeGitHub.scopes.Warning = "Could not reach GitHub: 401"
	if out, _ = run(factory(&fakeClient{}, nil), "", "github", "scopes"); !strings.Contains(out, "warning: Could not reach GitHub") {
		t.Errorf("warning not shown:\n%s", out)
	}
	if out, _ = run(factory(&fakeClient{}, nil), "", "github", "scopes", "--json"); !strings.Contains(out, `"owner":"WippData"`) {
		t.Errorf("json output:\n%s", out)
	}
	fakeGitHub.scopes = &orchdv1.ListGitHubScopesResponse{}
	if out, _ = run(factory(&fakeClient{}, nil), "", "github", "scopes"); !strings.Contains(out, "no scopes yet") {
		t.Errorf("empty output:\n%s", out)
	}
}

func TestGitHubEnableAndDisable(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantItems string
		wantMode  string
		wantRepos []string
		wantOn    bool
	}{
		{"enable defaults to involving and all repos", []string{"github", "enable", "wippdata"}, "involving", "all", nil, true},
		{"enable all items with include", []string{"github", "enable", "WippData", "--all-items", "--include", "api,site"}, "all", "include", []string{"api", "site"}, true},
		{"enable with exclude", []string{"github", "enable", "WippData", "--exclude", "legacy"}, "involving", "exclude", []string{"legacy"}, true},
		{"disable keeps the repo picks", []string{"github", "disable", "WippData"}, "involving", "exclude", []string{"old"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeGitHub(t)
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if len(fakeGitHub.set) != 1 {
				t.Fatalf("SetGitHubScope calls = %d", len(fakeGitHub.set))
			}
			s := fakeGitHub.set[0]
			if s.GetOwner() != "WippData" || s.GetEnabled() != tt.wantOn || s.GetItems() != tt.wantItems ||
				s.GetRepoMode() != tt.wantMode || !slices.Equal(s.GetRepos(), tt.wantRepos) {
				t.Errorf("sent %+v", s)
			}
		})
	}
}

func TestGitHubEnableErrors(t *testing.T) {
	withFakeGitHub(t)
	if _, err := run(factory(&fakeClient{}, nil), "", "github", "enable", "nobody"); err == nil || !strings.Contains(err.Error(), "not your account") {
		t.Errorf("unknown owner err = %v", err)
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "github", "enable", "WippData", "--include", "a", "--exclude", "b"); err == nil {
		t.Error("--include with --exclude accepted")
	}
	t.Setenv("ORCH_TASK_ID", "task-1")
	if _, err := run(factory(&fakeClient{}, nil), "", "github", "disable", "WippData"); err == nil || !strings.Contains(err.Error(), "queue tasks cannot change GitHub monitoring") {
		t.Errorf("inside a task err = %v", err)
	}
}

func TestGitHubTeamsAndRepos(t *testing.T) {
	withFakeGitHub(t)
	out, err := run(factory(&fakeClient{}, nil), "", "github", "teams")
	if err != nil || !strings.Contains(out, "WippData/admins  Admins") {
		t.Errorf("teams: %v\n%s", err, out)
	}
	fakeGitHub.identity.Teams = nil
	if out, _ = run(factory(&fakeClient{}, nil), "", "github", "teams"); !strings.Contains(out, "willesq is on no teams") {
		t.Errorf("no teams:\n%s", out)
	}
	out, err = run(factory(&fakeClient{}, nil), "", "github", "repos", "WippData")
	if err != nil || !strings.Contains(out, "api private") || !strings.Contains(out, "legacy archived") || !strings.Contains(out, "site \n") {
		t.Errorf("repos: %v\n%q", err, out)
	}
}

func TestGitHubCommandsReportRPCErrors(t *testing.T) {
	for _, args := range [][]string{
		{"github", "scopes"}, {"github", "enable", "x"}, {"github", "disable", "x"},
		{"github", "teams"}, {"github", "repos", "x"},
	} {
		withFakeGitHub(t)
		fakeGitHub.err = errors.New("orchd unavailable")
		if _, err := run(factory(&fakeClient{}, nil), "", args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	withFakeGitHub(t)
	if _, err := run(factory(nil, errors.New("dial failed")), "", "github", "scopes"); err == nil {
		t.Error("dial failure not reported")
	}
}

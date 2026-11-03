package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// ghDown makes every gh call fail, standing in for GitHub being unreachable.
var ghDown = errors.New("gh: could not resolve host")

func githubServer(t *testing.T, down *bool) *server {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		if *down {
			return nil, ghDown
		}
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "user/orgs"):
			return []byte("WippData\n"), nil
		case strings.Contains(joined, "user/teams"):
			return []byte(`{"org":"WippData","slug":"admins","name":"Admins"}`), nil
		case strings.Contains(joined, ".login"):
			return []byte("willesq\n"), nil
		case strings.Contains(joined, "orgs/WippData/repos"):
			return []byte(`{"name":"api","private":true,"archived":false}`), nil
		}
		return nil, errors.New("unexpected: " + joined)
	}
	return &server{github: github.NewService(github.NewStore(conn), gh), handoffDir: t.TempDir()}
}

func TestGitHubRPCsUnavailableWithoutService(t *testing.T) {
	s := &server{}
	ctx := context.Background()
	calls := []func() error{
		func() error { _, err := s.GetGitHubIdentity(ctx, &orchdv1.GetGitHubIdentityRequest{}); return err },
		func() error { _, err := s.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{}); return err },
		func() error { _, err := s.SetGitHubScope(ctx, &orchdv1.SetGitHubScopeRequest{}); return err },
		func() error { _, err := s.ListGitHubRepos(ctx, &orchdv1.ListGitHubReposRequest{}); return err },
	}
	for i, call := range calls {
		if code := grpcstatus.Code(call()); code != codes.Unavailable {
			t.Errorf("call %d code = %v", i, code)
		}
	}
}

func TestGitHubIdentityAndScopes(t *testing.T) {
	down := false
	s := githubServer(t, &down)
	ctx := context.Background()

	id, err := s.GetGitHubIdentity(ctx, &orchdv1.GetGitHubIdentityRequest{})
	if err != nil || id.GetLogin() != "willesq" || len(id.GetTeams()) != 1 || id.GetTeams()[0].GetSlug() != "admins" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	resp, err := s.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{})
	if err != nil || resp.GetWarning() != "" {
		t.Fatalf("scopes err %v warning %q", err, resp.GetWarning())
	}
	var got []string
	for _, sc := range resp.GetScopes() {
		got = append(got, sc.GetOwner()+":"+sc.GetKind()+":"+map[bool]string{true: "on", false: "off"}[sc.GetEnabled()])
	}
	if !slices.Equal(got, []string{"willesq:user:on", "WippData:org:off"}) {
		t.Errorf("scopes = %v", got)
	}

	// A refresh within 30 s is served from the cache rather than spending the
	// user's rate limit.
	down = true
	if _, err := s.GetGitHubIdentity(ctx, &orchdv1.GetGitHubIdentityRequest{Refresh: true}); err != nil {
		t.Errorf("an immediate refresh reached GitHub: %v", err)
	}
}

func TestGitHubDown(t *testing.T) {
	down := true
	s := githubServer(t, &down)
	ctx := context.Background()
	if _, err := s.GetGitHubIdentity(ctx, &orchdv1.GetGitHubIdentityRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("identity with GitHub down: %v", err)
	}
	resp, err := s.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{})
	if err != nil || len(resp.GetScopes()) != 0 || !strings.Contains(resp.GetWarning(), "Could not reach GitHub") {
		t.Errorf("scopes with GitHub down = %+v, %v", resp, err)
	}
}

func TestSetGitHubScope(t *testing.T) {
	down := false
	s := githubServer(t, &down)
	ctx := context.Background()
	if _, err := s.ListGitHubScopes(ctx, &orchdv1.ListGitHubScopesRequest{}); err != nil {
		t.Fatal(err)
	}

	resp, err := s.SetGitHubScope(ctx, &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{
		Owner: "wippdata", Kind: "user", Enabled: true, Items: "all", RepoMode: "include", Repos: []string{"api"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.handoffDir, "pr-sweep")); err != nil {
		t.Errorf("a scope change did not request a sweep: %v", err)
	}
	got := resp.GetScope()
	if got.GetOwner() != "WippData" || got.GetKind() != "org" || !got.GetEnabled() || got.GetItems() != "all" ||
		got.GetRepoMode() != "include" || !slices.Equal(got.GetRepos(), []string{"api"}) {
		t.Errorf("stored scope = %+v (kind must stay org, owner keep its case)", got)
	}

	// An org the user has left keeps its row but cannot be turned on.
	if _, err := s.github.Store.Set(ctx, github.Scope{Owner: "OldOrg", Kind: github.KindOrg,
		Items: github.ItemsInvolving, RepoMode: github.ReposAll}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		req  *orchdv1.SetGitHubScopeRequest
		code codes.Code
	}{
		{"an org the user left", &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{
			Owner: "OldOrg", Enabled: true, Items: "all", RepoMode: "all"}}, codes.FailedPrecondition},
		{"missing scope", &orchdv1.SetGitHubScopeRequest{}, codes.InvalidArgument},
		{"not a discovered account", &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{
			Owner: "someone-else", Items: "all", RepoMode: "all"}}, codes.NotFound},
		{"invalid settings", &orchdv1.SetGitHubScopeRequest{Scope: &orchdv1.GitHubScope{
			Owner: "WippData", Items: "everything", RepoMode: "all"}}, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.SetGitHubScope(ctx, tt.req); grpcstatus.Code(err) != tt.code {
				t.Errorf("code = %v (%v), want %v", grpcstatus.Code(err), err, tt.code)
			}
		})
	}
}

func TestListGitHubRepos(t *testing.T) {
	down := false
	s := githubServer(t, &down)
	ctx := context.Background()
	resp, err := s.ListGitHubRepos(ctx, &orchdv1.ListGitHubReposRequest{Owner: "WippData"})
	if err != nil || len(resp.GetRepos()) != 1 || resp.GetRepos()[0].GetName() != "api" || !resp.GetRepos()[0].GetPrivate() {
		t.Fatalf("repos = %+v, %v", resp, err)
	}
	if _, err := s.ListGitHubRepos(ctx, &orchdv1.ListGitHubReposRequest{Owner: "../x"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid owner: %v", err)
	}
	if _, err := s.ListGitHubRepos(ctx, &orchdv1.ListGitHubReposRequest{Owner: "microsoft"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("not a member: %v", err)
	}
	down = true
	if _, err := s.ListGitHubRepos(ctx, &orchdv1.ListGitHubReposRequest{Owner: "WippData"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("GitHub down: %v", err)
	}
}

func TestWatchFrom(t *testing.T) {
	ctx := context.Background()
	if _, err := watchFrom(nil)(ctx); err == nil {
		t.Error("no service: no error")
	}
	down := false
	s := githubServer(t, &down)
	w, err := watchFrom(s.github)(ctx)
	if err != nil || w.Login != "willesq" || len(w.Scopes) != 2 || !w.Scopes[0].Enabled {
		t.Errorf("watch = %+v, %v", w, err)
	}
	downAll := true
	if _, err := watchFrom(githubServer(t, &downAll).github)(ctx); err == nil {
		t.Error("GitHub down: no error")
	}
}

func TestRequestGitHubSweep(t *testing.T) {
	ctx := context.Background()
	t.Run("leaves one request however often it is asked", func(t *testing.T) {
		s := &server{handoffDir: t.TempDir()}
		for range 2 {
			if _, err := s.RequestGitHubSweep(ctx, &orchdv1.RequestGitHubSweepRequest{}); err != nil {
				t.Fatal(err)
			}
		}
		if !handoff.TakePRSweep(s.handoffDir) {
			t.Error("no sweep was requested")
		}
		if handoff.TakePRSweep(s.handoffDir) {
			t.Error("repeated calls left more than one request")
		}
	})
	t.Run("reports a request it could not leave", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s := &server{handoffDir: file}
		if _, err := s.RequestGitHubSweep(ctx, &orchdv1.RequestGitHubSweepRequest{}); grpcstatus.Code(err) != codes.Internal {
			t.Errorf("err = %v, want Internal", err)
		}
	})
}

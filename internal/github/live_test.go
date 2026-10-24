package github

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveGitHub reads the real account through gh. It only runs when
// ORCH_LIVE_GH=1, and only reads: identity, then the user's own repos.
func TestLiveGitHub(t *testing.T) {
	if os.Getenv("ORCH_LIVE_GH") != "1" {
		t.Skip("set ORCH_LIVE_GH=1 to read the real GitHub account through gh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	svc := NewService(testStore(t), GH)
	id, err := svc.Identity(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("login=%s orgs=%v teams=%d", id.Login, id.Orgs, len(id.Teams))
	scopes, ghErr, err := svc.Scopes(ctx, false)
	if err != nil || ghErr != nil || len(scopes) != 1+len(id.Orgs) || !scopes[0].Enabled || scopes[0].Owner != id.Login {
		t.Fatalf("scopes = %+v, ghErr %v, err %v", scopes, ghErr, err)
	}
	repos, err := svc.Repos(ctx, id.Login)
	if err != nil || len(repos) == 0 {
		t.Fatalf("own repos: %d, %v", len(repos), err)
	}
	t.Logf("own repos: %d (first %s)", len(repos), repos[0].Name)
	if len(id.Orgs) > 0 {
		orgRepos, err := svc.Repos(ctx, id.Orgs[0])
		if err != nil {
			t.Fatalf("%s repos: %v", id.Orgs[0], err)
		}
		t.Logf("%s repos: %d", id.Orgs[0], len(orgRepos))
	}
}

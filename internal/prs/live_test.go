package prs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/github"
)

// TestLiveWatch sweeps the real account through gh with default scopes (the
// user's own account only), into temporary files. It only runs when
// ORCH_LIVE_GH=1, and only reads GitHub.
func TestLiveWatch(t *testing.T) {
	if os.Getenv("ORCH_LIVE_GH") != "1" {
		t.Skip("set ORCH_LIVE_GH=1 to sweep the real GitHub account through gh")
	}
	dir := t.TempDir()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	svc := github.NewService(github.NewStore(conn), github.GH)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	watch := func(ctx context.Context) (Watch, error) {
		scopes, ghErr, err := svc.Scopes(ctx, false)
		if err == nil {
			err = ghErr
		}
		id, _ := svc.Identity(ctx, false)
		return Watch{Login: id.Login, Scopes: scopes}, err
	}
	b := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "events"), watch)
	if err := b.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	state := LoadState(filepath.Join(dir, "state.json"))
	var prCount, issues int
	reasons := map[string]int{}
	for _, item := range state.PRs {
		if item.IsPR() {
			prCount++
		} else {
			issues++
		}
		for _, r := range item.Reasons {
			reasons[r]++
		}
	}
	t.Logf("own account only: %d PRs, %d issues, reasons %v, needs you %d", prCount, issues, reasons, len(NeedsMe(state)))
}

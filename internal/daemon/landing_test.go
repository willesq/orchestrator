package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/landing"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

const landingSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// landingServer serves landings over a task that proposed landingSHA, with git
// faked: which commands ran is what these handlers are checked by.
func landingServer(t *testing.T) (*server, *[]string) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	home := t.TempDir()
	worktree := filepath.Join(home, "wt")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	tasks := &fakeTasks{found: true, one: queue.Task{
		Status: "complete", Body: "x\nPR: https://github.com/acme/widget/pull/7",
		Result: "done\nPROPOSED: " + worktree + " " + landingSHA,
	}}
	gh := github.NewService(github.NewStore(conn), func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "api user --jq .login" {
			return []byte("me\n"), nil
		}
		return nil, nil
	})
	svc := newLandingService(landing.NewStore(conn), tasks, gh, nil)
	svc.Home = home
	svc.GH = func(context.Context, ...string) ([]byte, error) {
		return json.Marshal(map[string]any{
			"state": "OPEN", "headRefOid": strings.Repeat("b", 40), "headRefName": "feat/x", "baseRefName": "main",
			"author": map[string]string{"login": "me"},
		})
	}
	var ran []string
	svc.Git = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		ran = append(ran, joined)
		switch {
		case args[0] == "init":
			return nil, os.MkdirAll(args[len(args)-1], 0o700)
		case args[0] == "version":
			return []byte("git version 2.56.0\n"), nil
		case strings.HasPrefix(joined, "rev-parse"):
			return []byte(landingSHA + "\n"), nil
		case strings.HasPrefix(joined, "rev-list"):
			return []byte("1\n"), nil
		case strings.Contains(joined, "--numstat"):
			return []byte("1\t2\ta.txt\x00"), nil
		case strings.HasPrefix(joined, "log"):
			return []byte(landingSHA + "\x00subject\x00me\n"), nil
		}
		return nil, nil
	}
	return &server{landing: svc, home: home}, &ran
}

func codeOf(err error) codes.Code { return grpcstatus.Code(err) }

func TestLandingRPCsWithoutTheService(t *testing.T) {
	s := &server{}
	ctx := context.Background()
	_, e1 := s.GetPRLanding(ctx, &orchdv1.PRLandingRequest{TaskId: "t"})
	_, e2 := s.ListPRLandings(ctx, &orchdv1.ListPRLandingsRequest{})
	_, e3 := s.LandPR(ctx, &orchdv1.LandPRRequest{TaskId: "t"})
	_, e4 := s.UndoPRLanding(ctx, &orchdv1.PRLandingRequest{TaskId: "t"})
	_, e5 := s.DiscardPRLanding(ctx, &orchdv1.DiscardPRLandingRequest{TaskId: "t"})
	for i, err := range []error{e1, e2, e3, e4, e5} {
		if codeOf(err) != codes.Unavailable {
			t.Errorf("rpc %d: %v", i, err)
		}
	}
}

func TestLandingRPCs(t *testing.T) {
	s, ran := landingServer(t)
	ctx := context.Background()

	got, err := s.GetPRLanding(ctx, &orchdv1.PRLandingRequest{TaskId: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != landing.StateProposed || !got.GetCanLand() || got.GetSha() != landingSHA || got.GetBranch() != "feat/x" {
		t.Fatalf("got %+v", got)
	}
	if len(got.GetCommits()) != 1 || got.GetCommits()[0].GetSubject() != "subject" || len(got.GetFiles()) != 1 || got.GetFiles()[0].GetDeletions() != 2 {
		t.Errorf("change: %+v", got)
	}

	// A commit other than the proposal is refused before anything is pushed.
	if _, err := s.LandPR(ctx, &orchdv1.LandPRRequest{TaskId: "task-1", Sha: strings.Repeat("c", 40)}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("wrong commit: %v", err)
	}
	if strings.Contains(strings.Join(*ran, "\n"), " push ") {
		t.Error("a refused land pushed")
	}

	if got, err = s.DiscardPRLanding(ctx, &orchdv1.DiscardPRLandingRequest{TaskId: "task-1"}); err != nil || got.GetState() != landing.StateDiscarded {
		t.Fatalf("discard: %+v, %v", got, err)
	}
	list, err := s.ListPRLandings(ctx, &orchdv1.ListPRLandingsRequest{})
	if err != nil || len(list.GetDecisions()) != 1 || list.GetDecisions()[0].GetState() != landing.StateDiscarded {
		t.Errorf("list: %+v, %v", list, err)
	}
	if got, err = s.DiscardPRLanding(ctx, &orchdv1.DiscardPRLandingRequest{TaskId: "task-1", Restore: true}); err != nil || got.GetState() != landing.StateProposed {
		t.Fatalf("restore: %+v, %v", got, err)
	}

	if got, err = s.LandPR(ctx, &orchdv1.LandPRRequest{TaskId: "task-1", Sha: landingSHA}); err != nil || got.GetState() != landing.StateLanded {
		t.Fatalf("land: %+v, %v", got, err)
	}
	if !strings.Contains(strings.Join(*ran, "\n"), "https://github.com/acme/widget.git "+landingSHA+":refs/heads/feat/x") {
		t.Errorf("push not as expected: %v", *ran)
	}
	// Undo is refused here: the fake branch never moved to the landed commit.
	if _, err := s.UndoPRLanding(ctx, &orchdv1.PRLandingRequest{TaskId: "task-1"}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("undo: %v", err)
	}
}

func TestLandingResultMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{landing.ErrNotFound, codes.NotFound},
		{errors.Join(landing.ErrRefused, errors.New("no")), codes.FailedPrecondition},
		{errors.New("git: rejected"), codes.Unavailable},
	} {
		if _, err := landingResult(landing.Landing{}, tc.err); codeOf(err) != tc.want {
			t.Errorf("%v: got %v", tc.err, err)
		}
	}
}

func TestGetTaskActivity(t *testing.T) {
	home := t.TempDir()
	s := &server{home: home}
	ctx := context.Background()
	if _, err := s.GetTaskActivity(ctx, &orchdv1.GetTaskActivityRequest{}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("no id: %v", err)
	}
	got, err := s.GetTaskActivity(ctx, &orchdv1.GetTaskActivityRequest{Id: "t1"})
	if err != nil || got.GetAvailable() {
		t.Errorf("no transcript: %+v, %v", got, err)
	}
	dir := filepath.Join(home, ".claude", "projects", "-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"2026-11-01T00:00:00Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push"}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, agentrun.SessionID("t1")+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTaskActivity(ctx, &orchdv1.GetTaskActivityRequest{Id: "t1"})
	if err != nil || !got.GetAvailable() || got.GetTotal() != 1 || got.GetSteps()[0].GetRisk() != "git push" {
		t.Errorf("got %+v, %v", got, err)
	}
}

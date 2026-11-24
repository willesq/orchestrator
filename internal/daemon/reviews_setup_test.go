package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/reviews"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestWatchPRReviewSetup(t *testing.T) {
	s, _, _ := reviewsServer(t)
	ctx := context.Background()

	r, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict"})
	if err != nil || r.GetInstructionsFrom() != reviews.FromDefault || r.GetCompareAgainst() != reviews.CompareBase || r.GetProjectHooks() != "" {
		t.Fatalf("default setup = %+v, %v", r, err)
	}
	bad := &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict", SetSetup: true, InstructionsFrom: "fork"}
	if _, err := s.WatchPRReview(ctx, bad); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad setup: %v", err)
	}
	set := &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict", SetSetup: true,
		InstructionsFrom: reviews.FromBase, CompareAgainst: reviews.CompareDefault, ProjectHooks: reviews.HooksOff}
	if r, err = s.WatchPRReview(ctx, set); err != nil || r.GetInstructionsFrom() != reviews.FromBase ||
		r.GetCompareAgainst() != reviews.CompareDefault || r.GetProjectHooks() != reviews.HooksOff {
		t.Fatalf("set = %+v, %v", r, err)
	}
	// Changing something else keeps the setup.
	if r, err = s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "comment"}); err != nil || r.GetInstructionsFrom() != reviews.FromBase {
		t.Fatalf("kept = %+v, %v", r, err)
	}
	list, err := s.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{})
	if err != nil || len(list.GetReviews()) != 1 || list.GetReviews()[0].GetProjectHooks() != reviews.HooksOff {
		t.Fatalf("ListPRReviews = %+v, %v", list, err)
	}
}

func TestReviewDispatchRunsInItsCheckout(t *testing.T) {
	got := &[3]string{}
	w := newReviewWatcher(nil, fakeDispatcher{id: "task-1", got: got}, nil, nil, nil, nil)
	if _, err := w.Dispatch(context.Background(), "acme/api", "review", reviews.Checkout{Cwd: "/work/x/setup"}); err != nil {
		t.Fatal(err)
	}
	if got[0] != reviews.Agent || got[2] != "/work/x/setup" {
		t.Errorf("dispatched %q", got)
	}
	if _, err := w.Dispatch(context.Background(), "acme/api", "review", reviews.Checkout{}); err != nil || got[2] != "" {
		t.Errorf("no checkout: %q, %v", got, err)
	}
}

func TestReviewHooksDefault(t *testing.T) {
	ctx := context.Background()
	login := func(name string, err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return name, err }
	}
	for _, tt := range []struct {
		name  string
		login func(context.Context) (string, error)
		owner string
		want  bool
	}{
		{name: "the user's own account", login: login("WillEsq", nil), owner: "willesq", want: true},
		{name: "someone else's", login: login("me", nil), owner: "stranger"},
		{name: "login unreadable", login: login("", errors.New("offline")), owner: "me"},
		{name: "no login", owner: "me"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewHooksDefault(ctx, nil, tt.login, tt.owner); got != tt.want {
				t.Errorf("got %v", got)
			}
		})
	}

	// Under a spaces policy, owners bound to a space run hooks; quarantined
	// (unbound) owners do not.
	s := policyServer(t)
	ghp := &ghProfiles{spaces: s.spaces}
	for repo, space := range repoSpaces {
		owner, _, _ := strings.Cut(repo, "/")
		want := space != "" && space != "quarantine"
		if got := reviewHooksDefault(ctx, ghp, nil, owner); got != want {
			t.Errorf("%s (space %q): hooks %v, want %v", owner, space, got, want)
		}
	}
	w := newReviewWatcher(nil, spaceDispatcher{got: new(string)}, nil, nil, nil, ghp)
	w.Login = login("me", nil)
	if !w.HooksDefault(ctx, "acme") || w.HooksDefault(ctx, "stranger") || !w.HooksDefault(ctx, "me") {
		t.Error("the watcher's hooks default does not follow the policy")
	}
}

func TestNewReviewCheckouts(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	c := newReviewCheckouts(nil)
	if c.Root != filepath.Join(paths.DataDir(), "review-checkouts") || c.Git == nil || c.Env == nil {
		t.Errorf("checkouts = %+v", c)
	}
	if env, err := c.Env(context.Background(), "acme"); env != nil || err != nil {
		t.Errorf("default login env = %v, %v", env, err)
	}
}

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/reviews"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/workflows"
)

const reviewURL = "https://github.com/willesq/theCalculator/pull/7"

func reviewsServer(t *testing.T) (*server, *[3]string, *fakeTasks) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	got := &[3]string{}
	route := &workflows.Route{Kind: "unset"}
	tasks := &fakeTasks{one: queue.Task{Status: "pending"}, found: true}
	gh := github.NewService(github.NewStore(conn), func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "api user --jq .login" {
			return []byte("me\n"), nil
		}
		return nil, nil
	})
	w := newReviewWatcher(reviews.NewStore(conn), fakeDispatcher{id: "task-1", got: got, route: route}, tasks, nil, gh, nil)
	w.GH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "api" {
			return []byte("OWNER\n"), nil
		}
		return []byte(`{"state":"OPEN","isDraft":false,"headRefOid":"` + strings.Repeat("a", 40) +
			`","mergeable":"MERGEABLE","statusCheckRollup":[],"author":{"login":"other"}}`), nil
	}
	t.Cleanup(func() {
		if route.Kind != "unset" && route.Kind != "" {
			t.Errorf("review dispatched with a route: %+v", route)
		}
	})
	return &server{reviews: w}, got, tasks
}

func TestReviewRPCs(t *testing.T) {
	s, got, _ := reviewsServer(t)
	ctx := context.Background()

	if _, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: "https://evil.com/a/b/pull/1", Mode: "verdict"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad URL: %v", err)
	}
	if _, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "merge"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad mode: %v", err)
	}
	r, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "comment", SetComment: true, Comment: "draft"})
	if err != nil || r.GetState() != "waiting" || r.GetComment() != "draft" || r.GetNumber() != 7 || r.GetCreatedAt() == 0 {
		t.Fatalf("WatchPRReview = %+v, %v", r, err)
	}

	s.reviews.Tick(ctx)
	if got[0] != reviews.Agent || got[2] != "" || !strings.Contains(got[1], reviewURL) {
		t.Errorf("dispatched %q", got)
	}

	list, err := s.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{})
	if err != nil || len(list.GetReviews()) != 1 || list.GetReviews()[0].GetState() != "reviewing" ||
		list.GetReviews()[0].GetTaskId() != "task-1" || list.GetReviews()[0].GetVerdictAt() != 0 {
		t.Fatalf("ListPRReviews = %+v, %v", list, err)
	}

	if _, err := s.StopPRReview(ctx, &orchdv1.StopPRReviewRequest{Url: "https://github.com/a/b/pull/9"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("stop unknown: %v", err)
	}
	if _, err := s.StopPRReview(ctx, &orchdv1.StopPRReviewRequest{Url: reviewURL, Forget: true}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{}); len(list.GetReviews()) != 0 {
		t.Errorf("forgotten review listed: %v", list)
	}
}

func TestReviewRPCsWatcherWiring(t *testing.T) {
	s, _, tasks := reviewsServer(t)
	ctx := context.Background()
	if login, err := s.reviews.Login(ctx); err != nil || login != "me" {
		t.Errorf("Login = %q, %v", login, err)
	}
	tasks.one = queue.Task{Status: "complete", Result: "done"}
	status, result, found, err := s.reviews.Task(ctx, "task-9")
	if status != "complete" || result != "done" || !found || err != nil || tasks.gotID != "task-9" {
		t.Errorf("Task = %q %q %v %v", status, result, found, err)
	}
	if u := reviewsUnit(s.reviews); u.Name != UnitReviews || u.Tick == nil || u.Interval != reviewsTick {
		t.Errorf("unit = %+v", u)
	}
}

func TestReviewRPCsUnavailable(t *testing.T) {
	s := &server{}
	ctx := context.Background()
	calls := []func() error{
		func() error { _, err := s.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{}); return err },
		func() error {
			_, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict"})
			return err
		},
		func() error { _, err := s.StopPRReview(ctx, &orchdv1.StopPRReviewRequest{Url: reviewURL}); return err },
		func() error { _, err := s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: reviewURL}); return err },
	}
	for i, call := range calls {
		if err := call(); grpcstatus.Code(err) != codes.Unavailable {
			t.Errorf("call %d = %v, want Unavailable", i, err)
		}
	}
}

func TestReviewRPCsStoreErrors(t *testing.T) {
	s, _, _ := reviewsServer(t)
	conn, _ := db.Open()
	_ = conn.Close()
	s.reviews.Store = reviews.NewStore(conn)
	ctx := context.Background()
	if _, err := s.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{}); err == nil {
		t.Error("List on closed db succeeded")
	}
	if _, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict"}); err == nil {
		t.Error("Watch on closed db succeeded")
	}
	if _, err := s.StopPRReview(ctx, &orchdv1.StopPRReviewRequest{Url: reviewURL}); err == nil || grpcstatus.Code(err) == codes.NotFound {
		t.Errorf("Stop on closed db = %v", err)
	}
}

func TestGetPRDetail(t *testing.T) {
	calls := 0
	fail := false
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, errors.New("rate limited")
		}
		return []byte(`{"title":"Add divide","author":{"login":"dev"},"state":"OPEN","headRefOid":"abc",
			"additions":3,"deletions":1,"changedFiles":1,"labels":[{"name":"bug"}],
			"reviewRequests":[{"login":"me"}],"latestReviews":[{"author":{"login":"bob"},"state":"COMMENTED"}],
			"statusCheckRollup":[{"name":"test","conclusion":"SUCCESS"}],"files":[{"path":"calc.js","additions":3,"deletions":1}]}`), nil
	}
	cache := newDetailCache(gh)
	clock := time.Unix(1_700_000_000, 0)
	cache.now = func() time.Time { return clock }
	s := &server{details: cache}
	ctx := context.Background()

	if _, err := s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: "nope"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad URL: %v", err)
	}
	d, err := s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: reviewURL})
	if err != nil || d.GetTitle() != "Add divide" || d.GetCiState() != "success" || d.GetFiles()[0].GetPath() != "calc.js" ||
		d.GetChecks()[0].GetResult() != "success" || d.GetReviews()[0].GetLogin() != "bob" || d.GetFetchedAt() != clock.Unix() ||
		d.GetLabels()[0] != "bug" || d.GetRequestedReviewers()[0] != "me" {
		t.Fatalf("GetPRDetail = %+v, %v", d, err)
	}
	// Cached for a minute; a refresh inside 10 s is served from cache too.
	clock = clock.Add(5 * time.Second)
	_, _ = s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: reviewURL, Refresh: true})
	clock = clock.Add(30 * time.Second)
	_, _ = s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: reviewURL})
	if calls != 1 {
		t.Fatalf("gh calls = %d, want 1", calls)
	}
	_, _ = s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: reviewURL, Refresh: true})
	if calls != 2 {
		t.Fatalf("refresh after 10 s did not fetch: %d calls", calls)
	}
	fail = true
	clock = clock.Add(2 * time.Minute)
	if _, err := s.GetPRDetail(ctx, &orchdv1.GetPRDetailRequest{Url: reviewURL}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("gh failure = %v", err)
	}
}

func TestDetailCacheEvictsOldest(t *testing.T) {
	cache := newDetailCache(func(context.Context, ...string) ([]byte, error) { return []byte(`{}`), nil })
	clock := time.Unix(0, 0)
	cache.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	ctx := context.Background()
	for i := 1; i <= detailCacheSize+1; i++ {
		if _, _, err := cache.get(ctx, "https://github.com/a/b/pull/"+strconv.Itoa(i), false); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != detailCacheSize {
		t.Fatalf("entries = %d", len(cache.entries))
	}
	if _, ok := cache.entries["https://github.com/a/b/pull/1"]; ok {
		t.Error("oldest entry kept")
	}
}

func TestWatchPRReviewInstructions(t *testing.T) {
	s, got, _ := reviewsServer(t)
	ctx := context.Background()
	long := strings.Repeat("x", reviews.MaxInstructions+1)
	if _, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict", SetInstructions: true, Instructions: long}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("long instructions: %v", err)
	}
	r, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "auto", SetInstructions: true, Instructions: "focus on units"})
	if err != nil || r.GetInstructions() != "focus on units" || r.GetMode() != "auto" {
		t.Fatalf("WatchPRReview = %+v, %v", r, err)
	}
	s.reviews.Tick(ctx)
	if !strings.Contains(got[1], "focus on units") {
		t.Fatalf("prompt lacks notes: %q", got[1])
	}
	list, _ := s.ListPRReviews(ctx, &orchdv1.ListPRReviewsRequest{})
	if pb := list.GetReviews()[0]; pb.GetCheckedAt() == 0 || pb.GetNextCheckAt() != 0 {
		t.Fatalf("schedule while reviewing: %+v", pb)
	}
}

func TestWatchPRReviewAnyway(t *testing.T) {
	s, _, _ := reviewsServer(t)
	r, err := s.WatchPRReview(context.Background(), &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict", ReviewAnyway: true})
	if err != nil || !r.GetGoAhead() {
		t.Fatalf("review anyway = %+v, %v", r, err)
	}
}

func TestSetPRReviewUpdateInPlace(t *testing.T) {
	s, _, _ := reviewsServer(t)
	ctx := context.Background()
	req := &orchdv1.SetPRReviewUpdateInPlaceRequest{Url: reviewURL, UpdateInPlace: true}
	if _, err := s.SetPRReviewUpdateInPlace(ctx, req); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("unwatched: %v", err)
	}
	if _, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetPRReviewUpdateInPlace(ctx, req)
	if err != nil || !got.GetUpdateInPlace() {
		t.Fatalf("%+v, %v", got, err)
	}
	w, err := s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict"})
	if err != nil || !w.GetUpdateInPlace() {
		t.Fatalf("kept: %+v, %v", w, err)
	}
	w, err = s.WatchPRReview(ctx, &orchdv1.WatchPRReviewRequest{Url: reviewURL, Mode: "verdict", SetUpdateInPlace: true})
	if err != nil || w.GetUpdateInPlace() {
		t.Fatalf("set off: %+v, %v", w, err)
	}
}

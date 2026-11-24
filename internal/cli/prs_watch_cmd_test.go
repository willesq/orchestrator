package cli

import (
	"errors"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// prWatches is what fakeClient's PR review calls answer and record.
var prWatches struct {
	reviews          []*orchdv1.PRReview
	stopped          []*orchdv1.StopPRReviewRequest
	listErr, stopErr error
}

const watchedURL = "https://github.com/o/r/pull/1"

func resetPRWatches(t *testing.T, reviews ...*orchdv1.PRReview) {
	t.Helper()
	prWatches.reviews, prWatches.stopped, prWatches.listErr, prWatches.stopErr = reviews, nil, nil, nil
	t.Cleanup(func() {
		prWatches.reviews, prWatches.stopped, prWatches.listErr, prWatches.stopErr = nil, nil, nil, nil
	})
}

func TestPRsWatches(t *testing.T) {
	resetPRWatches(t)
	out, err := run(factory(&fakeClient{}, nil), "", "prs", "watches")
	if err != nil || !strings.Contains(out, "no PRs watched") {
		t.Fatalf("empty: %q, %v", out, err)
	}

	resetPRWatches(t, &orchdv1.PRReview{Url: watchedURL, State: "waiting", Mode: "verdict",
		Note: "waiting: CI is failing\x1b[31m"})
	out, err = run(factory(&fakeClient{}, nil), "", "prs", "watches")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"waiting", "mode=verdict", "verdict=none", watchedURL, "waiting: CI is failing"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("control character reached the terminal: %q", out)
	}
	if strings.Contains(out, "posted=") {
		t.Errorf("nothing posted, but the line says so: %q", out)
	}
	if strings.Contains(out, "setup:") {
		t.Errorf("the default setup is spelled out: %q", out)
	}

	resetPRWatches(t, &orchdv1.PRReview{Url: watchedURL, State: "waiting", Mode: "verdict",
		InstructionsFrom: "head", CompareAgainst: "base", ProjectHooks: ""})
	out, err = run(factory(&fakeClient{}, nil), "", "prs", "watches")
	if err != nil || !strings.Contains(out, "setup: instructions from head, compared against base, project hooks automatic") {
		t.Errorf("non-default setup: %q, %v", out, err)
	}
	resetPRWatches(t, &orchdv1.PRReview{Url: watchedURL, State: "waiting", Mode: "verdict",
		InstructionsFrom: "default", CompareAgainst: "default", ProjectHooks: "off"})
	out, _ = run(factory(&fakeClient{}, nil), "", "prs", "watches")
	if !strings.Contains(out, "compared against default, project hooks off") {
		t.Errorf("non-default setup: %q", out)
	}

	resetPRWatches(t, &orchdv1.PRReview{Url: watchedURL, State: "reviewing", Mode: "feedback", Verdict: "changes",
		Posted: "superseded", Note: "superseded by bbbbbbb: the verdict for aaaaaaa was not posted"})
	out, err = run(factory(&fakeClient{}, nil), "", "prs", "watches")
	if err != nil || !strings.Contains(out, "posted=superseded") || !strings.Contains(out, "superseded by bbbbbbb") {
		t.Errorf("superseded verdict: %q, %v", out, err)
	}

	prWatches.listErr = errors.New("orchd down")
	if _, err := run(factory(&fakeClient{}, nil), "", "prs", "watches"); err == nil {
		t.Fatal("want the list error")
	}
	if _, err := run(factory(nil, errors.New("no daemon")), "", "prs", "watches"); err == nil {
		t.Fatal("want the dial error")
	}
}

func TestPRsStop(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		review   *orchdv1.PRReview
		forget   bool
		wantHint bool
	}{
		{name: "stops a settled watch", args: []string{"prs", "stop", watchedURL},
			review: &orchdv1.PRReview{Url: watchedURL, State: "verdict", TaskId: "task-1"}},
		{name: "forget reaches orchd", args: []string{"prs", "stop", watchedURL, "--forget"}, forget: true,
			review: &orchdv1.PRReview{Url: watchedURL, State: "waiting"}},
		{name: "says a review is still running", args: []string{"prs", "stop", watchedURL}, wantHint: true,
			review: &orchdv1.PRReview{Url: watchedURL, State: "reviewing", TaskId: "task-1"}},
		{name: "another PR's review is not mentioned", args: []string{"prs", "stop", watchedURL},
			review: &orchdv1.PRReview{Url: watchedURL + "2", State: "reviewing", TaskId: "task-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetPRWatches(t, tt.review)
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(prWatches.stopped) != 1 || prWatches.stopped[0].GetUrl() != watchedURL || prWatches.stopped[0].GetForget() != tt.forget {
				t.Fatalf("stopped %+v", prWatches.stopped)
			}
			if !strings.Contains(out, "stopped watching "+watchedURL) {
				t.Errorf("out = %q", out)
			}
			if got := strings.Contains(out, "orch task task-1 --cancel"); got != tt.wantHint {
				t.Errorf("hint = %v, want %v: %q", got, tt.wantHint, out)
			}
		})
	}
}

func TestPRsStopRefusedInAQueuedTask(t *testing.T) {
	resetPRWatches(t)
	t.Setenv("ORCH_TASK_ID", "task-1")
	if _, err := run(factory(&fakeClient{}, nil), "", "prs", "stop", watchedURL); err == nil || !strings.Contains(err.Error(), "queue tasks cannot") {
		t.Fatalf("err = %v", err)
	}
	if len(prWatches.stopped) != 0 {
		t.Fatalf("stopped %+v", prWatches.stopped)
	}
}

func TestPRsStopFailures(t *testing.T) {
	resetPRWatches(t)
	if _, err := run(factory(&fakeClient{}, nil), "", "prs", "stop"); err == nil {
		t.Fatal("want an error without a URL")
	}
	prWatches.stopErr = errors.New("not watched")
	if _, err := run(factory(&fakeClient{}, nil), "", "prs", "stop", watchedURL); err == nil || !strings.Contains(err.Error(), "not watched") {
		t.Fatalf("err = %v", err)
	}
	if len(prWatches.stopped) != 0 {
		t.Fatalf("stopped %+v", prWatches.stopped)
	}
	prWatches.stopErr, prWatches.listErr = nil, errors.New("orchd down")
	if _, err := run(factory(&fakeClient{}, nil), "", "prs", "stop", watchedURL); err == nil {
		t.Fatal("want the list error")
	}
	if _, err := run(factory(nil, errors.New("no daemon")), "", "prs", "stop", watchedURL); err == nil {
		t.Fatal("want the dial error")
	}
}

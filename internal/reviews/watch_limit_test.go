package reviews

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// reviewed takes a new watch through one approving review to its verdict.
func reviewed(t *testing.T, e *fakeEnv, w *Watcher, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.Watch(ctx, testURL, mode, nil); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	w.Tick(ctx)
	if r := get(t, w); r.State != StateVerdict {
		t.Fatalf("not at a verdict: %+v", r)
	}
}

// Each way a watch can be sitting when its PR needs reading again.
var activeStates = []struct {
	name  string
	state string
	setup func(t *testing.T, e *fakeEnv, w *Watcher)
}{
	{"waiting on a draft", StateWaiting, func(t *testing.T, e *fakeEnv, w *Watcher) {
		e.pr.draft = true
		_, _ = w.Watch(context.Background(), testURL, ModeVerdict, nil)
		w.Tick(context.Background())
	}},
	{"held", StateHeld, func(t *testing.T, e *fakeEnv, w *Watcher) {
		e.pr.assoc, e.pr.checks = "NONE", nil
		_, _ = w.Watch(context.Background(), testURL, ModeVerdict, nil)
		w.Tick(context.Background())
	}},
	{"at a verdict", StateVerdict, func(t *testing.T, e *fakeEnv, w *Watcher) {
		reviewed(t, e, w, ModeVerdict)
	}},
	{"failed", StateFailed, func(t *testing.T, e *fakeEnv, w *Watcher) {
		_, _ = w.Watch(context.Background(), testURL, ModeVerdict, nil)
		w.Tick(context.Background())
		e.tasks["task-1"] = [2]string{"complete", "no verdict block"}
		w.Tick(context.Background())
	}},
}

func TestMergedOrClosedPRStopsWatchingFromEveryState(t *testing.T) {
	ctx := context.Background()
	for _, tt := range activeStates {
		for _, state := range []string{"MERGED", "CLOSED"} {
			t.Run(tt.name+"/"+state, func(t *testing.T) {
				e, w := newEnv(t)
				tt.setup(t, e, w)
				if r := get(t, w); r.State != tt.state {
					t.Fatalf("setup = %+v, want %s", r, tt.state)
				}
				e.pr.state = state
				e.clock = e.clock.Add(DefaultPoll)
				w.Tick(ctx)
				r := get(t, w)
				if r.State != StateClosed || r.Note != "the PR is "+strings.ToLower(state) {
					t.Fatalf("%+v", r)
				}
				calls := e.ghCalls
				e.clock = e.clock.Add(DefaultPoll)
				w.Tick(ctx)
				if e.ghCalls != calls {
					t.Error("closed watch still read the PR")
				}
			})
		}
	}
}

func TestPRMergedDuringReviewStopsOnceItEnds(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeAuto, nil)
	w.Tick(ctx)
	e.pr.state = "MERGED"
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	w.Tick(ctx)
	w.Tick(ctx)
	r := get(t, w)
	if r.State != StateClosed || r.Note != "the PR is merged" {
		t.Fatalf("%+v", r)
	}
	if len(e.posts) != 0 {
		t.Errorf("posted to a merged PR: %+v", e.posts)
	}
	if r.Verdict != VerdictApprove {
		t.Errorf("verdict lost: %+v", r)
	}
}

func TestDraftFlipsKeepWatching(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	reviewed(t, e, w, ModeVerdict)
	started := get(t, w).WatchedAt

	// Back to draft, same commit: nothing changes.
	e.pr.draft = true
	e.clock = e.clock.Add(DefaultPoll)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateVerdict {
		t.Fatalf("draft = %+v", r)
	}
	// New commits while a draft: waits.
	e.pr.head = shaB
	e.clock = e.clock.Add(DefaultPoll)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateWaiting || !strings.Contains(r.Note, "draft") {
		t.Fatalf("draft with a new commit = %+v", r)
	}
	// Open again: reviews the new commit, on the same watch.
	e.pr.draft = false
	e.clock = e.clock.Add(DefaultPoll)
	w.Tick(ctx)
	r := get(t, w)
	if r.State != StateReviewing || r.PendingSHA != shaB || len(e.dispatched) != 2 {
		t.Fatalf("open again = %+v, %d dispatched", r, len(e.dispatched))
	}
	if !r.WatchedAt.Equal(started) {
		t.Errorf("WatchedAt moved from %v to %v", started, r.WatchedAt)
	}
}

func TestWatchLimitStopsEveryState(t *testing.T) {
	ctx := context.Background()
	for _, tt := range activeStates {
		t.Run(tt.name, func(t *testing.T) {
			e, w := newEnv(t)
			tt.setup(t, e, w)
			before := get(t, w)
			if before.State != tt.state {
				t.Fatalf("setup = %+v, want %s", before, tt.state)
			}
			// Exactly at the limit the watch still runs; past it, it stops.
			e.clock = before.WatchedAt.Add(MaxWatch)
			w.Tick(ctx)
			if r := get(t, w); r.State != tt.state {
				t.Fatalf("at the limit = %+v", r)
			}
			e.clock = e.clock.Add(DefaultPoll)
			w.Tick(ctx)
			r := get(t, w)
			if r.State != StateStopped || !strings.Contains(r.Note, "14-day") || !strings.Contains(r.Note, "watch the PR again") {
				t.Fatalf("past the limit = %+v", r)
			}
			if r.Verdict != before.Verdict {
				t.Errorf("verdict changed: %q to %q", before.Verdict, r.Verdict)
			}
			calls := e.ghCalls
			e.clock = e.clock.Add(DefaultPoll)
			w.Tick(ctx)
			if e.ghCalls != calls {
				t.Error("stopped watch still read the PR")
			}
		})
	}
}

func TestWatchLimitDoesNotStartAReview(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	e.pr.draft = true
	_, _ = w.Watch(ctx, testURL, ModeAuto, nil)
	w.Tick(ctx)
	e.pr.draft = false
	e.clock = e.clock.Add(MaxWatch + time.Second)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateStopped || len(e.dispatched) != 0 {
		t.Fatalf("%+v, %d dispatched", r, len(e.dispatched))
	}
}

func TestWatchLimitWhenThePRCannotBeRead(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeVerdict, nil)
	e.pr.err = errors.New("HTTP 404")
	w.Tick(ctx)
	if r := get(t, w); r.State != StateWaiting || !strings.Contains(r.Note, "could not read") {
		t.Fatalf("before the limit = %+v", r)
	}
	e.clock = e.clock.Add(MaxWatch + time.Second)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateStopped || !strings.Contains(r.Note, "14-day") {
		t.Fatalf("past the limit = %+v", r)
	}
}

func TestWatchLimitLetsAReviewFinish(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeAuto, nil)
	w.Tick(ctx)
	e.clock = e.clock.Add(MaxWatch + time.Second)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateReviewing {
		t.Fatalf("under review = %+v", r)
	}
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	w.Tick(ctx)
	w.Tick(ctx)
	r := get(t, w)
	if r.State != StateStopped || r.Verdict != VerdictApprove || !strings.Contains(r.Note, "14-day") {
		t.Fatalf("%+v", r)
	}
	if len(e.posts) != 0 {
		t.Errorf("posted past the limit: %+v", e.posts)
	}
}

func TestMergedPRReadsAsClosedPastTheLimit(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeVerdict, nil)
	e.pr.state = "MERGED"
	e.clock = e.clock.Add(MaxWatch + time.Hour)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateClosed || r.Note != "the PR is merged" {
		t.Fatalf("%+v", r)
	}
}

func TestWatchingAgainRestartsTheLimit(t *testing.T) {
	ctx := context.Background()
	stoppers := []struct {
		name string
		stop func(e *fakeEnv, w *Watcher)
	}{
		{"limit reached", func(e *fakeEnv, w *Watcher) {
			e.clock = e.clock.Add(MaxWatch + time.Second)
			w.Tick(ctx)
		}},
		{"stopped by the user", func(e *fakeEnv, w *Watcher) {
			e.clock = e.clock.Add(time.Hour)
			_ = w.Stop(ctx, testURL, false)
		}},
		{"closed", func(e *fakeEnv, w *Watcher) {
			e.pr.state = "CLOSED"
			e.clock = e.clock.Add(time.Hour)
			w.Tick(ctx)
			e.pr.state = "OPEN"
		}},
	}
	for _, tt := range stoppers {
		t.Run(tt.name, func(t *testing.T) {
			e, w := newEnv(t)
			reviewed(t, e, w, ModeVerdict)
			tt.stop(e, w)
			if get(t, w).Active() {
				t.Fatal("still active")
			}
			r, err := w.Watch(ctx, testURL, ModeVerdict, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.State != StateWaiting || r.Note != "" || !r.WatchedAt.Equal(e.clock.UTC()) {
				t.Fatalf("rewatch = %+v, clock %v", r, e.clock)
			}
			// The verdict still covers this commit, with a full limit ahead.
			e.clock = e.clock.Add(MaxWatch)
			w.Tick(ctx)
			if r := get(t, w); r.State != StateVerdict {
				t.Fatalf("after a full limit = %+v", r)
			}
			e.clock = e.clock.Add(DefaultPoll)
			w.Tick(ctx)
			if r := get(t, w); r.State != StateStopped {
				t.Fatalf("past the new limit = %+v", r)
			}
		})
	}
}

func TestChangingARunningWatchKeepsItsLimit(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	reviewed(t, e, w, ModeVerdict)
	started := get(t, w).WatchedAt
	notes := "look at the error paths"
	on := true
	e.clock = e.clock.Add(time.Hour)
	changes := []func() (Review, error){
		func() (Review, error) { return w.Watch(ctx, testURL, ModeAuto, nil) },
		func() (Review, error) { return w.WatchWith(ctx, testURL, ModeAuto, nil, &notes) },
		func() (Review, error) { return w.WatchOpts(ctx, testURL, ModeAuto, nil, nil, true, &on) },
		func() (Review, error) { return w.SetUpdateInPlace(ctx, testURL, false) },
	}
	for i, change := range changes {
		r, err := change()
		if err != nil {
			t.Fatalf("change %d: %v", i, err)
		}
		if !r.WatchedAt.Equal(started) {
			t.Errorf("change %d moved WatchedAt from %v to %v", i, started, r.WatchedAt)
		}
	}
}

func TestWatchedAtSurvivesTheStore(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	r, err := w.Watch(ctx, testURL, ModeVerdict, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.WatchedAt.Equal(e.clock.UTC()) {
		t.Fatalf("WatchedAt = %v, clock %v", r.WatchedAt, e.clock)
	}
	if got := get(t, w).WatchedAt; !got.Equal(r.WatchedAt) {
		t.Errorf("read back %v, want %v", got, r.WatchedAt)
	}
}

func TestGoAheadDoesNotCarryIntoANewWatch(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		give bool
	}{{"not given again", false}, {"given again", true}} {
		t.Run(tt.name, func(t *testing.T) {
			e, w := newEnv(t)
			e.pr.draft = true
			if _, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, true, nil); err != nil {
				t.Fatal(err)
			}
			e.clock = e.clock.Add(MaxWatch + time.Second)
			w.Tick(ctx)
			if r := get(t, w); r.State != StateStopped || !r.GoAhead {
				t.Fatalf("expired = %+v", r)
			}
			r, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, tt.give, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.GoAhead != tt.give {
				t.Fatalf("GoAhead = %v, want %v", r.GoAhead, tt.give)
			}
			// An untrusted author's ready PR holds without one.
			e.pr.draft, e.pr.assoc, e.pr.checks = false, "NONE", nil
			w.Tick(ctx)
			want := StateHeld
			if tt.give {
				want = StateReviewing
			}
			if r := get(t, w); r.State != want {
				t.Fatalf("state = %q, want %q", r.State, want)
			}
		})
	}
}

func TestVerdictKeptAtTheLimitPostsOnlyAfterWatchingAgain(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeApprove, nil)
	w.Tick(ctx)
	e.clock = e.clock.Add(MaxWatch + time.Second)
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	w.Tick(ctx)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateStopped || len(e.posts) != 0 {
		t.Fatalf("%+v, posts %+v", r, e.posts)
	}
	_, _ = w.Watch(ctx, testURL, ModeApprove, nil)
	w.Tick(ctx)
	if len(e.posts) != 1 || e.posts[0].event != "APPROVE" || e.posts[0].commit != shaA {
		t.Fatalf("posts = %+v", e.posts)
	}
}

// The poll that would stop the watch has not run yet, or orchd was down: what
// the user asks for now is a new watch, not lost to the next check.
func TestWatchingPastTheLimitBeforeItStopsIsANewWatch(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	notes := "look again"
	reviewed(t, e, w, ModeVerdict)
	e.clock = e.clock.Add(MaxWatch + time.Second)
	r, err := w.WatchWith(ctx, testURL, ModeVerdict, nil, &notes)
	if err != nil {
		t.Fatal(err)
	}
	if !r.WatchedAt.Equal(e.clock.UTC()) {
		t.Fatalf("WatchedAt = %v, clock %v", r.WatchedAt, e.clock)
	}
	w.Tick(ctx)
	if r := get(t, w); r.State != StateReviewing || r.Instructions != notes {
		t.Fatalf("after the check = %+v", r)
	}
}

func TestPreferenceChangePastTheLimitDoesNotExtendIt(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	reviewed(t, e, w, ModeVerdict)
	e.clock = e.clock.Add(MaxWatch + time.Second)
	if _, err := w.SetUpdateInPlace(ctx, testURL, true); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	if r := get(t, w); r.State != StateStopped {
		t.Fatalf("%+v", r)
	}
}

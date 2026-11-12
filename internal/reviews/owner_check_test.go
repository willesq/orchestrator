package reviews

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRefusedOwnerIsNotReviewed is SP-023.1.a4 for the review watcher: the
// PR owner's org binding is re-checked before a review is started and
// before a verdict is recorded; while it is refused, nothing from the PR is
// acted on or kept.
func TestRefusedOwnerIsNotReviewed(t *testing.T) {
	e, w := newEnv(t)
	ctx := context.Background()
	refuse := true
	var asked []string
	w.CheckOwner = func(_ context.Context, owner string) error {
		asked = append(asked, owner)
		if refuse {
			return errors.New("binding not confirmed")
		}
		return nil
	}
	if _, err := w.Watch(ctx, testURL, ModeVerdict, nil); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	r := get(t, w)
	if len(e.dispatched) != 0 || r.State != StateWaiting || !strings.Contains(r.Note, "not reviewed: binding not confirmed") {
		t.Fatalf("a refused owner's PR: %+v, dispatched %d", r, len(e.dispatched))
	}
	if len(asked) == 0 || asked[0] != "willesq" {
		t.Fatalf("asked %v", asked)
	}

	// Confirmed again: the review starts.
	refuse = false
	e.clock = e.clock.Add(DefaultPoll)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateReviewing || len(e.dispatched) != 1 {
		t.Fatalf("after confirming: %+v", r)
	}

	// Refused before the verdict lands: the verdict is not recorded.
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	refuse = true
	w.Tick(ctx)
	r = get(t, w)
	if r.State != StateFailed || r.Verdict != "" || r.Comment != "" || !strings.Contains(r.Note, "not reviewed") {
		t.Fatalf("verdict of a refused owner: %+v", r)
	}
	if len(e.posts) != 0 {
		t.Fatalf("posted %v", e.posts)
	}
}

func TestOwnerRefusedNeedsAnOwner(t *testing.T) {
	w := &Watcher{CheckOwner: func(context.Context, string) error { return nil }}
	if note := w.ownerRefused(context.Background(), "https://github.com/"); !strings.Contains(note, "cannot be read") {
		t.Fatalf("note = %q", note)
	}
	if note := (&Watcher{}).ownerRefused(context.Background(), testURL); note != "" {
		t.Fatalf("no check: %q", note)
	}
}

package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAttributorCountsPRsAsTheirSpacesAccount is SP-074.1.a3 for the
// PR-outcome tracker: with LoginFor, a PR counts as the task's when the
// account of its owner's space opened it, the default login is never asked
// for a space's own account, and an owner whose account cannot be used leaves
// the task undecided rather than judged as another login.
func TestAttributorCountsPRsAsTheirSpacesAccount(t *testing.T) {
	const (
		acmePR   = "https://github.com/acme/api/pull/1"
		otherPR  = "https://github.com/willesq/orch/pull/2"
		lockedPR = "https://github.com/initech/x/pull/3"
	)
	gh := &fakeGH{login: "me", prs: map[string]string{
		acmePR:   pr("MERGED", "work-me", opened, commit{authored: before}),
		otherPR:  pr("CLOSED", "me", opened),
		lockedPR: pr("MERGED", "work-initech", opened),
	}}
	var asked []string
	a := &Attributor{GH: gh.run, Recheck: time.Hour, Batch: 10, now: func() time.Time { return dispatched.Add(48 * time.Hour) },
		LoginFor: func(_ context.Context, owner string) (string, error) {
			asked = append(asked, owner)
			switch strings.ToLower(owner) {
			case "acme":
				return "work-me", nil
			case "initech":
				return "", errors.New("its space's gh profile is not logged in")
			}
			return "me", nil
		}}
	conn := testDB(t)
	a.DB = conn
	insertAt(t, conn, "acme", "code", acmePR, dispatched, finished)
	insertAt(t, conn, "acme-again", "code", acmePR, dispatched, finished)
	insertAt(t, conn, "other", "code", otherPR, dispatched, finished)
	insertAt(t, conn, "locked", "code", lockedPR, dispatched, finished)
	err := a.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "task locked") {
		t.Fatalf("sweep err = %v, want the locked task's failure", err)
	}
	for id, want := range map[string]string{"acme": Good, "acme-again": Good, "other": Bad, "locked": ""} {
		if _, got, _ := labels(t, conn, id); got != want {
			t.Errorf("%s outcome = %q, want %q", id, got, want)
		}
	}
	if gh.userErr == nil && gh.calls != 4 {
		t.Errorf("gh ran %d times, want only the 4 PR views (no 'gh api user')", gh.calls)
	}
	if n := strings.Count(strings.Join(asked, ","), "acme"); n != 1 {
		t.Errorf("acme's login asked %d times in one sweep, want 1 (%v)", n, asked)
	}
}

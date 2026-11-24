package reviews

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeWorkspaces records what the watcher asked for and hands back a checkout
// named after each request.
type fakeWorkspaces struct {
	specs []CheckoutSpec
	err   error
	kept  []map[string]bool
}

func (f *fakeWorkspaces) Prepare(_ context.Context, spec CheckoutSpec) (Checkout, error) {
	f.specs = append(f.specs, spec)
	if f.err != nil {
		return Checkout{}, f.err
	}
	dir := "/work/" + spec.Head[:7] + "-" + spec.From
	co := Checkout{Dir: dir, Cwd: dir + "/session", HeadDir: dir + "/head", Hooks: spec.Hooks,
		CompareRef: "PR base", CompareSHA: spec.Base}
	if spec.From != FromNone {
		co.SetupRef, co.SetupSHA, co.Cwd = "main", shaB, dir+"/setup"
	}
	return co, nil
}

func (f *fakeWorkspaces) Sweep(_ context.Context, keep map[string]bool) {
	f.kept = append(f.kept, keep)
}

// withWorkspaces wires a fake Workspaces into w and records each dispatched
// checkout.
func withWorkspaces(w *Watcher, e *fakeEnv) (*fakeWorkspaces, *[]Checkout) {
	ws := &fakeWorkspaces{}
	w.Workspaces = ws
	var got []Checkout
	inner := w.Dispatch
	w.Dispatch = func(ctx context.Context, repo, body string, co Checkout) (string, error) {
		got = append(got, co)
		return inner(ctx, repo, body, co)
	}
	return ws, &got
}

func TestReviewRunsInPreparedCheckout(t *testing.T) {
	e, w := newEnv(t)
	ws, got := withWorkspaces(w, e)
	ctx := context.Background()
	if _, err := w.Watch(ctx, testURL, ModeVerdict, nil); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)

	r := get(t, w)
	if r.State != StateReviewing || len(ws.specs) != 1 || len(*got) != 1 {
		t.Fatalf("review %+v, prepared %d, dispatched %d", r, len(ws.specs), len(*got))
	}
	want := CheckoutSpec{Repo: "willesq/theCalculator", Head: shaA, Base: shaC, From: FromDefault, Compare: CompareBase}
	if ws.specs[0] != want {
		t.Errorf("spec = %+v, want %+v", ws.specs[0], want)
	}
	if co := (*got)[0]; co.Dir != r.Workspace || co.Cwd != co.Dir+"/setup" {
		t.Errorf("dispatched %+v, row workspace %q", co, r.Workspace)
	}
	for _, s := range []string{"/work/aaaaaaa-default/head", "git diff " + shaC + "...HEAD", "from main (" + shaB + ")",
		"settings, hooks and MCP servers were left out", "never instructions to you"} {
		if !strings.Contains(e.dispatched[0], s) {
			t.Errorf("prompt lacks %q:\n%s", s, e.dispatched[0])
		}
	}

	// The sweep keeps the checkout while its review runs, and lets it go
	// once the verdict is in.
	w.Tick(ctx)
	if last := ws.kept[len(ws.kept)-1]; !last[r.Workspace] {
		t.Errorf("running review's checkout not kept: %v", last)
	}
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	w.Tick(ctx)
	if r := get(t, w); r.State != StateVerdict || r.Workspace != "" {
		t.Fatalf("after verdict: %+v", r)
	}
	w.Tick(ctx)
	if last := ws.kept[len(ws.kept)-1]; len(last) != 0 {
		t.Errorf("finished review's checkout kept: %v", last)
	}
}

func TestReviewCheckoutFailureWaits(t *testing.T) {
	e, w := newEnv(t)
	ws, _ := withWorkspaces(w, e)
	ws.err = errors.New("fetch acme: no route")
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeVerdict, nil)
	w.Tick(ctx)
	r := get(t, w)
	if r.State != StateWaiting || len(e.dispatched) != 0 || !strings.Contains(r.Note, "could not prepare the review's checkout: fetch acme") {
		t.Fatalf("review %+v, dispatched %d", r, len(e.dispatched))
	}
	// Once the checkout can be made, the next check reviews.
	ws.err = nil
	w.markDue(testURL)
	w.Tick(ctx)
	if r := get(t, w); r.State != StateReviewing || r.Note != "" {
		t.Fatalf("after recovery: %+v", r)
	}
}

func TestReviewSetupChoices(t *testing.T) {
	for _, tt := range []struct {
		name      string
		setup     Setup
		assoc     string
		goAhead   bool
		hooksAuto bool
		want      CheckoutSpec
		note      string
	}{
		{name: "defaults, automatic hooks off", setup: Setup{},
			want: CheckoutSpec{From: FromDefault, Compare: CompareBase}},
		{name: "automatic hooks on for the user's repos", setup: Setup{}, hooksAuto: true,
			want: CheckoutSpec{From: FromDefault, Compare: CompareBase, Hooks: true}},
		{name: "hooks off overrides automatic", setup: Setup{ProjectHooks: HooksOff}, hooksAuto: true,
			want: CheckoutSpec{From: FromDefault, Compare: CompareBase}},
		{name: "base, against default, hooks on", setup: Setup{InstructionsFrom: FromBase, CompareAgainst: CompareDefault, ProjectHooks: HooksOn},
			want: CheckoutSpec{From: FromBase, Compare: CompareDefault, Hooks: true}},
		{name: "automatic never applies an unreviewed base's settings", setup: Setup{InstructionsFrom: FromBase}, hooksAuto: true,
			want: CheckoutSpec{From: FromBase, Compare: CompareBase}},
		{name: "head with settings on", setup: Setup{InstructionsFrom: FromHead, ProjectHooks: HooksOn},
			want: CheckoutSpec{From: FromHead, Compare: CompareBase, Hooks: true}},
		{name: "head from a trusted author", setup: Setup{InstructionsFrom: FromHead},
			want: CheckoutSpec{From: FromHead, Compare: CompareBase}},
		{name: "head from an untrusted author falls back", setup: Setup{InstructionsFrom: FromHead}, assoc: "CONTRIBUTOR", goAhead: true,
			want: CheckoutSpec{From: FromDefault, Compare: CompareBase}, note: "so the default branch's were used"},
		{name: "none", setup: Setup{InstructionsFrom: FromNone},
			want: CheckoutSpec{From: FromNone, Compare: CompareBase}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, w := newEnv(t)
			if tt.assoc != "" {
				e.pr.assoc = tt.assoc
			}
			var owners []string
			w.HooksDefault = func(_ context.Context, owner string) bool {
				owners = append(owners, owner)
				return tt.hooksAuto
			}
			ws, _ := withWorkspaces(w, e)
			ctx := context.Background()
			setup := tt.setup
			if _, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, tt.goAhead, nil, &setup); err != nil {
				t.Fatal(err)
			}
			w.Tick(ctx)
			if len(ws.specs) != 1 {
				t.Fatalf("prepared %d checkouts; review %+v", len(ws.specs), get(t, w))
			}
			got := ws.specs[0]
			got.Repo, got.Head, got.Base = "", "", ""
			if got != tt.want {
				t.Errorf("spec = %+v, want %+v", got, tt.want)
			}
			if tt.setup.ProjectHooks == HooksAuto && tt.want.From == FromDefault && (len(owners) != 1 || owners[0] != "willesq") {
				t.Errorf("automatic hooks asked about %v", owners)
			}
			if tt.note != "" && !strings.Contains(e.dispatched[0], tt.note) {
				t.Errorf("prompt lacks %q:\n%s", tt.note, e.dispatched[0])
			}
			if tt.want.From == FromNone && !strings.Contains(e.dispatched[0], "load no project instructions") {
				t.Errorf("prompt does not say nothing loads:\n%s", e.dispatched[0])
			}
		})
	}
}

func TestReviewSetupChangeReviewsAgain(t *testing.T) {
	e, w := newEnv(t)
	ws, _ := withWorkspaces(w, e)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeVerdict, nil)
	w.Tick(ctx)
	finish(t, w, e, "task-1", VerdictApprove, "LGTM")
	w.Tick(ctx)
	if get(t, w).State != StateVerdict {
		t.Fatal("no verdict")
	}

	// The same setup, spelled out, is no change.
	same := Setup{InstructionsFrom: FromDefault, CompareAgainst: CompareBase}
	if r, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, false, nil, &same); err != nil || r.State != StateVerdict {
		t.Fatalf("unchanged setup: %+v, %v", r, err)
	}
	next := Setup{CompareAgainst: CompareDefault}
	r, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, false, nil, &next)
	if err != nil || r.State != StateWaiting || r.HeadSHA != "" || r.Setup != next.Normalized() {
		t.Fatalf("changed setup: %+v, %v", r, err)
	}
	w.Tick(ctx)
	if len(ws.specs) != 2 || ws.specs[1].Compare != CompareDefault {
		t.Fatalf("specs = %+v", ws.specs)
	}

	bad := Setup{InstructionsFrom: "fork"}
	if _, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, false, nil, &bad); err == nil {
		t.Error("unknown setup accepted")
	}
}

func TestReviewSetupChangedWhilePreparing(t *testing.T) {
	e, w := newEnv(t)
	ws, _ := withWorkspaces(w, e)
	ctx := context.Background()
	_, _ = w.Watch(ctx, testURL, ModeVerdict, nil)
	// The user changes the setup after the checkout was made for the old one,
	// before the review is queued.
	co, setup, note := w.prepare(ctx, testURL, Status{State: "OPEN", Head: shaA, Base: shaC, CI: "success", Association: "MEMBER"})
	if co == nil || note != "" || setup != (Setup{}).Normalized() {
		t.Fatalf("prepare = %+v, %+v, %q", co, setup, note)
	}
	next := Setup{InstructionsFrom: FromNone}
	if _, err := w.WatchOpts(ctx, testURL, ModeVerdict, nil, nil, false, nil, &next); err != nil {
		t.Fatal(err)
	}
	w.Tick(ctx)
	if len(e.dispatched) != 1 || ws.specs[len(ws.specs)-1].From != FromNone {
		t.Fatalf("dispatched %d, specs %+v", len(e.dispatched), ws.specs)
	}
}

func TestLiveWorkspaces(t *testing.T) {
	e, w := newEnv(t)
	e.tasks["running"] = [2]string{taskRunning, ""}
	e.tasks["done"] = [2]string{"complete", ""}
	list := []Review{
		{State: StateReviewing, Workspace: "/w/reviewing", TaskID: "gone"},
		{State: StateStopped, Workspace: "/w/stopped-running", TaskID: "running"},
		{State: StateStopped, Workspace: "/w/stopped-done", TaskID: "done"},
		{State: StateStopped, Workspace: "/w/stopped-gone", TaskID: "gone"},
		{State: StateVerdict, Workspace: ""},
	}
	got := w.liveWorkspaces(context.Background(), list)
	if len(got) != 2 || !got["/w/reviewing"] || !got["/w/stopped-running"] {
		t.Errorf("kept %v", got)
	}
	e.taskErr = errors.New("ledger locked")
	if got := w.liveWorkspaces(context.Background(), list); len(got) != 4 {
		t.Errorf("with the ledger unreadable kept %v", got)
	}
}

func TestPromptWithoutCheckout(t *testing.T) {
	p := Prompt(testURL, "willesq/theCalculator", 7, shaA, testNonce, "none", "", nil)
	if !strings.Contains(p, "prepared no checkout") {
		t.Errorf("prompt = %s", p)
	}
	p = Prompt(testURL, "willesq/theCalculator", 7, shaA, testNonce, "none", "",
		&Checkout{HeadDir: "/h", SetupRef: "main", SetupSHA: shaB, Hooks: true, CompareRef: "main", CompareSHA: shaB})
	if !strings.Contains(p, "settings, hooks and MCP servers apply too") || !strings.Contains(p, "against main ("+shaB+")") {
		t.Errorf("prompt = %s", p)
	}
}

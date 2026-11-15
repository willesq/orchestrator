package spaces

import (
	"context"
	"errors"
	"testing"
)

// TestEngineOff: with memory spaces switched off the engine reports no
// policy to every reader (INV-10's path), refuses an apply, and keeps the
// applied revision, enforced again once spaces are on.
func TestEngineOff(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	e := openEngine(t, conn)
	r := saveAndApply(t, e, validPolicy)

	e.SetOff(true)
	if !e.Off() {
		t.Fatal("Off() after SetOff(true)")
	}
	if !e.PolicyKept() {
		t.Fatal("the applied policy is not reported kept while off")
	}
	if got := current(t, e); got != nil {
		t.Fatalf("Current while off = %v, want no policy", got)
	}
	if got, rev, err := e.CurrentRevision(); got != nil || rev != 0 || err != nil {
		t.Fatalf("CurrentRevision while off = %v, %d, %v", got, rev, err)
	}
	st := status(t, e)
	if !st.Off || st.Configured || st.AppliedRevision != r.Number {
		t.Fatalf("status while off = %+v", st)
	}
	next, _, err := e.SaveDraft(ctx, otherValidPolicy, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, next.Number, "test", false, nil); !errors.Is(err, ErrSpacesOff) {
		t.Fatalf("Apply while off = %v, want ErrSpacesOff", err)
	}

	e.SetOff(false)
	if got := current(t, e); got == nil || !got.CanRead("acme", "acme") {
		t.Fatal("the kept revision is not enforced once spaces are on")
	}
	if st := status(t, e); st.Off || !st.Configured || st.AppliedRevision != r.Number {
		t.Fatalf("status once on = %+v", st)
	}
}

// TestEngineOffHidesABrokenPolicy: off, nothing is enforced, so a revision
// that no longer verifies refuses nothing either; on again, it fails
// closed as before.
func TestEngineOffHidesABrokenPolicy(t *testing.T) {
	e := openEngine(t, openStore(t))
	e.broken = "broken"
	e.SetOff(true)
	if r, err := e.Current(); r != nil || err != nil {
		t.Fatalf("Current while off = %v, %v", r, err)
	}
	e.SetOff(false)
	if _, err := e.Current(); !errors.Is(err, ErrBroken) {
		t.Fatalf("Current once on = %v, want ErrBroken", err)
	}
	if _, _, err := e.CurrentRevision(); !errors.Is(err, ErrBroken) {
		t.Fatalf("CurrentRevision once on = %v, want ErrBroken", err)
	}
}

// TestEnginePolicyKept: only an applied policy with spaces, or one that does
// not verify, is kept.
func TestEnginePolicyKept(t *testing.T) {
	e := openEngine(t, openStore(t))
	if e.PolicyKept() {
		t.Fatal("an empty store keeps a policy")
	}
	e.broken = "broken"
	if !e.PolicyKept() {
		t.Fatal("a broken apply is not kept")
	}
}

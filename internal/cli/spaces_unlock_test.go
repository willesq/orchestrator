package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func resetUnlockFake(t *testing.T) *[][]string {
	t.Helper()
	unlockFake.request = &orchdv1.RequestUnlockResponse{Unlock: &orchdv1.OperatorUnlock{Id: "u1", State: "confirmed"}}
	unlockFake.requestErr, unlockFake.requestReq = nil, nil
	unlockFake.proveErrs, unlockFake.proveReqs = nil, nil
	tty := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(tty, []byte(" 482-913 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTTY := unlockTTY
	unlockTTY = tty
	t.Cleanup(func() { unlockTTY = oldTTY })
	unlockFake.lock, unlockFake.lockErr, unlockFake.lockReqs = &orchdv1.LockResponse{Ended: 1}, nil, nil
	var ran [][]string
	old := runUnlocked
	runUnlocked = func(argv []string, _ io.Reader, _, _ io.Writer) (int, error) {
		ran = append(ran, argv)
		return 0, nil
	}
	t.Cleanup(func() { runUnlocked = old })
	return &ran
}

// TestSpacesUnlockRunsTheShellThenLocks: once orchd confirms, the shell (or
// the command after --) runs, and the unlock is ended when it exits.
func TestSpacesUnlockRunsTheShellThenLocks(t *testing.T) {
	ran := resetUnlockFake(t)
	t.Setenv("SHELL", "/bin/zsh")
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock", "--for", "30m")
	if err != nil {
		t.Fatal(err)
	}
	if unlockFake.requestReq.GetForSeconds() != 1800 {
		t.Fatalf("asked for %ds", unlockFake.requestReq.GetForSeconds())
	}
	if len(unlockFake.proveReqs) != 1 || unlockFake.proveReqs[0].GetId() != "u1" || unlockFake.proveReqs[0].GetCode() != "482-913" {
		t.Fatalf("proved %v, want u1 with the typed code", unlockFake.proveReqs)
	}
	if len(*ran) != 1 || !slices.Equal((*ran)[0], []string{"/bin/zsh"}) {
		t.Fatalf("ran %v, want the shell", *ran)
	}
	if len(unlockFake.lockReqs) != 1 || unlockFake.lockReqs[0].GetId() != "u1" {
		t.Fatalf("locked %v, want u1 once", unlockFake.lockReqs)
	}
	for _, want := range []string{"Confirm it in Orch.app", "Type the code Orch.app shows", "Unlocked until", "Locked."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	*ran = nil
	unlockFake.lockReqs = nil
	t.Setenv("SHELL", "")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock", "--", "orch", "rag", "relabel"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal((*ran)[0], []string{"orch", "rag", "relabel"}) || unlockFake.requestReq.GetForSeconds() != 900 {
		t.Fatalf("ran %v for %ds, want the command for the default 15m", *ran, unlockFake.requestReq.GetForSeconds())
	}
	if !slices.Equal(unlockShell(), []string{"/bin/sh"}) {
		t.Fatalf("with no $SHELL the shell is %v", unlockShell())
	}
}

// TestSpacesUnlockFailures: a bad duration, a command not after --, a
// refusal or denial, a command that fails and a lock that fails.
func TestSpacesUnlockFailures(t *testing.T) {
	ran := resetUnlockFake(t)
	for _, args := range [][]string{
		{"spaces", "unlock", "--for", "61m"},
		{"spaces", "unlock", "--for", "0s"},
		{"spaces", "unlock", "orch", "rag", "relabel"},
	} {
		if _, err := run(factory(&fakeClient{}, nil), "", args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
	if _, err := run(factory(nil, errors.New("no orchd")), "", "spaces", "unlock"); err == nil || !strings.Contains(err.Error(), "no orchd") {
		t.Errorf("dial failure = %v", err)
	}
	// A wrong code is asked for again; orchd's refusal after the last
	// try ends it, and nothing runs.
	unlockFake.proveErrs = []error{
		grpcstatus.Error(codes.InvalidArgument, "that is not the code Orch.app shows (2 of 3 tries left)"),
		grpcstatus.Error(codes.PermissionDenied, "the request is dropped"),
	}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock")
	if err == nil || !strings.Contains(err.Error(), "dropped") || !strings.Contains(out, "2 of 3 tries left") || len(unlockFake.proveReqs) != 2 {
		t.Errorf("wrong codes = %v\n%s", err, out)
	}
	unlockFake.proveErrs, unlockFake.proveReqs = nil, nil
	// The code is read from the terminal only.
	unlockTTY = filepath.Join(t.TempDir(), "no-tty")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock"); err == nil || !strings.Contains(err.Error(), "typed at a terminal") {
		t.Errorf("no terminal = %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unlockTTY = empty
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock"); err == nil || !strings.Contains(err.Error(), "reading the code") {
		t.Errorf("empty terminal = %v", err)
	}
	if len(*ran) != 0 {
		t.Fatalf("ran %v without an unlock", *ran)
	}
	unlockFake.requestErr = grpcstatus.Error(codes.PermissionDenied, "the unlock was denied in Orch.app")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock"); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("denied unlock = %v", err)
	}
	if len(*ran) != 0 || len(unlockFake.lockReqs) != 0 {
		t.Fatalf("something ran without an unlock: %v %v", *ran, unlockFake.lockReqs)
	}
	unlockFake.requestErr = nil
	resetUnlockFake(t)

	runUnlocked = func([]string, io.Reader, io.Writer, io.Writer) (int, error) { return 3, nil }
	unlockFake.lockErr = errors.New("orchd went away")
	out, err = run(factory(&fakeClient{}, nil), "", "spaces", "unlock", "--", "false")
	if err == nil || !strings.Contains(err.Error(), "status 3") || !strings.Contains(out, "could not be ended") {
		t.Errorf("failing command = %v\n%s", err, out)
	}
	runUnlocked = func([]string, io.Reader, io.Writer, io.Writer) (int, error) { return 0, errors.New("no such file") }
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "unlock", "--", "nope"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("command that cannot start = %v", err)
	}
}

// TestRunUnlockedReportsTheExitStatus runs real commands.
func TestRunUnlockedReportsTheExitStatus(t *testing.T) {
	var out strings.Builder
	code, err := runUnlocked([]string{"sh", "-c", "echo hi; exit 3"}, strings.NewReader(""), &out, io.Discard)
	if err != nil || code != 3 || out.String() != "hi\n" {
		t.Fatalf("code %d, err %v, out %q", code, err, out.String())
	}
	if _, err := runUnlocked([]string{"/nonexistent/orch-sp077"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("a missing program ran")
	}
}

func TestSpacesLock(t *testing.T) {
	resetUnlockFake(t)
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "lock")
	if err != nil || !strings.Contains(out, "Ended 1 unlock(s).") || unlockFake.lockReqs[0].GetId() != "" {
		t.Fatalf("lock = %q, %v", out, err)
	}
	unlockFake.lock = &orchdv1.LockResponse{}
	if out, _ := run(factory(&fakeClient{}, nil), "", "spaces", "lock"); !strings.Contains(out, "No unlock covers") {
		t.Fatalf("lock with nothing = %q", out)
	}
	unlockFake.lockErr = errors.New("refused")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "lock"); err == nil {
		t.Fatal("a refused lock succeeded")
	}
}

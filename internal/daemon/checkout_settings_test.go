package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// checkoutWorld is orchd with a spaces store under a temp home holding two
// job roots, code/a and code/b.
func checkoutWorld(t *testing.T) (*server, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	e, err := spaces.Open(context.Background(), conn, spaces.Env{Home: home, InGitTree: func(string) bool { return false }, LookupOrg: orgNodes{}.lookup})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{spaces: e, enforcement: newEnforcement(context.Background(), conn, nil), checkouts: newCheckoutSettings()}
	// Reason: the default temp root is the real /tmp/orch-<uid>; keep the
	// test's spaces out of it.
	tmp, err := os.MkdirTemp("/tmp", "oe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	s.checkouts.syncer.Layout.Tmp = filepath.Join(tmp, "r")
	s.enforcement.onSandbox = s.checkouts.nudge
	return s, home
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

func kicked(c *checkoutSettings) bool {
	select {
	case <-c.kick:
		return true
	default:
		return false
	}
}

// TestCheckoutSettingsFollowPolicyAndSwitch: orchd writes nothing without
// a policy, writes every checkout's file once one is applied (an apply asks
// for a sync at once), and takes its entries out when the sandbox layer is
// switched off (which also asks for a sync).
func TestCheckoutSettingsFollowPolicyAndSwitch(t *testing.T) {
	s, home := checkoutWorld(t)
	ctx := context.Background()
	repo := filepath.Join(home, "code", "a", "app")
	initRepo(t, repo)
	initRepo(t, filepath.Join(home, "code", "b", "svc"))

	if rep := s.syncCheckoutSettings(ctx); rep.Checkouts != 0 || len(rep.Written) != 0 {
		t.Fatalf("no policy: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(home, "data", "spaces", ".orch", "checkouts.json")); !os.IsNotExist(err) {
		t.Errorf("state file without a policy: %v", err)
	}

	doc := fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q]},{"name":"jobb","kind":"isolated","roots":[%q]}]}`,
		filepath.Join(home, "code", "a"), filepath.Join(home, "code", "b"))
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	if !kicked(s.checkouts) {
		t.Error("apply did not ask for a sync")
	}
	rep := s.syncCheckoutSettings(ctx)
	if rep.Checkouts != 2 || len(rep.Written) != 2 || len(rep.Problems) != 0 {
		t.Fatalf("applied: %+v", rep)
	}
	local := filepath.Join(repo, ".claude", "settings.local.json")
	b, err := os.ReadFile(local)
	if err != nil || !strings.Contains(string(b), "Read(/"+filepath.Join(home, "code", "b")+"/**)") {
		t.Fatalf("settings: %s %v", b, err)
	}
	if strings.Contains(string(b), `"`+filepath.Join(home, "code", "a")+`"`) {
		t.Errorf("job a's checkout denies job a:\n%s", b)
	}
	if info, err := os.Stat(s.checkouts.syncer.Layout.TmpDir("joba")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("joba tmp: %v %v", info, err)
	}

	if _, _, err := s.enforcement.set(ctx, spaces.LayerSandbox, spaces.SwitchOff, "test", 0); err != nil {
		t.Fatal(err)
	}
	if !kicked(s.checkouts) {
		t.Error("switch did not ask for a sync")
	}
	if rep := s.syncCheckoutSettings(ctx); len(rep.Cleared) != 2 {
		t.Fatalf("off: %+v", rep)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Errorf("orch's file left after off: %v", err)
	}
	if _, _, err := s.enforcement.set(ctx, spaces.LayerSandbox, spaces.SwitchOn, "test", 0); err != nil {
		t.Fatal(err)
	}
	if rep := s.syncCheckoutSettings(ctx); len(rep.Written) != 2 {
		t.Fatalf("on: %+v", rep)
	}
}

// TestCheckoutSettingsLoop: the loop syncs at once, again on a nudge, and
// stops with its context; a nil runner or store does nothing.
func TestCheckoutSettingsLoop(t *testing.T) {
	s, home := checkoutWorld(t)
	doc := fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q]}]}`, filepath.Join(home, "code", "a"))
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	// Reason: with one isolated space and nothing else, the default setting
	// (auto) writes no rules (D-61).
	if _, err := s.enforcement.setSandbox(context.Background(), spaces.SandboxOn, "test"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runCheckoutSettings(ctx, time.Hour); close(done) }()
	repo := filepath.Join(home, "code", "a", "late")
	initRepo(t, repo)
	local := filepath.Join(repo, ".claude", "settings.local.json")
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.checkouts.nudge()
		if _, err := os.Stat(local); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the nudged loop never wrote the new checkout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	var none *checkoutSettings
	none.nudge()
	if rep := (&server{}).syncCheckoutSettings(context.Background()); rep.Checkouts != 0 {
		t.Errorf("no runner: %+v", rep)
	}
}

// TestCheckoutSettingsProblemsLoggedOnce: the same problems are not logged
// at every sync.
func TestCheckoutSettingsProblemsLoggedOnce(t *testing.T) {
	c := newCheckoutSettings()
	c.logProblems([]string{"x"})
	c.logProblems([]string{"x"})
	if !slices.Equal(c.problems, []string{"x"}) {
		t.Errorf("problems = %v", c.problems)
	}
	c.logProblems(nil)
	if c.problems != nil {
		t.Errorf("problems = %v", c.problems)
	}
}

// TestPlaceCheckoutByRootAndRemote: orchd places a checkout as it places
// content from it; a checkout outside every root has no space.
func TestPlaceCheckoutByRootAndRemote(t *testing.T) {
	s, home := checkoutWorld(t)
	doc := fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q]}]}`, filepath.Join(home, "code", "a"))
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	space, placed, err := s.placeCheckout(context.Background(), filepath.Join(home, "code", "a", "x"))
	if err != nil || !placed || space != "joba" {
		t.Errorf("in root: %q %v %v", space, placed, err)
	}
	if _, placed, err := s.placeCheckout(context.Background(), filepath.Join(home, "elsewhere")); err != nil || placed {
		t.Errorf("outside: %v %v", placed, err)
	}
	if _, _, err := s.placeCheckout(context.Background(), "relative"); err == nil {
		t.Error("relative path placed")
	}
}

// TestPlaceCheckoutIgnoresStricterRemote: a checkout gets a space's rules
// only when its folder root placed it; a remote the session can rewrite
// cannot move a checkout under personal into a job's space.
func TestPlaceCheckoutIgnoresStricterRemote(t *testing.T) {
	s, home := checkoutWorld(t)
	doc := fmt.Sprintf(`{"spaces":[{"name":"personal","kind":"private","roots":[%q]},{"name":"jobb","kind":"isolated","github_orgs":["globex"],"roots":[%q]}]}`,
		filepath.Join(home, "code", "me"), filepath.Join(home, "code", "b"))
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	withRemote := func(dir string) string {
		initRepo(t, dir)
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/globex/x.git").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return dir
	}
	mine := withRemote(filepath.Join(home, "code", "me", "looks-like-b"))
	// Content from it is placed in jobb by its remote...
	if e, err := s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: mine}); err != nil || e.GetRule() != string(spaces.PlaceRemoteStricter) || e.GetSpace() != "jobb" {
		t.Fatalf("placement %v %v", e, err)
	}
	// ...but its deny rules are not jobb's.
	if space, placed, err := s.placeCheckout(context.Background(), mine); err != nil || placed {
		t.Errorf("remote raised the checkout to %q (placed %v, %v)", space, placed, err)
	}
	agree := withRemote(filepath.Join(home, "code", "b", "real"))
	if space, placed, err := s.placeCheckout(context.Background(), agree); err != nil || !placed || space != "jobb" {
		t.Errorf("root and remote agree: %q %v %v", space, placed, err)
	}
}

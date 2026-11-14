package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// sandboxRig is checkoutWorld with a fake clock and the emitted changes.
func sandboxRig(t *testing.T) (*server, string, *fakeClock, *[]spaces.SwitchChange) {
	t.Helper()
	s, home := checkoutWorld(t)
	clock := &fakeClock{t: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	emitted := &[]spaces.SwitchChange{}
	s.enforcement.now = clock.now
	s.enforcement.emit = func(c spaces.SwitchChange, _ []string) { *emitted = append(*emitted, c) }
	return s, home, clock, emitted
}

func sandboxOf(t *testing.T, s *server, ctx context.Context) *orchdv1.SandboxSetting {
	t.Helper()
	rep, err := s.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return rep.GetSandboxSetting()
}

// TestSandboxSettingOperatorOnly is SP-046.4.a1: the setting is auto by
// default, stored in orch.db (it survives a restart), shown by GetEnforcement
// and Status (orch status), changed only by the operator, and every change is
// logged with its time and caller.
func TestSandboxSettingOperatorOnly(t *testing.T) {
	s, _, clock, emitted := sandboxRig(t)
	ctx := context.Background()
	if sb := sandboxOf(t, s, operatorCtx); sb.GetMode() != "auto" || sb.GetDefaultMode() != "auto" || sb.GetSetAt() != 0 ||
		strings.Join(sb.GetModes(), ",") != "auto,on,off" {
		t.Fatalf("default: %+v", sb)
	}
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	for name, c := range map[string]context.Context{
		"claude session": callerCtx(callerAuth{pid: 7, harness: "claude"}),
		"codex session":  callerCtx(callerAuth{pid: 7, harness: "codex"}),
		"orchd's child":  callerCtx(callerAuth{pid: 7, spawned: true}),
		"queued task":    queued,
		"unreadable":     callerCtx(callerAuth{pid: 7, harnessErr: errors.New("gone")}),
		"no peer":        context.Background(),
	} {
		if _, err := s.SetSandboxSetting(c, &orchdv1.SetSandboxSettingRequest{Mode: "off"}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: SetSandboxSetting = %v, want PermissionDenied", name, err)
		}
	}
	if sandboxOf(t, s, operatorCtx).GetMode() != "auto" || len(*emitted) != 0 {
		t.Fatalf("a refused change took effect: %v", *emitted)
	}
	if _, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "sometimes"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad mode: %v", err)
	}

	_ = kicked(s.checkouts)
	rep, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "OFF"})
	if err != nil || !rep.GetChanged() || rep.GetCurrent().GetMode() != "off" || rep.GetCurrent().GetSetBy() != "operator (process 42)" {
		t.Fatalf("set off: %+v %v", rep, err)
	}
	if !kicked(s.checkouts) {
		t.Error("the change did not ask for a checkout sync")
	}
	if len(*emitted) != 1 || (*emitted)[0].State != "off" || (*emitted)[0].Previous != "auto" || (*emitted)[0].By != "operator (process 42)" {
		t.Errorf("emitted %+v", *emitted)
	}
	if rep, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "off"}); err != nil || rep.GetChanged() {
		t.Errorf("setting the same mode: %+v %v", rep, err)
	}
	clock.advance(time.Minute)
	if _, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "on"}); err != nil {
		t.Fatal(err)
	}
	hist, err := spaces.SandboxSettingHistory(ctx, s.enforcement.db, 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if h := hist[0]; h.State != "on" || h.Previous != "off" || h.By != "operator (process 42)" || !h.At.Equal(clock.now()) {
		t.Errorf("newest change: %+v", h)
	}
	if h := hist[1]; h.State != "off" || !h.At.Equal(clock.now().Add(-time.Minute)) {
		t.Errorf("first change: %+v", h)
	}

	// Everyone may read it (every session's digest asks GetEnforcement);
	// who set it is the operator's to see.
	if sb := sandboxOf(t, s, inClaude); sb.GetMode() != "on" || sb.GetSetBy() != "" || sb.GetSetAt() != clock.now().Unix() {
		t.Errorf("as a session: %+v", sb)
	}
	if sb := s.spacesStatus(operatorCtx).GetSandbox(); sb.GetMode() != "on" || sb.GetSetBy() == "" {
		t.Errorf("status: %+v", sb)
	}

	// orchd restarts: the setting comes back from orch.db.
	again := newEnforcement(ctx, s.enforcement.db, nil)
	if set, problem := again.sandboxSetting(ctx); set.Mode != spaces.SandboxOn || problem != "" || set.SetBy != "operator (process 42)" {
		t.Errorf("after a restart: %+v %q", set, problem)
	}
}

// TestSandboxSettingChangedOutsideOrchd: a setting row written outside orchd
// is drift; orchd keeps its own, and a restart over it writes the rules (on).
func TestSandboxSettingChangedOutsideOrchd(t *testing.T) {
	s, _, _, _ := sandboxRig(t)
	ctx := context.Background()
	if _, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "on"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enforcement.db.ExecContext(ctx, `UPDATE spaces_enforcement SET state = 'off' WHERE layer = 'sandbox-setting'`); err != nil {
		t.Fatal(err)
	}
	sb := sandboxOf(t, s, operatorCtx)
	if sb.GetMode() != "on" || !strings.Contains(sb.GetProblem(), "changed outside orchd") {
		t.Errorf("drift: %+v", sb)
	}
	if s.enforcement.choice().Setting != spaces.SandboxOn {
		t.Error("orchd took the edited setting")
	}
	restarted := newEnforcement(ctx, s.enforcement.db, nil)
	if set, problem := restarted.sandboxSetting(ctx); set.Mode != spaces.SandboxOn || !strings.Contains(problem, "does not match") {
		t.Errorf("restart over an edited row: %+v %q", set, problem)
	}
	// Setting it again writes a matching row and clears the problem.
	if _, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	if p := sandboxOf(t, s, operatorCtx).GetProblem(); p != "" {
		t.Errorf("problem after a set: %q", p)
	}
	var none *enforcement
	if set, p := none.sandboxSetting(ctx); set.Mode != spaces.SandboxAuto || p != "" || none.choice() != spacesettings.ChoiceAuto {
		t.Errorf("no store: %+v %q", set, p)
	}
	if _, err := (&server{}).SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: "on"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no store: %v", err)
	}
}

// TestSandboxSettingDecidesCheckouts is SP-046.4.a2 in orchd: with auto a
// checkout gets the sandbox rules only when another isolated space's folders
// would be denied; with off none and with on always; neither the setting nor
// auto with nothing to deny marks start digests, while a timed switch to off
// does (INV-16 as amended by D-61).
func TestSandboxSettingDecidesCheckouts(t *testing.T) {
	s, home, clock, _ := sandboxRig(t)
	ctx := context.Background()
	a, p := filepath.Join(home, "code", "a", "app"), filepath.Join(home, "code", "p", "lib")
	initRepo(t, a)
	initRepo(t, p)
	apply := func(doc string) {
		t.Helper()
		d := draft(t, s, doc)
		if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
			t.Fatal(err)
		}
	}
	setMode := func(m string) {
		t.Helper()
		if _, err := s.SetSandboxSetting(operatorCtx, &orchdv1.SetSandboxSettingRequest{Mode: m}); err != nil {
			t.Fatal(err)
		}
	}
	local := func(c string) string { return filepath.Join(c, ".claude", "settings.local.json") }
	noDigest := func(when string) {
		t.Helper()
		for _, sw := range switchesOf(t, s, inClaude) {
			if sw.GetRelaxed() {
				t.Errorf("%s: the digest would name %s %s", when, sw.GetLayer(), sw.GetState())
			}
		}
		if r := s.spacesStatus(operatorCtx).GetRelaxed(); len(r) != 0 {
			t.Errorf("%s: orch status names relaxed layers %v", when, r)
		}
	}

	// One isolated job and a public space: nothing to keep job a out of.
	apply(fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q]},{"name":"oss","kind":"public","roots":[%q]}]}`,
		filepath.Join(home, "code", "a"), filepath.Join(home, "code", "p")))
	rep := s.syncCheckoutSettings(ctx)
	if rep.Checkouts != 2 || len(rep.Written) != 1 || rep.Written[0] != p || len(rep.Problems) != 0 {
		t.Fatalf("auto, one isolated job: %+v", rep)
	}
	if _, err := os.Stat(local(a)); !os.IsNotExist(err) {
		t.Errorf("auto wrote job a's checkout with nothing to deny: %v", err)
	}
	if b, err := os.ReadFile(local(p)); err != nil || !strings.Contains(string(b), "sandbox") {
		// The public space's sessions are kept out of job a's root.
		t.Errorf("auto left the public checkout without rules: %s %v", b, err)
	}
	if sb := s.spacesStatus(operatorCtx).GetSandbox(); sb.GetMode() != "auto" || !sb.GetActive() {
		t.Errorf("status with the public checkout sandboxed: %+v", sb)
	}
	noDigest("auto")

	// Only job a: auto writes nothing anywhere and says so.
	apply(fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q]}]}`, filepath.Join(home, "code", "a")))
	if rep := s.syncCheckoutSettings(ctx); len(rep.Written) != 0 || len(rep.Cleared) != 1 {
		t.Fatalf("auto, only job a: %+v", rep)
	}
	if sb := s.spacesStatus(operatorCtx).GetSandbox(); sb.GetActive() || !strings.Contains(sb.GetDetail(), "no other isolated space") {
		t.Errorf("status, nothing to deny: %+v", sb)
	}
	noDigest("auto with nothing to deny")

	setMode("on")
	if rep := s.syncCheckoutSettings(ctx); len(rep.Written) != 1 {
		t.Fatalf("on: %+v", rep)
	}
	if b, err := os.ReadFile(local(a)); err != nil || !strings.Contains(string(b), `"enabled": true`) {
		t.Errorf("on: %s %v", b, err)
	}
	if sb := s.spacesStatus(operatorCtx).GetSandbox(); !sb.GetActive() || !strings.HasPrefix(sb.GetDetail(), "on:") {
		t.Errorf("status on: %+v", sb)
	}

	setMode("off")
	if rep := s.syncCheckoutSettings(ctx); len(rep.Cleared) != 1 {
		t.Fatalf("off: %+v", rep)
	}
	if _, err := os.Stat(local(a)); !os.IsNotExist(err) {
		t.Errorf("off left orch's file: %v", err)
	}
	if sb := s.spacesStatus(operatorCtx).GetSandbox(); sb.GetActive() || !strings.Contains(sb.GetDetail(), "off by choice") {
		t.Errorf("status off: %+v", sb)
	}
	noDigest("off")

	// A timed switch to off for debugging still marks every digest.
	setMode("on")
	if _, _, err := s.enforcement.set(ctx, spaces.LayerSandbox, spaces.SwitchOff, "operator (process 42)", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if sw := switchesOf(t, s, inClaude)["sandbox"]; !sw.GetRelaxed() || sw.GetUntil() == 0 {
		t.Errorf("timed switch off is not relaxed: %+v", sw)
	}
	if sb := s.spacesStatus(operatorCtx).GetSandbox(); sb.GetActive() || !strings.Contains(sb.GetDetail(), "switch is off") {
		t.Errorf("status under the switch: %+v", sb)
	}
	if rep := s.syncCheckoutSettings(ctx); len(rep.Written) != 0 {
		t.Fatalf("switch off: %+v", rep)
	}
	clock.advance(16 * time.Minute)
	if rep := s.syncCheckoutSettings(ctx); len(rep.Written) != 1 {
		t.Fatalf("after the timed switch: %+v", rep)
	}
	noDigest("after the timed switch")
}

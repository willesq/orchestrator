package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/safetynet/fakeclaude"
)

// blockingRunner holds every command until released, so a test can observe
// an action while it runs.
type blockingRunner struct {
	inner   safetynet.Runner
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRunner) Run(ctx context.Context, args []string) (string, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.inner.Run(ctx, args)
}

func safetyNetServer(t *testing.T, r safetynet.Runner) (*server, safetynet.Locations) {
	t.Helper()
	loc := safetynet.Locate(t.TempDir())
	if r == nil {
		r = fakeclaude.New(loc.ClaudeDir)
	}
	return &server{safetyNet: newSafetyNetManager(loc, r, t.TempDir())}, loc
}

func TestGetSafetyNet(t *testing.T) {
	if _, err := (&server{}).GetSafetyNet(context.Background(), &orchdv1.GetSafetyNetRequest{}); code(err) != codes.Unavailable {
		t.Errorf("nil manager err = %v", err)
	}
	if _, err := (&server{}).InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{}); code(err) != codes.Unavailable {
		t.Errorf("nil manager install err = %v", err)
	}
	s, _ := safetyNetServer(t, nil)
	st, err := s.GetSafetyNet(context.Background(), &orchdv1.GetSafetyNetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetState() != "missing" || st.GetOk() || st.GetPinnedVersion() != safetynet.Pinned.Version ||
		st.GetRules().GetOk() || len(st.GetRules().GetProblems()) == 0 || st.GetAction().GetStartedAt() != 0 {
		t.Errorf("state %+v", st)
	}
}

func TestInstallSafetyNet(t *testing.T) {
	s, loc := safetyNetServer(t, nil)
	st, err := s.InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	a := st.GetAction()
	if !st.GetOk() || st.GetState() != "ok" || st.GetInstalledVersion() != safetynet.Pinned.Version ||
		a.GetRunning() || !a.GetOk() || a.GetFinishedAt() == 0 || !strings.Contains(a.GetLog(), "claude plugin install") {
		t.Fatalf("state %+v", st)
	}
	var orch *orchdv1.SafetyNetRulebook
	for _, rb := range st.GetRules().GetRulebooks() {
		if rb.GetName() == safetynet.RulebookName {
			orch = rb
		}
	}
	if orch == nil || orch.GetOwner() != "orch" || len(orch.GetRules()) == 0 || orch.GetRules()[0].GetReason() == "" {
		t.Errorf("orch-guards %+v", orch)
	}

	// A disabled plugin is re-enabled by the explicit action.
	fake := fakeclaude.New(loc.ClaudeDir)
	s.safetyNet.runner = fake
	if _, err := fake.Run(context.Background(), []string{"plugin", "disable", safetynet.PluginID}); err != nil {
		t.Fatal(err)
	}
	if got := safetynet.Detect(loc).Plugin; got != safetynet.StateDisabled {
		t.Fatalf("setup: %s", got)
	}
	st, _ = s.InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{})
	if st.GetState() != "ok" {
		t.Errorf("not re-enabled: %s", st.GetState())
	}
}

func TestInstallSafetyNetFailureIsReported(t *testing.T) {
	loc := safetynet.Locate(t.TempDir())
	fake := fakeclaude.New(loc.ClaudeDir)
	fake.Fail = map[string]error{"plugin install": errors.New("exit status 1")}
	s := &server{safetyNet: newSafetyNetManager(loc, fake, t.TempDir())}
	st, err := s.InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{})
	if err != nil {
		t.Fatalf("a failed action is a state, not an RPC error: %v", err)
	}
	if a := st.GetAction(); a.GetOk() || !strings.Contains(a.GetError(), "exit status 1") || st.GetState() != "missing" {
		t.Errorf("state %+v", st)
	}
	// Rules-only still repairs the rules without any plugin command.
	before := len(fake.Calls())
	st, _ = s.InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{RulesOnly: true})
	if !st.GetRules().GetOk() || !st.GetAction().GetRulesOnly() || len(fake.Calls()) != before {
		t.Errorf("rules-only %+v, calls %v", st, fake.Calls())
	}
}

func TestInstallSafetyNetIsSingleFlight(t *testing.T) {
	loc := safetynet.Locate(t.TempDir())
	b := &blockingRunner{inner: fakeclaude.New(loc.ClaudeDir), started: make(chan struct{}), release: make(chan struct{})}
	s := &server{safetyNet: newSafetyNetManager(loc, b, t.TempDir())}
	done := make(chan error, 1)
	go func() {
		_, err := s.InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{})
		done <- err
	}()
	<-b.started
	st, err := s.GetSafetyNet(context.Background(), &orchdv1.GetSafetyNetRequest{})
	if err != nil || !st.GetAction().GetRunning() || !strings.Contains(st.GetAction().GetStep(), "orch marketplace") {
		t.Errorf("while running: %+v %v", st.GetAction(), err)
	}
	_, err = s.InstallSafetyNet(context.Background(), &orchdv1.InstallSafetyNetRequest{RulesOnly: true})
	if code(err) != codes.Aborted || !strings.Contains(err.Error(), "not queued") {
		t.Errorf("concurrent request err = %v", err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetSafetyNet(context.Background(), &orchdv1.GetSafetyNetRequest{}); st.GetAction().GetRunning() || !st.GetOk() {
		t.Errorf("after: %+v", st)
	}
}

func TestInstallSafetyNetOutlivesItsClient(t *testing.T) {
	loc := safetynet.Locate(t.TempDir())
	b := &blockingRunner{inner: fakeclaude.New(loc.ClaudeDir), started: make(chan struct{}), release: make(chan struct{})}
	s := &server{safetyNet: newSafetyNetManager(loc, b, t.TempDir())}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = s.InstallSafetyNet(ctx, &orchdv1.InstallSafetyNetRequest{})
		close(done)
	}()
	<-b.started
	cancel() // the client gives up mid-action
	close(b.release)
	<-done
	if st, _ := s.GetSafetyNet(context.Background(), &orchdv1.GetSafetyNetRequest{}); !st.GetOk() || !st.GetAction().GetOk() {
		t.Errorf("action stopped with its client: %+v", st.GetAction())
	}
}

func TestSafetyNetLogIsCapped(t *testing.T) {
	m := newSafetyNetManager(safetynet.Locations{}, nil, "")
	chunk := []byte(strings.Repeat("x", 1024) + "\n")
	for range 40 {
		if n, err := m.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write %d %v", n, err)
		}
	}
	if got := len(m.snapshot().GetLog()); got != maxSafetyNetLog {
		t.Errorf("log %d bytes, want %d", got, maxSafetyNetLog)
	}
}

func TestSafetyNetFor(t *testing.T) {
	m := safetyNetFor(config.Settings{})
	if m == nil || m.loc.Home == "" || m.backupDir == "" {
		t.Fatalf("manager %+v", m)
	}
	r, ok := m.runner.(safetynet.ExecRunner)
	if !ok || !strings.Contains(strings.Join(r.DropEnv, ","), "QDRANT_API_KEY") {
		t.Errorf("runner %+v", m.runner)
	}
	var s config.Settings
	s.Queue.Worker.ClaudeBin = "/opt/claude"
	if r := safetyNetFor(s).runner.(safetynet.ExecRunner); r.Bin != "/opt/claude" {
		t.Errorf("configured claude not used: %q", r.Bin)
	}
}

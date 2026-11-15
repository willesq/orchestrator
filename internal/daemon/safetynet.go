package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/safetynet"
)

// maxSafetyNetLog caps the action log kept for GetSafetyNet.
const maxSafetyNetLog = 16 << 10

// safetyNetManager runs safety-net install actions one at a time and keeps
// the last one's result for GetSafetyNet.
type safetyNetManager struct {
	loc       safetynet.Locations
	runner    safetynet.Runner
	backupDir string

	mu     sync.Mutex
	action *orchdv1.SafetyNetAction
	log    strings.Builder
}

// safetyNetFor builds the manager for the user's home, running the claude
// CLI queued tasks use, with the credentials orchd holds withheld.
func safetyNetFor(s config.Settings) *safetyNetManager {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	loc := safetynet.Locate(home)
	runner := safetynet.ExecRunner{Bin: s.Queue.Worker.ClaudeBin, Loc: loc, DropEnv: secretEnv(s)}
	return newSafetyNetManager(loc, runner, filepath.Join(paths.DataDir(), "backups"))
}

// safetyNetActionTimeout bounds a whole install action; each command inside
// it is bounded on its own too.
const safetyNetActionTimeout = 10 * time.Minute

func newSafetyNetManager(loc safetynet.Locations, runner safetynet.Runner, backupDir string) *safetyNetManager {
	return &safetyNetManager{loc: loc, runner: runner, backupDir: backupDir, action: &orchdv1.SafetyNetAction{}}
}

// snapshot copies the action under the lock.
func (m *safetyNetManager) snapshot() *orchdv1.SafetyNetAction {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.action
	return &orchdv1.SafetyNetAction{
		Running: a.Running, RulesOnly: a.RulesOnly, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
		Step: a.Step, Ok: a.Ok, Error: a.Error, Log: m.log.String(),
	}
}

// begin claims the single action slot, or reports the running action.
func (m *safetyNetManager) begin(rulesOnly bool) (bool, *orchdv1.SafetyNetAction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.action.Running {
		return false, &orchdv1.SafetyNetAction{Step: m.action.Step, StartedAt: m.action.StartedAt}
	}
	m.action = &orchdv1.SafetyNetAction{Running: true, RulesOnly: rulesOnly, StartedAt: time.Now().Unix(), Step: "starting"}
	m.log.Reset()
	return true, nil
}

// Write records install output; a line starting a file write or a step
// becomes the current step, which the app shows as progress.
func (m *safetyNetManager) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		switch {
		case strings.HasPrefix(line, "run safety-net: "):
			m.action.Step = strings.TrimPrefix(line, "run safety-net: ")
		case strings.HasPrefix(line, "wrote "):
			m.action.Step = line
		}
	}
	if room := maxSafetyNetLog - m.log.Len(); room > 0 {
		m.log.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (m *safetyNetManager) finish(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.action.Running = false
	m.action.FinishedAt = time.Now().Unix()
	m.action.Ok = err == nil
	if err != nil {
		m.action.Error = err.Error()
	}
}

// run performs one action. It is detached from the request: a client that
// gives up mid-migration (after removing a marketplace, before installing
// from the new one) must not leave Claude Code with no safety net, so the
// action finishes on its own deadline and GetSafetyNet reports how it ended.
func (m *safetyNetManager) run(ctx context.Context, rulesOnly bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), safetyNetActionTimeout)
	defer cancel()
	plan := install.SafetyNetPlan(m.loc, install.SafetyNetOptions{
		RulesOnly: rulesOnly,
		// Reason: this is an explicit click, so a disabled plugin is turned
		// back on; `orch install` leaves that choice alone.
		Plugin: safetynet.PlanOptions{EnableDisabled: true},
		Runner: m.runner,
	})
	return plan.ApplyContext(ctx, m, m.backupDir)
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_GetSafetyNet_FullMethodName,
	spaceDecision{spaceNone, "cc-safety-net's install state"}))

func (s *server) GetSafetyNet(context.Context, *orchdv1.GetSafetyNetRequest) (*orchdv1.SafetyNetState, error) {
	if s.safetyNet == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "safety-net management unavailable")
	}
	return safetyNetState(safetynet.Detect(s.safetyNet.loc), s.safetyNet.snapshot()), nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_InstallSafetyNet_FullMethodName, "install or upgrade cc-safety-net",
	spaceDecision{spaceNone, "installs cc-safety-net"}))

func (s *server) InstallSafetyNet(ctx context.Context, req *orchdv1.InstallSafetyNetRequest) (*orchdv1.SafetyNetState, error) {
	m := s.safetyNet
	if m == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "safety-net management unavailable")
	}
	ok, running := m.begin(req.GetRulesOnly())
	if !ok {
		return nil, grpcstatus.Errorf(codes.Aborted,
			"a safety-net install is already running (%s, started %s ago); this request was not queued",
			running.GetStep(), time.Since(time.Unix(running.GetStartedAt(), 0)).Round(time.Second))
	}
	log.Printf("safety-net: install requested (rules only %v)", req.GetRulesOnly())
	err := m.run(ctx, req.GetRulesOnly())
	m.finish(err)
	if err != nil {
		log.Printf("safety-net: install failed: %v", err)
	} else {
		log.Printf("safety-net: install finished")
	}
	// Reason: a failed action is still reported as a state, not an RPC error,
	// so the caller sees what was installed alongside the error and log.
	return safetyNetState(safetynet.Detect(m.loc), m.snapshot()), nil
}

// safetyNetState converts detection results to the RPC message.
func safetyNetState(st safetynet.State, action *orchdv1.SafetyNetAction) *orchdv1.SafetyNetState {
	out := &orchdv1.SafetyNetState{
		State: string(st.Plugin), Ok: st.OK(),
		PinnedVersion: st.Pinned.Version, PinnedSha: st.Pinned.SHA, PinnedReleasedOn: st.Pinned.ReleasedOn,
		Errors: st.Errors, Action: action,
		Rules: &orchdv1.SafetyNetRules{
			Ok: st.Rules.OK(), ConfigPath: st.Rules.ConfigPath, ConfigExists: st.Rules.ConfigExists,
			ConfigError: st.Rules.ConfigError, Referenced: st.Rules.Referenced, RtkWrapper: st.Rules.RTKWrapper,
			RulebookCurrent: st.Rules.RulebookCurrent, Problems: st.Rules.Problems(),
		},
	}
	if st.Installed != nil {
		out.InstalledVersion, out.InstalledSha = st.Installed.Version, st.Installed.SHA
	}
	for _, in := range st.Legacy {
		out.Legacy = append(out.Legacy, &orchdv1.SafetyNetInstall{Id: in.ID, Scope: in.Scope, Version: in.Version})
	}
	for _, rb := range st.Rules.Rulebooks {
		pb := &orchdv1.SafetyNetRulebook{
			Source: rb.Source, Name: rb.Name, Owner: rb.Owner, Remote: rb.Remote,
			Version: rb.Version, Description: rb.Description, Error: rb.Error,
		}
		for _, r := range rb.Rules {
			pb.Rules = append(pb.Rules, &orchdv1.SafetyNetRule{
				Name: r.Name, Command: r.Command, Subcommand: r.Subcommand,
				BlockArgs: r.BlockArgs, Reason: r.Reason, Off: r.Off,
			})
		}
		out.Rules.Rulebooks = append(out.Rules.Rulebooks, pb)
	}
	return out
}

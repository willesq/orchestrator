package safetynet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/redact"
)

// Command is one `claude` invocation, as an argument array (never a shell line).
type Command struct {
	Args    []string
	Summary string
}

// String renders the command for previews and logs.
func (c Command) String() string { return "claude " + strings.Join(c.Args, " ") }

// PlanOptions tune which commands are planned.
type PlanOptions struct {
	// EnableDisabled re-enables our install when it is disabled. `orch install`
	// leaves a disabled plugin alone, since disabling is a user choice; the
	// app's explicit Install button turns it back on.
	EnableDisabled bool
}

// PlanCommands returns the commands that bring the plugin from st to the pin.
// Each is idempotent in Claude Code, so a partly applied plan converges when
// re-run. An already-converged state plans nothing.
func PlanCommands(st State, loc Locations, opts PlanOptions) []Command {
	var cmds []Command
	switch {
	case st.MarketplaceMismatch:
		cmds = append(cmds,
			Command{[]string{"plugin", "marketplace", "remove", MarketplaceName},
				"unregister " + MarketplaceName + ", which points at " + st.MarketplacePath},
			Command{[]string{"plugin", "marketplace", "add", loc.MarketplaceDir},
				"register the orch marketplace at " + loc.MarketplaceDir})
	case !st.MarketplaceKnown:
		cmds = append(cmds, Command{[]string{"plugin", "marketplace", "add", loc.MarketplaceDir},
			"register the orch marketplace at " + loc.MarketplaceDir})
	case st.Installed != nil && st.Plugin == StateOutdated,
		st.Installed != nil && st.Plugin == StateLegacy && outdated(*st.Installed):
		cmds = append(cmds, Command{[]string{"plugin", "marketplace", "update", MarketplaceName},
			"reload the orch marketplace so it serves v" + st.Pinned.Version})
	}
	for _, in := range st.Legacy {
		// Reason: project and local scopes are tied to a working directory
		// this process is not in; uninstalling them is left to the user.
		if in.Scope != "user" {
			continue
		}
		cmds = append(cmds, Command{[]string{"plugin", "uninstall", in.ID, "--scope", "user"},
			"uninstall the competing copy " + in.ID + " (v" + in.Version + ")"})
	}
	switch {
	case st.Installed == nil || st.MarketplaceMismatch:
		cmds = append(cmds, Command{[]string{"plugin", "install", PluginID, "--scope", "user"},
			"install " + PluginID + " v" + st.Pinned.Version})
	case outdated(*st.Installed):
		cmds = append(cmds, Command{[]string{"plugin", "update", PluginID, "--scope", "user"},
			"upgrade " + PluginID + " from v" + st.Installed.Version + " to v" + st.Pinned.Version})
	case st.Plugin == StateDisabled && opts.EnableDisabled:
		cmds = append(cmds, Command{[]string{"plugin", "enable", PluginID, "--scope", "user"},
			"enable " + PluginID})
	}
	if st.LegacyMarketplaceKnown && !st.LegacyMarketplaceShared && legacyAllUser(st.Legacy) {
		cmds = append(cmds, Command{[]string{"plugin", "marketplace", "remove", LegacyMarketplace},
			"unregister " + LegacyMarketplace + ", which no other installed plugin uses"})
	}
	return cmds
}

// outdated reports whether an install differs from the pin.
func outdated(in Install) bool {
	return in.Version != Pinned.Version || (in.SHA != "" && in.SHA != Pinned.SHA)
}

// legacyAllUser reports whether every legacy install from cc-marketplace is
// one this plan removes; a project-scope copy still needs the marketplace.
func legacyAllUser(legacy []Install) bool {
	return !slices.ContainsFunc(legacy, func(in Install) bool {
		return in.Marketplace == LegacyMarketplace && in.Scope != "user"
	})
}

// Runner runs one claude command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, args []string) (string, error)
}

// commandTimeout bounds one claude command. An install clones the plugin
// repository, which is the slow case.
var commandTimeout = 3 * time.Minute

// maxOutput caps the output kept per command.
const maxOutput = 4 << 10

// ExecRunner runs the claude CLI found the way queued tasks find it.
type ExecRunner struct {
	// Bin is the claude binary; empty means look it up.
	Bin string
	// Loc decides the environment: a relocated home runs with HOME set to it.
	Loc Locations
	// DropEnv names variables withheld from the child, such as credentials
	// orchd holds.
	DropEnv []string
}

// ErrNoClaude means the claude CLI could not be found.
var ErrNoClaude = errors.New("claude CLI not found on PATH or in the usual install locations")

// Run implements Runner. stdin is empty, so a command that wants to prompt
// fails instead of hanging.
func (r ExecRunner) Run(ctx context.Context, args []string) (string, error) {
	bin := r.Bin
	if bin == "" {
		bin = agentrun.FindClaude("", r.Loc.Home)
	}
	if bin == "" {
		return "", ErrNoClaude
	}
	if _, err := os.Stat(bin); err != nil {
		return "", ErrNoClaude
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = CommandEnv(os.Environ(), r.Loc, r.DropEnv)
	cmd.Dir = r.Loc.Home
	// Reason: an install starts git, which starts ssh. Its own process group
	// lets a timeout stop all of them, and WaitDelay bounds anything that
	// escaped the group while holding the output pipe, so a stuck child cannot
	// keep orchd's single action slot busy forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	out := &tailBuffer{max: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	text := cleanOutput(out.String())
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("timed out after %s", commandTimeout)
	}
	return text, err
}

// CommandEnv builds the child environment. For a relocated home, HOME points
// at it and the variables that would redirect Claude Code or cc-safety-net
// back to the real home are removed.
func CommandEnv(base []string, loc Locations, drop []string) []string {
	remove := slices.Clone(drop)
	if loc.Relocated {
		remove = append(remove, "HOME", "CLAUDE_CONFIG_DIR", "CC_SAFETY_NET_HOME")
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(remove, name) {
			out = append(out, kv)
		}
	}
	if loc.Relocated {
		out = append(out, "HOME="+loc.Home)
	}
	return out
}

// waitDelay bounds how long Run waits for the output pipe after the command
// is stopped.
var waitDelay = 5 * time.Second

// tailBuffer keeps the last max bytes written, where errors land, so a
// chatty command cannot grow orchd's memory.
type tailBuffer struct {
	max  int
	buf  []byte
	lost bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.lost = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t.lost {
		return "…" + string(t.buf)
	}
	return string(t.buf)
}

// credentialURL matches the user:password part of a URL, which git can print
// when a remote or insteadOf rule embeds a token.
var credentialURL = regexp.MustCompile(`(://)[^/@\s]+@`)

// cleanOutput trims command output and masks credentials in it, since it is
// kept in orchd and shown in the app.
func cleanOutput(s string) string {
	s = credentialURL.ReplaceAllString(strings.TrimSpace(s), "${1}[REDACTED]@")
	s, _ = redact.Redact(s)
	return s
}

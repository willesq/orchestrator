package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/safetynet"
)

// `orch install` sets everything up in one command, and `orch uninstall`
// (uninstall_cmd.go) is its mirror: both walk install.FullUnits, the one
// registered list of paired units, install in order and uninstall in
// reverse. A single --target keeps the per-harness plan of earlier releases.

// setupFlags are the flags install and uninstall share.
type setupFlags struct {
	apply, yes bool
	target     string
	home       string
	binDir     string

	noModel, noDaemon, noGitHooks, noApp bool
	purgeData                            bool
}

// The machine-facing pieces setup needs, as vars so tests never reach the
// real launchctl, brew, orchd or model download.
var (
	// setupRunner runs launchctl and brew as argument arrays.
	setupRunner install.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// setupDaemonUp reports whether orchd answers on its socket.
	setupDaemonUp = func() bool {
		_, err := doPing(dialOrchd, daemonProbeTimeout)
		return err == nil
	}
	// setupModel returns the embedding model's directory, whether it is
	// present, and how to download it.
	setupModel = func(dataDir string, relocated bool) (string, func() bool, func(context.Context) error) {
		dir := filepath.Join(dataDir, "models", "minilm")
		// Reason: a relocated home never reads the real configuration.
		if !relocated {
			if v, err := config.Load(); err == nil {
				if s, err := config.Unmarshal(v); err == nil && s.Embedder.ModelDir != "" {
					dir = s.Embedder.ModelDir
				}
			}
		}
		m := model.New(model.Config{Dir: dir})
		return dir, m.Present, m.Ensure
	}
	// stdinIsTerminal reports whether a confirmation can be asked.
	stdinIsTerminal = func(r io.Reader) bool {
		f, ok := r.(*os.File)
		if !ok {
			return false
		}
		info, err := f.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0
	}
)

// resolveHome returns the home to set up and whether it is relocated (not
// the real home), which keeps launchd, Homebrew and the real data
// directory out of reach.
func resolveHome(home string) (string, bool, error) {
	real, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	if home == "" {
		return real, false, nil
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", false, err
	}
	return home, !safetynet.SameDir(home, real), nil
}

// machineFor describes this machine for the setup units.
func machineFor(f setupFlags, home string, relocated bool, bins install.Bins) install.Machine {
	dataDir, configDir := paths.DataDir(), paths.ConfigDir()
	if relocated {
		dataDir = filepath.Join(home, ".local", "share", "orch")
		configDir = filepath.Join(home, ".config", "orch")
	}
	modelDir, present, fetch := setupModel(dataDir, relocated)
	apps := []string{filepath.Join(home, "Applications", "Orch.app")}
	if !relocated {
		apps = append([]string{"/Applications/Orch.app"}, apps...)
	}
	return install.Machine{
		Home: home, DataDir: dataDir, ConfigDir: configDir, Relocated: relocated, Bins: bins,
		Run: setupRunner, DaemonUp: setupDaemonUp, Homebrew: install.IsHomebrewManaged(bins.Orchd),
		ModelDir: modelDir, ModelPresent: present, FetchModel: fetch, AppPaths: apps,
		NoModel: f.noModel, NoDaemon: f.noDaemon, NoGitHooks: f.noGitHooks, NoApp: f.noApp,
		PurgeData: f.purgeData,
	}
}

// detectAdapters returns the harnesses installed under home (their config
// directory exists), or Claude Code when there is none.
func detectAdapters(home string) []install.Adapter {
	dirs := map[string]string{"claude-code": ".claude", "codex": ".codex", "gemini": ".gemini"}
	var out []install.Adapter
	for _, a := range install.AllAdapters() {
		if info, err := os.Stat(filepath.Join(home, dirs[a.Name()])); err == nil && info.IsDir() {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		out = []install.Adapter{install.ClaudeCode{}}
	}
	return out
}

func adapterNames(as []install.Adapter) string {
	names := make([]string, len(as))
	for i, a := range as {
		names[i] = a.Name()
	}
	return strings.Join(names, "+")
}

// confirm asks a yes/no question on a terminal; anything but y or yes is no.
func confirm(c *cobra.Command, question string) bool {
	fmt.Fprintf(c.OutOrStdout(), "\n%s [y/N] ", question)
	line, _ := bufio.NewReader(c.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func newInstallCmd() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Set orch up: harness wiring, model, git hooks and orchd (dry-run by default)",
		Long: "install sets everything up in one command. With no --target it wires every coding-agent\n" +
			"harness found under the home (Claude Code, Codex, Gemini), downloads the embedding model,\n" +
			"keeps orch's git hooks current in the repositories they were added to, and starts orchd.\n" +
			"It prints one plan of every change and writes nothing until you confirm, or pass --apply\n" +
			"(or --yes). Every file is backed up before it changes; re-running repairs drift.\n" +
			"--target claude-code|codex|gemini wires one harness only, as earlier releases did.\n" +
			"`orch uninstall` reverses all of it.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			home, relocated, err := resolveHome(f.home)
			if err != nil {
				return err
			}
			bins, err := installBins(f.binDir)
			if err != nil {
				return err
			}
			m := machineFor(f, home, relocated, bins)
			backups := filepath.Join(m.DataDir, "backups")
			if f.target != "auto" && f.target != "all" {
				// One harness, as before: its own plan, no machine units.
				a, err := install.AdapterFor(f.target)
				if err != nil {
					return err
				}
				plan := install.BuildPlan(a, bins, home)
				if f.apply || f.yes {
					return plan.ApplyContext(contextOf(c), c.OutOrStdout(), backups)
				}
				return plan.Print(c.OutOrStdout())
			}
			adapters := install.AllAdapters()
			if f.target == "auto" {
				adapters = detectAdapters(home)
			}
			plan := install.InstallPlan(adapterNames(adapters), install.FullUnits(m, adapters))
			if !f.apply && !f.yes {
				if err := plan.Print(c.OutOrStdout()); err != nil {
					return err
				}
				if n, err := plan.Pending(); err != nil || n == 0 || !stdinIsTerminal(c.InOrStdin()) {
					return err
				}
				if !confirm(c, "Apply these changes?") {
					fmt.Fprintln(c.OutOrStdout(), "Nothing changed.")
					return nil
				}
			}
			return plan.ApplyContext(contextOf(c), c.OutOrStdout(), backups)
		},
	}
	cmd.Flags().BoolVar(&f.apply, "apply", false, "make the changes (default: dry-run, writes nothing)")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "make the changes without asking (same as --apply)")
	cmd.Flags().StringVar(&f.target, "target", "auto", "harness: auto (every one found), all, claude-code, codex or gemini")
	cmd.Flags().StringVar(&f.home, "home", "", "override HOME (for testing against a temp dir; launchd and brew are then left alone)")
	cmd.Flags().StringVar(&f.binDir, "bin-dir", "", "dir of the built binaries (default: dir of this executable)")
	cmd.Flags().BoolVar(&f.noModel, "no-model", false, "do not download the embedding model")
	cmd.Flags().BoolVar(&f.noDaemon, "no-daemon", false, "do not start orchd")
	cmd.Flags().BoolVar(&f.noGitHooks, "no-git-hooks", false, "leave orch's git hooks as they are")
	cmd.Flags().BoolVar(&f.noApp, "no-app", false, "do not suggest the menu bar app")
	return cmd
}

// contextOf is the command's context, or a background one when run
// without (as in tests that call RunE directly).
func contextOf(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

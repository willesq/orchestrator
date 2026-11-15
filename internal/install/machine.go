package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/settings/spacesettings"
)

// CommandRunner runs an external command (launchctl, brew) as an argument
// array, never a shell line, and returns its combined output. Tests inject a
// fake so no test reaches the real launchd or Homebrew.
type CommandRunner func(ctx context.Context, name string, args ...string) (string, error)

// Machine is the part of an install that is not one harness's: the
// embedding model, git hooks, orchd's launchd agent or Homebrew service, the
// per-checkout settings orchd writes, orch's data, and the app. Its units
// sit around the harness units in the one registered list (FullUnits).
type Machine struct {
	Home    string
	DataDir string
	// ConfigDir is orch's config directory (legacy config.yaml lives there).
	ConfigDir string
	// Relocated is true for a --home that is not the real home: launchctl
	// and brew are then never run, so a test cannot reach the real ones.
	Relocated bool
	Bins      Bins
	Run       CommandRunner
	// DaemonUp reports whether orchd answers on its socket.
	DaemonUp func() bool
	// Homebrew is true when orchd is Homebrew-managed (brew services).
	Homebrew bool
	// ModelDir, ModelPresent and FetchModel download the embedding model.
	ModelDir     string
	ModelPresent func() bool
	FetchModel   func(ctx context.Context) error
	// AppPaths are where Orch.app may be installed.
	AppPaths []string

	// Git runs git for the uninstall's search for orch's git hooks; nil
	// asks git nothing (only <repo>/.git/hooks is then looked at).
	Git GitRunner
	// GitHookPlaces lists the repositories and folders orch has a record
	// of (RecordedGitHookPlaces); GitHookScan are folders the user asked
	// to search as well (--scan).
	GitHookPlaces func() GitHookPlaces
	GitHookScan   []string

	NoModel, NoDaemon, NoGitHooks, NoApp bool
	// PurgeData makes the uninstall delete orch's data (PurgeTargets);
	// PurgeList, when set, is the list the user confirmed, and only it is
	// deleted.
	PurgeData bool
	PurgeList []string
}

// FullUnits is the whole registered list for a machine, in install order:
// the machine units that hold data first (so an uninstall reaches them
// last), then each harness's units and what earlier releases installed,
// then the git hooks and the daemon (so an uninstall stops orchd before
// anything else).
func FullUnits(m Machine, adapters []Adapter) []Unit {
	units := []Unit{m.prerequisitesUnit(), m.dataUnit(), m.checkoutSettingsUnit(), m.modelUnit()}
	for _, a := range adapters {
		units = append(units, a.Units(m.Bins, m.Home)...)
	}
	units = append(units, pastToolsUnit(m.Home))
	return append(units, m.gitHooksUnit(), m.gitHookDiscoveryUnit(), m.daemonUnit(), m.appUnit())
}

// prerequisitesUnit names what only the user can set up: Homebrew itself
// and their own logins. orch never handles credentials.
func (m Machine) prerequisitesUnit() Unit {
	return Unit{
		Name:        "prerequisites",
		InstalledBy: "you: Homebrew installs orch, and logins are yours",
		Manual: func() []string {
			return []string{"`gh auth login`, if `orch check` reports gh is not logged in (orch never handles credentials)"}
		},
		Leftovers: func() []string {
			return []string{"your gh and Claude Code logins are untouched"}
		},
	}
}

// dataUnit is orch's data directory: memory, models, logs, backups.
func (m Machine) dataUnit() Unit {
	return Unit{
		Name:        "data",
		InstalledBy: "orchd, as it runs (memory, Qdrant, models, logs)",
		UndoSteps: func() []Step {
			if !m.PurgeData {
				return nil
			}
			return m.purgeSteps()
		},
		Leftovers: func() []string {
			var out []string
			if !m.PurgeData {
				out = append(out, "your data is kept in "+m.DataDir+" (memory, Qdrant, models); "+
					"`orch uninstall --purge-data --apply` deletes it")
			}
			return append(out, "backups of every file orch changed are kept in "+filepath.Join(m.DataDir, "backups")+
				" (owner-only; copies of harness config such as ~/.claude.json can hold login state — delete them once you no longer need them)")
		},
	}
}

// checkoutSettingsUnit is the per-checkout .claude/settings.local.json
// entries orchd's spaces sync writes. Uninstall runs the same sync switched
// off: it takes out exactly what orch recorded adding, and removes files
// orch created.
func (m Machine) checkoutSettingsUnit() Unit {
	layout := spacesettings.Layout{Dir: filepath.Join(m.DataDir, "spaces")}
	return Unit{
		Name:        "checkout settings",
		InstalledBy: "orchd's spaces sync",
		UndoSteps: func() []Step {
			n := recordedCheckouts(layout.StateFile())
			if n == 0 {
				return nil
			}
			return []Step{{
				Summary: fmt.Sprintf("spaces: take orch's entries out of %d checkout settings file(s) "+
					"(.claude/settings.local.json); files orch created are removed", n),
				Run: func(ctx context.Context) (string, error) {
					s := &spacesettings.Syncer{Layout: layout, StateFile: layout.StateFile(),
						BackupDir: filepath.Join(layout.OrchDir(), "backups")}
					rep := s.Sync(ctx, nil, spacesettings.Choice{SwitchedOff: true})
					out := fmt.Sprintf("cleared %d file(s)", len(rep.Cleared))
					if len(rep.Problems) > 0 {
						return out, errors.New(strings.Join(rep.Problems, "; "))
					}
					return out, nil
				},
			}}
		},
	}
}

// recordedCheckouts counts the checkouts orch's record says it wrote.
func recordedCheckouts(stateFile string) int { return len(recordedCheckoutPaths(stateFile)) }

// modelUnit downloads the embedding model when it is missing. The model is
// data: uninstall keeps it, and only --purge-data deletes it (dataUnit).
func (m Machine) modelUnit() Unit {
	return Unit{
		Name: "embedding model",
		Steps: func() []Step {
			if m.NoModel || m.FetchModel == nil || (m.ModelPresent != nil && m.ModelPresent()) {
				return nil
			}
			return []Step{{
				Summary: "model: download the embedding model (~90 MB) into " + m.ModelDir,
				Run:     func(ctx context.Context) (string, error) { return "downloaded", m.FetchModel(ctx) },
			}}
		},
		UndoSteps: func() []Step { return nil },
	}
}

// gitHooksUnit keeps orch's git hooks current in every recorded hooks
// directory, and removes them (putting chained hooks back) on uninstall.
func (m Machine) gitHooksUnit() Unit {
	backups := filepath.Join(m.DataDir, "backups", "git-hooks")
	plan := func(uninstall bool) []Step {
		if m.NoGitHooks {
			return nil
		}
		dirs, err := RegisteredGitHookDirs(m.DataDir)
		if err != nil {
			return []Step{{Summary: "git hooks: read " + GitHookRegistryPath(m.DataDir),
				Run: func(context.Context) (string, error) { return "", err }}}
		}
		var steps []Step
		for _, dir := range dirs {
			if !isRealDir(dir) {
				if uninstall {
					steps = append(steps, Step{Summary: "git hooks: forget " + dir + " — it is gone",
						Run: func(context.Context) (string, error) { return "", RecordGitHookDir(m.DataDir, dir, false) }})
				}
				continue
			}
			// Reason: install only repairs. A recorded directory holding
			// none of orch's hooks (removed by hand) gets none back, so an
			// entry added to the registry cannot make install write hooks
			// somewhere they never were.
			if !uninstall && !holdsOrchGitHook(dir) {
				continue
			}
			p, err := PlanGitHooks(dir, m.Bins.Orch, uninstall)
			if err != nil {
				steps = append(steps, Step{Summary: "git hooks: " + dir,
					Run: func(context.Context) (string, error) { return "", err }})
				continue
			}
			if p.Changes() == 0 && !uninstall {
				continue
			}
			verb := "repair orch's hooks in "
			switch {
			case uninstall && p.Changes() == 0:
				verb = "forget (no orch hooks left there) "
			case uninstall:
				verb = "remove orch's hooks (putting back any they chained) from "
			}
			steps = append(steps, Step{Summary: "git hooks: " + verb + dir, Run: func(context.Context) (string, error) {
				var out bytes.Buffer
				if p.Changes() > 0 {
					if err := p.Apply(&out, backups); err != nil {
						return out.String(), err
					}
				}
				if uninstall {
					return out.String(), RecordGitHookDir(m.DataDir, dir, false)
				}
				return out.String(), nil
			}})
		}
		return steps
	}
	return Unit{
		Name:      "git hooks",
		Steps:     func() []Step { return plan(false) },
		UndoSteps: func() []Step { return plan(true) },
	}
}

// daemonUnit starts orchd under whatever supervises it and stops it on
// uninstall, first, so nothing rewrites what the rest of the uninstall
// removes.
func (m Machine) daemonUnit() Unit {
	plist := PlistPath(m.Home)
	run := func(name string, args ...string) func(ctx context.Context) (string, error) {
		return func(ctx context.Context) (string, error) { return m.Run(ctx, name, args...) }
	}
	return Unit{
		Name: "daemon",
		Steps: func() []Step {
			if m.NoDaemon || m.Relocated || m.Run == nil || (m.DaemonUp != nil && m.DaemonUp()) {
				return nil
			}
			if m.Homebrew {
				return []Step{{Summary: "daemon: start orchd\n  brew services start orch", Run: run("brew", "services", "start", "orch")}}
			}
			return []Step{{Summary: "daemon: start orchd\n  launchctl load -w " + plist, Run: run("launchctl", "load", "-w", plist)}}
		},
		UndoSteps: func() []Step {
			if m.Relocated || m.Run == nil {
				return nil
			}
			if m.Homebrew {
				if m.DaemonUp != nil && !m.DaemonUp() {
					return nil
				}
				return []Step{{Summary: "daemon: stop orchd\n  brew services stop orch", Run: run("brew", "services", "stop", "orch")}}
			}
			if _, err := m.Run(context.Background(), "launchctl", "list", OrchdLabel); err != nil {
				return nil // not loaded
			}
			if _, err := os.Lstat(plist); err != nil {
				return []Step{{Summary: "daemon: stop orchd\n  launchctl remove " + OrchdLabel, Run: run("launchctl", "remove", OrchdLabel)}}
			}
			return []Step{{Summary: "daemon: stop orchd\n  launchctl unload -w " + plist, Run: run("launchctl", "unload", "-w", plist)}}
		},
		Manual: func() []string {
			if m.Relocated && !m.NoDaemon {
				return []string{"orchd was not started: --home is not your home, so launchd and Homebrew are left alone"}
			}
			return nil
		},
		Leftovers: func() []string {
			switch {
			case m.Relocated:
				return []string{"--home is not your home, so launchd and Homebrew were left alone"}
			case m.Homebrew:
				return []string{"remove the binaries with Homebrew: `brew uninstall orch` (and `brew untap willesq/tap` if nothing else uses it)"}
			}
			return append(strayHomebrewService(m.Home, m.Homebrew),
				"remove the orch, orchd and orch-hook binaries from "+filepath.Dir(m.Bins.Orch))
		},
	}
}

// appUnit is Orch.app, which Homebrew or `make app-install` installs. Its
// login item belongs to the app (SMAppService), which a CLI cannot
// unregister, so uninstall names what to do instead of doing it.
func (m Machine) appUnit() Unit {
	found := func() []string {
		var out []string
		for _, p := range m.AppPaths {
			if isRealDir(p) {
				out = append(out, p)
			}
		}
		return out
	}
	return Unit{
		Name:        "app",
		InstalledBy: "Homebrew (`brew install --cask willesq/tap/orch-app`) or `make app-install`",
		Manual: func() []string {
			if m.NoApp || len(found()) > 0 {
				return nil
			}
			return []string{"optional menu bar app: `brew install --cask willesq/tap/orch-app`"}
		},
		Leftovers: func() []string {
			var out []string
			for _, p := range found() {
				out = append(out, "Orch.app at "+p+": quit it and turn off Open at login in its menu "+
					"(or System Settings > General > Login Items), then `brew uninstall --cask orch-app` "+
					"or move it to the Trash")
			}
			return out
		},
	}
}

// holdsOrchGitHook reports whether any hook orch installs in dir is orch's.
func holdsOrchGitHook(dir string) bool {
	for _, name := range GitHookNames {
		content, _, _, err := lstatHook(filepath.Join(dir, name))
		if err == nil && IsOrchGitHook(content) {
			return true
		}
	}
	return false
}

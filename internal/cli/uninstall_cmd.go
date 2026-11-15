package cli

import (
	"bufio"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/install"
)

func newUninstallCmd() *cobra.Command {
	var f setupFlags
	var scan []string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove everything orch install put in place (dry-run by default)",
		Long: "uninstall reverses `orch install`, walking the same steps in reverse: it stops orchd,\n" +
			"removes orch's git hooks wherever it finds them (the repositories they were recorded\n" +
			"in, every repository and root orch has a record of, the global core.hooksPath, and any\n" +
			"--scan folder), putting back the hooks they chained, removes what earlier releases\n" +
			"installed, and takes orch's entries out of per-checkout settings, the Claude Code plugin and every harness file,\n" +
			"and deletes the files only orch owns. Your own content in those files is kept, and\n" +
			"every file is backed up before it changes. It works with orchd down.\n" +
			"Your data (memory, Qdrant, models, logs) is kept unless --purge-data, which lists\n" +
			"every path and asks you to confirm (or pass --yes). Credentials are never touched.\n" +
			"--target claude-code|codex|gemini removes one harness's wiring and keeps the shared parts.",
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
			if f.purgeData && f.target != "all" {
				return errors.New("--purge-data needs a full uninstall (--target all)")
			}
			m := machineFor(f, home, relocated, bins)
			m.Git = uninstallGit(home, relocated)
			for _, dir := range scan {
				abs, err := filepath.Abs(dir)
				if err != nil {
					return err
				}
				m.GitHookScan = append(m.GitHookScan, abs)
			}
			m.GitHookPlaces = func() install.GitHookPlaces { return uninstallPlaces(contextOf(c), home, m.DataDir) }
			var purge []string
			if f.purgeData {
				if purge, err = m.PurgeTargets(); err != nil {
					return err
				}
				// Exactly the listed paths are deleted, never one that
				// appears after the confirmation.
				m.PurgeList = append([]string{}, purge...)
			}
			var plan install.Plan
			if f.target == "all" {
				plan = install.UninstallPlan("all", install.FullUnits(m, install.AllAdapters()), true)
			} else {
				a, err := install.AdapterFor(f.target)
				if err != nil {
					return err
				}
				plan = install.UninstallPlan(a.Name(), a.Units(bins, home), false)
			}
			interactive := stdinIsTerminal(c.InOrStdin())
			if !f.apply && !f.yes {
				if err := plan.Print(c.OutOrStdout()); err != nil {
					return err
				}
				if n, err := plan.Pending(); err != nil || n == 0 || !interactive {
					return err
				}
			}
			if len(purge) > 0 && !f.yes {
				if !interactive {
					return fmt.Errorf("--purge-data deletes %d path(s) under %s and cannot be undone; "+
						"review them with `orch uninstall --purge-data`, then re-run with --yes", len(purge), m.DataDir)
				}
				if !confirmPurge(c, purge, filepath.Join(m.DataDir, "backups")) {
					fmt.Fprintln(c.OutOrStdout(), "Nothing changed.")
					return nil
				}
			} else if !f.apply && !f.yes && !confirm(c, "Remove these?") {
				fmt.Fprintln(c.OutOrStdout(), "Nothing changed.")
				return nil
			}
			return plan.ApplyContext(contextOf(c), c.OutOrStdout(), filepath.Join(m.DataDir, "backups"))
		},
	}
	cmd.Flags().BoolVar(&f.apply, "apply", false, "make the changes (default: dry-run, changes nothing)")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "make the changes without asking, including --purge-data")
	cmd.Flags().StringVar(&f.target, "target", "all", "all, or one harness: claude-code, codex or gemini")
	cmd.Flags().StringVar(&f.home, "home", "", "override HOME (for testing against a temp dir; launchd and brew are then left alone)")
	cmd.Flags().StringVar(&f.binDir, "bin-dir", "", "dir of the built binaries (default: dir of this executable)")
	cmd.Flags().BoolVar(&f.noGitHooks, "no-git-hooks", false, "leave orch's git hooks in place (they refuse commits once orch is gone)")
	cmd.Flags().StringArrayVar(&scan, "scan", nil, "also search this folder for orch's git hooks (repeatable; bounded depth, symlinks not followed)")
	cmd.Flags().BoolVar(&f.purgeData, "purge-data", false, "also delete orch's data: memory, Qdrant, models, logs (backups are kept)")
	return cmd
}

// confirmPurge lists every path --purge-data deletes and asks the user to
// type "delete".
func confirmPurge(c *cobra.Command, paths []string, backups string) bool {
	w := c.OutOrStdout()
	fmt.Fprintf(w, "\n--purge-data permanently deletes these %d path(s):\n", len(paths))
	for _, p := range paths {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintf(w, "Backups in %s are kept.\nType delete to confirm: ", backups)
	line, _ := bufio.NewReader(c.InOrStdin()).ReadString('\n')
	return strings.TrimSpace(line) == "delete"
}

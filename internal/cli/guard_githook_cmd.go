package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// `orch guard git-hook` (spaces plan SP-045): git hooks that run the
// outbound terms check for every commit and push in a repository, whoever
// makes them. The hooks read the same guard files as the tool hook, so
// they need no orchd connection.

// gitHookFiles returns the guard files the hook check reads; tests replace
// it.
var gitHookFiles = func() spaces.GuardFiles {
	return spaces.GuardFiles{Dir: paths.GuardDir(), DB: paths.DBFile()}
}

func newGuardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Helpers for the spaces guard outside agent sessions",
	}
	cmd.AddCommand(newGitHookCmd())
	return cmd
}

func newGitHookCmd() *cobra.Command {
	var installHooks, uninstall, apply bool
	var repo, hooksDir, orchBin string
	cmd := &cobra.Command{
		Use:   "git-hook (--install | --uninstall) [--apply]",
		Short: "Add git hooks that refuse commits and pushes carrying another space's terms",
		Long: "--install adds commit-msg and pre-push hooks to a repository's hooks directory\n" +
			"(core.hooksPath when set, or --hooks-dir) that run the outbound terms check on\n" +
			"every commit and push, including ones made from your own terminal. A hook already\n" +
			"there moves to <name>.orch-prev and still runs after orch's check passes.\n" +
			"--uninstall removes orch's hooks and puts the chained ones back.\n" +
			"Without --apply nothing is written.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if installHooks == uninstall {
				return errors.New("use exactly one of --install or --uninstall")
			}
			dir, err := gitHooksDir(c.Context(), repo, hooksDir)
			if err != nil {
				return err
			}
			if !uninstall && orchBin == "" {
				if orchBin, err = os.Executable(); err != nil {
					return fmt.Errorf("find orch's own path (pass --orch): %w", err)
				}
			}
			if orchBin != "" {
				if orchBin, err = filepath.Abs(orchBin); err != nil {
					return err
				}
			}
			plan, err := install.PlanGitHooks(dir, orchBin, uninstall)
			if err != nil {
				return err
			}
			if !apply {
				plan.Print(c.OutOrStdout())
				return nil
			}
			// Recorded so `orch install` keeps these hooks current and
			// `orch uninstall` removes them: before an install, so a hook
			// is never left unrecorded; after an uninstall, once gone.
			if !uninstall {
				if err := install.RecordGitHookDir(paths.DataDir(), dir, true); err != nil {
					return fmt.Errorf("record %s for orch uninstall: %w", dir, err)
				}
			}
			if err := plan.Apply(c.OutOrStdout(), filepath.Join(paths.DataDir(), "backups", "git-hooks")); err != nil {
				return err
			}
			if uninstall {
				return install.RecordGitHookDir(paths.DataDir(), dir, false)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&installHooks, "install", false, "add orch's commit-msg and pre-push hooks")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "remove orch's hooks and restore the ones they chained")
	cmd.Flags().BoolVar(&apply, "apply", false, "write the changes (default: dry-run, writes nothing)")
	cmd.Flags().StringVar(&repo, "repo", "", "repository whose hooks directory to use (default: the current directory)")
	cmd.Flags().StringVar(&hooksDir, "hooks-dir", "", "hooks directory to use instead of the repository's (e.g. a shared core.hooksPath)")
	cmd.Flags().StringVar(&orchBin, "orch", "", "orch binary the hooks run (default: this one)")
	cmd.AddCommand(newGitHookRunCmd())
	return cmd
}

// gitHooksDir is the hooks directory to install in: hooksDir when given,
// else the one git uses for the repository at repo (core.hooksPath when
// set, the common git directory's hooks otherwise).
func gitHooksDir(ctx context.Context, repo, hooksDir string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if hooksDir != "" {
		return filepath.Abs(hooksDir)
	}
	if repo == "" {
		repo = "."
	}
	repo, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	out, err := spaces.RunGit(ctx, repo, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		return "", fmt.Errorf("%s is not a git repository (or git could not name its hooks directory)", repo)
	}
	return dir, nil
}

func newGitHookRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run (commit-msg <message-file> | pre-push <remote> <url>)",
		Short: "Run the outbound terms check as a git hook (what the installed hooks call)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			in, err := gitHookInput(c, args)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), spaces.GitHookTimeout)
			defer cancel()
			r := checkGitHook(ctx, gitHookFiles().ReadTerms(ctx), in)
			if r.Refuse {
				return errors.New(r.Reason())
			}
			return nil
		},
	}
}

// checkGitHook runs the check, refusing when it panics. Reason: a check
// that fails cannot vouch for the commit (INV-07).
func checkGitHook(ctx context.Context, g spaces.GuardTerms, in spaces.GitHookInput) (r spaces.OutboundResult) {
	defer func() {
		if recover() != nil {
			kind := "git commit"
			if in.Hook == spaces.GitHookPrePush {
				kind = "git push"
			}
			r = spaces.OutboundResult{Refuse: true, Rule: spaces.RuleOutboundUnchecked, Kind: kind}
		}
	}()
	return spaces.CheckGitHook(ctx, g, in)
}

// gitHookInput reads a hook's arguments and input. git runs a hook in the
// work tree's top directory, which its relative paths are under.
func gitHookInput(c *cobra.Command, args []string) (spaces.GitHookInput, error) {
	dir, err := os.Getwd()
	if err != nil {
		return spaces.GitHookInput{}, err
	}
	in := spaces.GitHookInput{Hook: args[0], Dir: dir}
	switch args[0] {
	case spaces.GitHookCommitMsg:
		if len(args) != 2 {
			return in, errors.New("commit-msg takes the message file")
		}
		in.MsgFile = args[1]
		if !filepath.IsAbs(in.MsgFile) {
			in.MsgFile = filepath.Join(dir, in.MsgFile)
		}
	case spaces.GitHookPrePush:
		if len(args) != 3 {
			return in, errors.New("pre-push takes the remote and its URL")
		}
		in.Remote, in.URL = args[1], args[2]
		if in.Updates, err = spaces.ParsePushUpdates(c.InOrStdin()); err != nil {
			return in, fmt.Errorf("orch outbound terms refused git push: %w", err)
		}
	default:
		return in, fmt.Errorf("unknown git hook %q (commit-msg or pre-push)", args[0])
	}
	return in, nil
}

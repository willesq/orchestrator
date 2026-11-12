package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// launcher starts a harness with a space's profile (spaces plan SP-046.2).
// Its fields are what tests replace.
type launcher struct {
	cf     clientFactory
	layout spacesettings.Layout
	// claudeDir is the user's own Claude Code configuration folder.
	claudeDir func() (string, error)
	getwd     func() (string, error)
	environ   func() []string
	lookPath  func(string) (string, error)
	// exec replaces orch with the harness.
	exec func(path string, argv, env []string) error
}

func newLauncher(cf clientFactory) *launcher {
	return &launcher{
		cf:        cf,
		layout:    spacesettings.DefaultLayout(),
		claudeDir: userClaudeDir,
		getwd:     os.Getwd,
		environ:   os.Environ,
		lookPath:  exec.LookPath,
		exec:      syscall.Exec,
	}
}

// userClaudeDir is Claude Code's configuration folder for sessions started
// without the launcher: $CLAUDE_CONFIG_DIR, else ~/.claude.
func userClaudeDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// harnesses are the programs the launcher starts.
var harnesses = []string{"claude", "codex"}

// spaceEnvKeys are the variables the launcher sets; any inherited value is
// replaced.
var spaceEnvKeys = []string{"CLAUDE_CONFIG_DIR", "GH_CONFIG_DIR", "TMPDIR", "CLAUDE_CODE_TMPDIR"}

func newRunCmd(cf clientFactory) *cobra.Command {
	return newRunCmdWith(newLauncher(cf))
}

func newRunCmdWith(l *launcher) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "run <space> -- claude|codex [args...]",
		Short: "Start Claude Code or Codex with a space's profile, from any folder",
		Long: `Starts a harness session with the space's full profile: the OS sandbox and
file-tool denies for other jobs' folders, the space's gh profile
(GH_CONFIG_DIR) and its own TMPDIR. Claude Code runs with a per-space
CLAUDE_CONFIG_DIR, which needs its own login the first time (run /login in
the session). Codex runs with your own login and a permissions profile
given with -c.

The space must be in the applied policy. Run it in the space's folders or in
a folder no space holds; --force allows a folder another space holds.
The variables are set for the harness only, never in your shell.`,
		Args: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash != 1 || len(args) < 2 {
				return errors.New("usage: orch run <space> -- claude|codex [args...]")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return l.run(c.ErrOrStderr(), args[0], args[1:], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "start even though the current folder is in another space")
	return cmd
}

// run checks the space and the folder, prepares the profile and starts the
// harness.
func (l *launcher) run(out io.Writer, space string, argv []string, force bool) error {
	name := filepath.Base(argv[0])
	if !slices.Contains(harnesses, name) {
		return fmt.Errorf("orch run starts claude or codex, not %q", argv[0])
	}
	cwd, err := l.getwd()
	if err != nil {
		return err
	}
	var res *spaces.Resolved
	on := true
	err = withClient(l.cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
		var err error
		if res, err = appliedPolicy(ctx, client); err != nil {
			return err
		}
		if _, ok := res.Space(space); !ok {
			return fmt.Errorf("unknown space %q (spaces: %s)", space, strings.Join(res.Names(), ", "))
		}
		if err := checkFolder(ctx, client, cwd, space, force); err != nil {
			return err
		}
		on = sandboxOn(ctx, client)
		return nil
	})
	if err != nil {
		return err
	}
	rules := spacesettings.For(res, space, l.layout)
	if err := spacesettings.EnsurePrivateDir(l.layout.SpaceDir(space)); err != nil {
		return err
	}
	if err := l.layout.EnsureTmpDir(space); err != nil {
		return err
	}
	env := withoutKeys(l.environ(), spaceEnvKeys)
	for k, v := range rules.Env() {
		env = append(env, k+"="+v)
	}
	args := slices.Clone(argv)
	if !on {
		fmt.Fprintln(out, "orch run: the sandbox layer is switched off; the session gets no deny rules")
	}
	switch name {
	case "claude":
		var frag spacesettings.Fragment
		if on {
			frag = spacesettings.LauncherFragment(rules)
		}
		userDir, err := l.claudeDir()
		if err != nil {
			return err
		}
		dir, err := spacesettings.WriteClaudeProfile(l.layout, space, userDir, frag)
		if err != nil {
			return err
		}
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
		fmt.Fprintf(out, "orch run: space %s, Claude Code profile %s\n", space, dir)
		if spacesettings.ClaudeSandboxTmp(rules.TmpDir) == spacesettings.SharedClaudeTmp() {
			fmt.Fprintf(out, "orch run: %s is too long for Claude Code's temp folder; its shell commands use the shared %s\n", rules.TmpDir, spacesettings.SharedClaudeTmp())
		}
	case "codex":
		if on {
			args = append([]string{argv[0]}, append(spacesettings.CodexArgs(rules), argv[1:]...)...)
		}
		fmt.Fprintf(out, "orch run: space %s, Codex permissions profile %s\n", space, spacesettings.CodexProfile(space))
	}
	path, err := l.lookPath(argv[0])
	if err != nil {
		return err
	}
	return l.exec(path, args, env)
}

// appliedPolicy reads the applied spaces policy from orchd. orchd answers
// only the operator, so an agent cannot use the launcher to read it.
func appliedPolicy(ctx context.Context, client orchdv1.OrchdClient) (*spaces.Resolved, error) {
	d, err := client.DiffSpaces(ctx, &orchdv1.DiffSpacesRequest{})
	if err != nil {
		return nil, err
	}
	if d.GetAppliedRevision() == 0 {
		return nil, errors.New("no spaces policy is applied; see 'orch spaces'")
	}
	p, err := spaces.Parse([]byte(d.GetAppliedJson()))
	if err != nil {
		return nil, err
	}
	return spaces.Validate(p, spaces.DefaultEnv())
}

// checkFolder refuses a folder that another space holds, or whose space
// cannot be decided, unless forced. A folder no root or remote places is
// allowed: the launcher is how a session there gets the space's profile.
func checkFolder(ctx context.Context, client orchdv1.OrchdClient, cwd, space string, force bool) error {
	e, err := client.ExplainPlacement(ctx, &orchdv1.ExplainPlacementRequest{Target: cwd})
	if err != nil {
		return err
	}
	switch {
	case e.GetOutcome() == string(spaces.Placed) && e.GetSpace() == space:
		return nil
	case e.GetRule() == string(spaces.PlaceUnmatched):
		return nil
	case force:
		return nil
	case e.GetOutcome() == string(spaces.Placed):
		return fmt.Errorf("this folder is in space %s, not %s; start the session from a folder of %s, or pass --force", e.GetSpace(), space, space)
	}
	return fmt.Errorf("this folder's space cannot be decided (%s); start the session from a folder of %s, or pass --force", e.GetReason(), space)
}

// sandboxOn reports whether the sandbox layer is on. When orchd cannot say,
// the rules are applied.
func sandboxOn(ctx context.Context, client orchdv1.OrchdClient) bool {
	st, err := client.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
	if err != nil {
		return true
	}
	for _, sw := range st.GetSwitches() {
		if sw.GetLayer() == string(spaces.LayerSandbox) {
			return sw.GetState() != spaces.SwitchOff
		}
	}
	return true
}

// withoutKeys drops every variable named in keys.
func withoutKeys(env, keys []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(keys, k) {
			out = append(out, kv)
		}
	}
	return out
}

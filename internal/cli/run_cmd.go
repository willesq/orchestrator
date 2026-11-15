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

	"github.com/willesq/orchestrator/internal/github"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// launcher starts a harness with a space's profile (spaces plan SP-046.2).
// Its fields are what tests replace.
type launcher struct {
	cf       clientFactory
	layout   spacesettings.Layout
	getwd    func() (string, error)
	environ  func() []string
	lookPath func(string) (string, error)
	// exec replaces orch with the harness.
	exec func(path string, argv, env []string) error
}

func newLauncher(cf clientFactory) *launcher {
	return &launcher{
		cf:       cf,
		layout:   spacesettings.DefaultLayout(),
		getwd:    os.Getwd,
		environ:  os.Environ,
		lookPath: exec.LookPath,
		exec:     syscall.Exec,
	}
}

// harnesses are the programs the launcher starts.
var harnesses = []string{"claude", "codex"}

// tmpEnvKeys are the temp folder variables; the launcher always replaces
// any inherited value with the space's.
var tmpEnvKeys = []string{"TMPDIR", "CLAUDE_CODE_TMPDIR"}

// claudeLoginOverrides would make Claude Code use another login than the
// one in CLAUDE_CONFIG_DIR.
var claudeLoginOverrides = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// overridesOf are the variables dropped with a login folder the space sets:
// with them a session would act as whoever they name instead (the
// variables orchd's own gh calls drop, D-18).
func overridesOf(key string) []string {
	if key == "GH_CONFIG_DIR" {
		return github.ProfileOverrides()
	}
	return claudeLoginOverrides
}

// loginEnvKeys name a login folder. The launcher sets each only when the
// space names one (D-59); otherwise an inherited value is the user's own
// login and stays, unless it is a folder another space names.
var loginEnvKeys = []string{"CLAUDE_CONFIG_DIR", "GH_CONFIG_DIR"}

func newRunCmd(cf clientFactory) *cobra.Command {
	return newRunCmdWith(newLauncher(cf))
}

func newRunCmdWith(l *launcher) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "run <space> -- claude|codex [args...]",
		Short: "Start Claude Code or Codex with a space's profile, from any folder",
		Long: `Starts a harness session with the space's full profile: the OS sandbox and
file-tool denies for other jobs' folders and its own TMPDIR. Claude Code gets
the rules with --settings; Codex as a permissions profile given with -c.
Both run with your own logins unless the space names its own: its
gh_config_dir becomes GH_CONFIG_DIR and its claude_config_dir becomes
CLAUDE_CONFIG_DIR (which needs its own /login the first time).

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
			if cliSpacesOff() {
				return errCLISpacesOff
			}
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
	choice := spacesettings.ChoiceOn
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
		choice = sandboxChoice(ctx, client)
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
	on := choice.Sandboxes(rules)
	switch {
	case choice.SwitchedOff:
		fmt.Fprintln(out, "orch run: the sandbox layer is switched off; the session gets no deny rules")
	case !on && choice.Setting == spaces.SandboxOff:
		fmt.Fprintln(out, "orch run: the sandbox setting is off; the session gets no deny rules")
	case !on:
		fmt.Fprintln(out, "orch run: no other isolated space's folders or login folders to deny (sandbox setting auto); the session gets no deny rules")
	}
	var (
		args     = slices.Clone(argv)
		spaceEnv map[string]string
	)
	switch name {
	case "claude":
		var frag spacesettings.Fragment
		if on {
			if slices.ContainsFunc(argv[1:], isSettingsFlag) {
				// Reason: Claude Code takes one --settings, so the user's
				// would replace the one holding the space's rules.
				return errors.New("orch run gives Claude Code the space's rules with --settings; put your own settings in a settings file instead")
			}
			frag = spacesettings.LauncherFragment(rules)
			// Reason (RC-215): Claude Code adds this folder's own project
			// settings, whose allowRead or allowWrite would re-open the
			// space's denies; orch cannot change them, so it says so.
			for _, l := range spacesettings.Loosening(cwd, rules.Deny, spacesettings.Owned{}) {
				fmt.Fprintf(out, "orch run: warning: %s\n", l)
			}
		}
		extra, env, err := spacesettings.ClaudeLaunch(rules, frag)
		if err != nil {
			return err
		}
		args = append([]string{argv[0]}, append(extra, argv[1:]...)...)
		spaceEnv = env
		cfg := "your own Claude Code configuration"
		if rules.ClaudeConfigDir != "" {
			cfg = "Claude Code configuration " + rules.ClaudeConfigDir
		}
		fmt.Fprintf(out, "orch run: space %s, %s\n", space, cfg)
		if spacesettings.ClaudeSandboxTmp(rules.TmpDir) == spacesettings.SharedClaudeTmp() {
			fmt.Fprintf(out, "orch run: %s is too long for Claude Code's temp folder; its shell commands use the shared %s\n", rules.TmpDir, spacesettings.SharedClaudeTmp())
		}
	case "codex":
		if on {
			args = append([]string{argv[0]}, append(spacesettings.CodexArgs(rules), argv[1:]...)...)
		}
		spaceEnv = rules.Env()
		fmt.Fprintf(out, "orch run: space %s, Codex permissions profile %s\n", space, spacesettings.CodexProfile(space))
	}
	env := launchEnv(out, l.environ(), spaceEnv, otherLogins(res, space))
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

// sandboxChoice is the sandbox layer's switch and setting (D-61). When
// orchd cannot say, or names no setting, the rules are applied.
func sandboxChoice(ctx context.Context, client orchdv1.OrchdClient) spacesettings.Choice {
	st, err := client.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
	if err != nil {
		return spacesettings.ChoiceOn
	}
	c := spacesettings.ChoiceOn
	if m, err := spaces.ParseSandboxMode(st.GetSandboxSetting().GetMode()); err == nil {
		c.Setting = m
	}
	for _, sw := range st.GetSwitches() {
		if sw.GetLayer() == string(spaces.LayerSandbox) {
			c.SwitchedOff = sw.GetState() == spaces.SwitchOff
		}
	}
	return c
}

// launchEnv is the harness's environment: environ with the temp folder
// variables replaced by the space's, a login variable replaced when the
// space sets it and dropped when it names another space's login folder
// (the session would act as that space's account), and the rest kept.
func launchEnv(out io.Writer, environ []string, spaceEnv map[string]string, others []string) []string {
	drop := slices.Clone(tmpEnvKeys)
	for _, k := range loginEnvKeys {
		if _, ok := spaceEnv[k]; ok {
			drop = append(drop, k)
			for _, o := range overridesOf(k) {
				if o != k && slices.ContainsFunc(environ, func(kv string) bool { return strings.HasPrefix(kv, o+"=") }) {
					fmt.Fprintf(out, "orch run: not passing on %s, which would override the space's %s\n", o, k)
					drop = append(drop, o)
				}
			}
			continue
		}
		for _, kv := range environ {
			if key, v, _ := strings.Cut(kv, "="); key == k && slices.ContainsFunc(others, func(o string) bool { return samePath(o, v) }) {
				fmt.Fprintf(out, "orch run: not passing on %s, which names another space's folder; using the default\n", k)
				drop = append(drop, k)
			}
		}
	}
	env := withoutKeys(environ, drop)
	keys := make([]string, 0, len(spaceEnv))
	for k := range spaceEnv {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		env = append(env, k+"="+spaceEnv[k])
	}
	return env
}

// otherLogins are the gh profiles and Claude Code folders the spaces other
// than space name.
func otherLogins(res *spaces.Resolved, space string) []string {
	var out []string
	for _, n := range res.Names() {
		if n == space {
			continue
		}
		for _, d := range []string{res.GHConfigDir(n), res.ClaudeConfigDir(n)} {
			if d != "" {
				out = append(out, d)
			}
		}
	}
	return out
}

// samePath compares folders as placement does, case-insensitively since
// macOS volumes usually are, both as written and with links resolved.
// b, an inherited value, is made absolute first.
func samePath(a, b string) bool {
	if b == "" {
		return false
	}
	if abs, err := filepath.Abs(b); err == nil {
		b = abs
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) || strings.EqualFold(resolved(a), resolved(b))
}

// resolved is p with its links resolved, or p cleaned when it cannot be.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// isSettingsFlag reports whether arg is Claude Code's --settings option.
func isSettingsFlag(arg string) bool {
	return arg == "--settings" || strings.HasPrefix(arg, "--settings=")
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

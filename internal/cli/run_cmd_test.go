package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/settings/spacesettings"
)

// runClient answers the three calls the launcher makes.
type runClient struct {
	orchdv1.OrchdClient
	applied   string
	placement *orchdv1.PlacementExplanation
	sandbox   string
	// setting is the sandbox setting orchd reports; "" reports none.
	setting  string
	diffErr  error
	enfErr   error
	placeErr error
}

func (c *runClient) DiffSpaces(context.Context, *orchdv1.DiffSpacesRequest, ...grpc.CallOption) (*orchdv1.SpacesDiff, error) {
	if c.diffErr != nil {
		return nil, c.diffErr
	}
	if c.applied == "" {
		return &orchdv1.SpacesDiff{}, nil
	}
	return &orchdv1.SpacesDiff{AppliedRevision: 3, AppliedJson: c.applied}, nil
}

func (c *runClient) ExplainPlacement(context.Context, *orchdv1.ExplainPlacementRequest, ...grpc.CallOption) (*orchdv1.PlacementExplanation, error) {
	return c.placement, c.placeErr
}

func (c *runClient) GetEnforcement(context.Context, *orchdv1.GetEnforcementRequest, ...grpc.CallOption) (*orchdv1.EnforcementState, error) {
	if c.enfErr != nil {
		return nil, c.enfErr
	}
	st := &orchdv1.EnforcementState{Switches: []*orchdv1.EnforcementSwitch{
		{Layer: "guard", State: "block-other-jobs"}, {Layer: "sandbox", State: c.sandbox},
	}}
	if c.setting != "" {
		st.SandboxSetting = &orchdv1.SandboxSetting{Mode: c.setting}
	}
	return st, nil
}

// launch is one launcher run with its exec captured.
type launch struct {
	path      string
	argv, env []string
	out       string
	err       error
	layout    spacesettings.Layout
}

// parentEnv is the environment the launcher inherits in tests.
var parentEnv = []string{"PATH=/usr/bin", "GH_CONFIG_DIR=/parent/gh", "TMPDIR=/parent/tmp", "CLAUDE_CONFIG_DIR=/parent/claude"}

// runWorld is a temp home with job roots code/a (joba, which names its own
// gh profile gh/a and Claude Code folder claude/a) and code/b (jobb, which
// names neither and so uses the default logins, D-59).
func runWorld(t *testing.T) (home, policy string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy = fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q],"gh_config_dir":%q,"claude_config_dir":%q},{"name":"jobb","kind":"isolated","roots":[%q]}]}`,
		filepath.Join(home, "code", "a"), filepath.Join(home, "gh", "a"), filepath.Join(home, "claude", "a"), filepath.Join(home, "code", "b"))
	return home, policy
}

// claudeSettings is the --settings JSON the launcher gave Claude Code, ""
// when none.
func claudeSettings(argv []string) string {
	for i, a := range argv {
		if a == "--settings" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func doLaunch(t *testing.T, home string, client *runClient, args ...string) launch {
	t.Helper()
	return doLaunchIn(t, home, spacesettings.Layout{Dir: filepath.Join(home, "data", "spaces")}, client, args...)
}

// doLaunchIn is doLaunch with the per-space folders in layout.
func doLaunchIn(t *testing.T, home string, layout spacesettings.Layout, client *runClient, args ...string) launch {
	t.Helper()
	return doLaunchEnv(t, home, layout, parentEnv, client, args...)
}

// doLaunchEnv is doLaunchIn with environ as the inherited environment.
func doLaunchEnv(t *testing.T, home string, layout spacesettings.Layout, environ []string, client *runClient, args ...string) launch {
	t.Helper()
	var got launch
	got.layout = layout
	l := &launcher{
		cf:       func() (orchdv1.OrchdClient, func(), error) { return client, func() {}, nil },
		layout:   got.layout,
		getwd:    func() (string, error) { return filepath.Join(home, "scratch"), nil },
		environ:  func() []string { return slices.Clone(environ) },
		lookPath: func(name string) (string, error) { return "/bin/" + filepath.Base(name), nil },
		exec: func(path string, argv, env []string) error {
			got.path, got.argv, got.env = path, argv, env
			return nil
		},
	}
	cmd := newRunCmdWith(l)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	got.err = cmd.Execute()
	got.out = out.String()
	return got
}

func envOf(env []string, key string) []string {
	var vals []string
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			vals = append(vals, v)
		}
	}
	return vals
}

func unmatched() *orchdv1.PlacementExplanation {
	return &orchdv1.PlacementExplanation{Outcome: "quarantined", Rule: "unmatched", Space: "quarantine"}
}

// TestRunSetsProfileForChildOnly (SP-046.2.a1, D-59): 'orch run <space> --
// claude' gives the harness the space's TMPDIR, replacing an inherited one,
// and its rules with --settings; CLAUDE_CONFIG_DIR and GH_CONFIG_DIR are set
// (replacing inherited values) only when the space names its own, and
// orch's own environment is left alone.
func TestRunSetsProfileForChildOnly(t *testing.T) {
	home, policy := runWorld(t)
	t.Setenv("GH_CONFIG_DIR", "/parent/gh")
	t.Setenv("CLAUDE_CONFIG_DIR", "/parent/claude")
	client := &runClient{applied: policy, placement: unmatched(), sandbox: "on"}
	got := doLaunch(t, home, client, "joba", "--", "claude", "-p", "hi")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if v := envOf(got.env, "CLAUDE_CONFIG_DIR"); !slices.Equal(v, []string{filepath.Join(home, "claude", "a")}) {
		t.Errorf("CLAUDE_CONFIG_DIR = %v", v)
	}
	if v := envOf(got.env, "GH_CONFIG_DIR"); !slices.Equal(v, []string{filepath.Join(home, "gh", "a")}) {
		t.Errorf("GH_CONFIG_DIR = %v", v)
	}
	if v := envOf(got.env, "TMPDIR"); !slices.Equal(v, []string{got.layout.TmpDir("joba")}) {
		t.Errorf("TMPDIR = %v", v)
	}
	if v := envOf(got.env, "CLAUDE_CODE_TMPDIR"); !slices.Equal(v, []string{got.layout.TmpDir("joba")}) {
		t.Errorf("CLAUDE_CODE_TMPDIR = %v", v)
	}
	if v := envOf(got.env, "PATH"); !slices.Equal(v, []string{"/usr/bin"}) {
		t.Errorf("PATH not passed on: %v", v)
	}
	if os.Getenv("GH_CONFIG_DIR") != "/parent/gh" || os.Getenv("CLAUDE_CONFIG_DIR") != "/parent/claude" {
		t.Error("the launcher changed its own environment")
	}
	settings := claudeSettings(got.argv)
	if got.path != "/bin/claude" || len(got.argv) != 5 || got.argv[0] != "claude" || got.argv[1] != "--settings" || !slices.Equal(got.argv[3:], []string{"-p", "hi"}) {
		t.Errorf("exec %s %q", got.path, got.argv)
	}
	if !strings.Contains(settings, "Read(/"+filepath.Join(home, "code", "b")+"/**)") || !strings.Contains(settings, `"allowUnsandboxedCommands":false`) {
		t.Errorf("--settings = %s", settings)
	}
	if !strings.Contains(got.out, "Claude Code configuration "+filepath.Join(home, "claude", "a")) {
		t.Errorf("output %q", got.out)
	}
	if info, err := os.Stat(got.layout.TmpDir("joba")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("tmp dir: %v %v", info, err)
	}

	// jobb names neither: the session keeps the user's own logins, as
	// inherited, and its rules deny joba's gh profile and Claude folder.
	got = doLaunch(t, home, client, "jobb", "--", "claude")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if v := envOf(got.env, "CLAUDE_CONFIG_DIR"); !slices.Equal(v, []string{"/parent/claude"}) {
		t.Errorf("default CLAUDE_CONFIG_DIR = %v", v)
	}
	if v := envOf(got.env, "GH_CONFIG_DIR"); !slices.Equal(v, []string{"/parent/gh"}) {
		t.Errorf("default GH_CONFIG_DIR = %v", v)
	}
	if v := envOf(got.env, "TMPDIR"); !slices.Equal(v, []string{got.layout.TmpDir("jobb")}) {
		t.Errorf("jobb TMPDIR = %v", v)
	}
	settings = claudeSettings(got.argv)
	for _, d := range []string{filepath.Join(home, "gh", "a"), filepath.Join(home, "claude", "a")} {
		if !strings.Contains(settings, "Read(/"+d+"/**)") {
			t.Errorf("jobb's --settings do not deny %s: %s", d, settings)
		}
	}
	if !strings.Contains(got.out, "your own Claude Code configuration") {
		t.Errorf("output %q", got.out)
	}

	// With nothing inherited, nothing is set.
	got = doLaunchEnv(t, home, got.layout, []string{"PATH=/usr/bin"}, client, "jobb", "--", "claude")
	if got.err != nil || len(envOf(got.env, "CLAUDE_CONFIG_DIR")) != 0 || len(envOf(got.env, "GH_CONFIG_DIR")) != 0 {
		t.Errorf("unnamed logins set: %v %q", got.err, got.env)
	}
}

// TestRunDropsOtherSpaceLogin (D-59): an inherited CLAUDE_CONFIG_DIR or
// GH_CONFIG_DIR naming another space's folder is not passed on to a space
// that names none, so the session cannot run as that space's account.
func TestRunDropsOtherSpaceLogin(t *testing.T) {
	home, policy := runWorld(t)
	environ := []string{"PATH=/usr/bin", "GH_CONFIG_DIR=" + filepath.Join(home, "GH", "a") + "/", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude", "a")}
	got := doLaunchEnv(t, home, spacesettings.Layout{Dir: filepath.Join(home, "data", "spaces")}, environ,
		&runClient{applied: policy, placement: unmatched(), sandbox: "on"}, "jobb", "--", "codex")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(envOf(got.env, "GH_CONFIG_DIR")) != 0 || len(envOf(got.env, "CLAUDE_CONFIG_DIR")) != 0 {
		t.Errorf("another space's login passed on: %q", got.env)
	}
	if strings.Count(got.out, "names another space's folder") != 2 {
		t.Errorf("output %q", got.out)
	}
}

// TestRunDropsLoginOverrides: when the space names its own login folders,
// inherited variables that would override them (GH_TOKEN and the like,
// ANTHROPIC_API_KEY and the like) are not passed on; for a space naming
// none they stay, as the user's own login.
func TestRunDropsLoginOverrides(t *testing.T) {
	home, policy := runWorld(t)
	environ := []string{"PATH=/usr/bin", "GH_TOKEN=x", "GH_HOST=h", "ANTHROPIC_API_KEY=y", "CLAUDE_CODE_OAUTH_TOKEN=z"}
	layout := spacesettings.Layout{Dir: filepath.Join(home, "data", "spaces")}
	client := &runClient{applied: policy, placement: unmatched(), sandbox: "on"}
	got := doLaunchEnv(t, home, layout, environ, client, "joba", "--", "claude")
	if got.err != nil {
		t.Fatal(got.err)
	}
	for _, k := range []string{"GH_TOKEN", "GH_HOST", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if len(envOf(got.env, k)) != 0 {
			t.Errorf("%s passed on to a space with its own login", k)
		}
		if !strings.Contains(got.out, "not passing on "+k) {
			t.Errorf("no note for %s: %q", k, got.out)
		}
	}
	if strings.Contains(got.out, "=x") || strings.Contains(got.out, "=y") {
		t.Errorf("a value was printed: %q", got.out)
	}
	got = doLaunchEnv(t, home, layout, environ, client, "jobb", "--", "claude")
	if got.err != nil || len(envOf(got.env, "GH_TOKEN")) != 1 || len(envOf(got.env, "ANTHROPIC_API_KEY")) != 1 {
		t.Errorf("default logins changed: %v %q", got.err, got.env)
	}
}

// TestSamePath: an inherited folder matches another space's by case, by a
// trailing slash, through a link and from a relative path.
func TestSamePath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "gh")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, v := range []string{real, strings.ToUpper(real) + "/", link, "gh", "./link"} {
		if !samePath(real, v) {
			t.Errorf("samePath(%q, %q) = false", real, v)
		}
	}
	for _, v := range []string{"", filepath.Join(dir, "other"), "/"} {
		if samePath(real, v) {
			t.Errorf("samePath(%q, %q) = true", real, v)
		}
	}
}

// TestRunRefusesUserSettings: with the rules on, a --settings of the user's
// own would replace the space's, so it is refused; with the sandbox layer
// off it is passed on.
func TestRunRefusesUserSettings(t *testing.T) {
	home, policy := runWorld(t)
	for _, arg := range [][]string{{"--settings", "x.json"}, {"--settings=x.json"}} {
		got := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "on"}, append([]string{"joba", "--", "claude"}, arg...)...)
		if got.err == nil || !strings.Contains(got.err.Error(), "--settings") || got.path != "" {
			t.Errorf("%q: %v", arg, got.err)
		}
	}
	got := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "off"}, "joba", "--", "claude", "--settings", "x.json")
	if got.err != nil || !slices.Equal(got.argv, []string{"claude", "--settings", "x.json"}) {
		t.Errorf("off: %v %q", got.err, got.argv)
	}
}

// TestRunClaudeTmp (SP-046.1.a6): with the temp folders under a short root,
// as DefaultLayout has them, the launched session's CLAUDE_CODE_TMPDIR is
// one Claude Code uses, and its --settings deny the shared folder sessions
// started without the launcher use. A folder too long for Claude Code is
// reported and the shared folder is left alone.
func TestRunClaudeTmp(t *testing.T) {
	home, policy := runWorld(t)
	root, err := os.MkdirTemp("/tmp", "oe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	layout := spacesettings.Layout{Dir: filepath.Join(home, "data", "spaces"), Tmp: filepath.Join(root, "r")}
	got := doLaunchIn(t, home, layout, &runClient{applied: policy, placement: unmatched(), sandbox: "on"}, "joba", "--", "claude")
	if got.err != nil {
		t.Fatal(got.err)
	}
	tmp := layout.TmpDir("joba")
	if v := envOf(got.env, "CLAUDE_CODE_TMPDIR"); !slices.Equal(v, []string{tmp}) || spacesettings.ClaudeSandboxTmp(tmp) == spacesettings.SharedClaudeTmp() {
		t.Errorf("CLAUDE_CODE_TMPDIR = %v", v)
	}
	if info, err := os.Lstat(tmp); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("tmp dir: %v %v", info, err)
	}
	if settings := claudeSettings(got.argv); !strings.Contains(settings, "Read(/"+spacesettings.SharedClaudeTmp()+"/**)") {
		t.Errorf("--settings do not deny the shared folder: %s", settings)
	}
	if strings.Contains(got.out, "too long") {
		t.Errorf("warned: %s", got.out)
	}
	long := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "on"}, "joba", "--", "claude")
	if long.err != nil || !strings.Contains(long.out, "too long for Claude Code") {
		t.Errorf("long folder: %v %q", long.err, long.out)
	}
}

// TestRunRefusesUnknownSpace (SP-046.2.a1): a space not in the applied
// policy, or no applied policy at all, is refused and nothing starts.
func TestRunRefusesUnknownSpace(t *testing.T) {
	home, policy := runWorld(t)
	got := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "on"}, "jobz", "--", "claude")
	if got.err == nil || !strings.Contains(got.err.Error(), `unknown space "jobz"`) || got.path != "" {
		t.Errorf("unknown space: %v, exec %q", got.err, got.path)
	}
	got = doLaunch(t, home, &runClient{placement: unmatched()}, "joba", "--", "claude")
	if got.err == nil || !strings.Contains(got.err.Error(), "no spaces policy") || got.path != "" {
		t.Errorf("no policy: %v", got.err)
	}
	got = doLaunch(t, home, &runClient{diffErr: errors.New("permission denied")}, "joba", "--", "claude")
	if got.err == nil || got.path != "" {
		t.Errorf("orchd refused: %v", got.err)
	}
	got = doLaunch(t, home, &runClient{applied: `{"spaces":[{"name":"x","kind":"weird"}]}`}, "joba", "--", "claude")
	if got.err == nil || got.path != "" {
		t.Errorf("bad policy: %v", got.err)
	}
	got = doLaunch(t, home, &runClient{applied: `{"nope":1}`}, "joba", "--", "claude")
	if got.err == nil || got.path != "" {
		t.Errorf("unparseable policy: %v", got.err)
	}
}

// TestRunRefusesOtherSpaceFolder (SP-046.2.a1): a folder another space
// holds, or one whose space cannot be decided, is refused unless --force; a
// folder of the space itself is allowed.
func TestRunRefusesOtherSpaceFolder(t *testing.T) {
	home, policy := runWorld(t)
	other := &orchdv1.PlacementExplanation{Outcome: "placed", Rule: "root", Space: "jobb"}
	got := doLaunch(t, home, &runClient{applied: policy, placement: other, sandbox: "on"}, "joba", "--", "claude")
	if got.err == nil || !strings.Contains(got.err.Error(), "in space jobb") || got.path != "" {
		t.Errorf("other space: %v", got.err)
	}
	conflict := &orchdv1.PlacementExplanation{Outcome: "conflict", Rule: "conflict", Reason: "root and remote differ", Space: "quarantine"}
	got = doLaunch(t, home, &runClient{applied: policy, placement: conflict, sandbox: "on"}, "joba", "--", "claude")
	if got.err == nil || !strings.Contains(got.err.Error(), "cannot be decided") || got.path != "" {
		t.Errorf("conflict: %v", got.err)
	}
	got = doLaunch(t, home, &runClient{applied: policy, placement: other, sandbox: "on"}, "--force", "joba", "--", "claude")
	if got.err != nil || got.path != "/bin/claude" {
		t.Errorf("forced: %v", got.err)
	}
	mine := &orchdv1.PlacementExplanation{Outcome: "placed", Rule: "root", Space: "joba"}
	if got := doLaunch(t, home, &runClient{applied: policy, placement: mine, sandbox: "on"}, "joba", "--", "claude"); got.err != nil {
		t.Errorf("own space: %v", got.err)
	}
	got = doLaunch(t, home, &runClient{applied: policy, placeErr: errors.New("down"), sandbox: "on"}, "joba", "--", "claude")
	if got.err == nil || got.path != "" {
		t.Errorf("placement error: %v", got.err)
	}
}

// TestRunCodex: Codex gets the space's permissions profile as -c overrides
// before its own arguments, and its environment.
func TestRunCodex(t *testing.T) {
	home, policy := runWorld(t)
	got := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "on"}, "joba", "--", "/usr/local/bin/codex", "exec", "hi")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.argv) != 7 || got.argv[0] != "/usr/local/bin/codex" || got.argv[1] != "-c" || got.argv[2] != `default_permissions="orch_joba"` ||
		!strings.HasPrefix(got.argv[4], `permissions.orch_joba={extends=":workspace",filesystem={`) || !slices.Equal(got.argv[5:], []string{"exec", "hi"}) {
		t.Errorf("argv = %q", got.argv)
	}
	if !strings.Contains(got.argv[4], fmt.Sprintf("%q=\"deny\"", filepath.Join(home, "code", "b"))) {
		t.Errorf("no deny of job b: %s", got.argv[4])
	}
	if v := envOf(got.env, "CLAUDE_CONFIG_DIR"); !slices.Equal(v, []string{"/parent/claude"}) {
		t.Errorf("codex got the space's CLAUDE_CONFIG_DIR: %v", v)
	}
	if v := envOf(got.env, "TMPDIR"); !slices.Equal(v, []string{got.layout.TmpDir("joba")}) {
		t.Errorf("TMPDIR = %v", v)
	}
}

// TestRunSandboxOff: with the sandbox layer off the session gets no deny
// rules (its environment stays), and the operator is told.
func TestRunSandboxOff(t *testing.T) {
	home, policy := runWorld(t)
	got := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "off"}, "joba", "--", "codex")
	if got.err != nil || !slices.Equal(got.argv, []string{"codex"}) || !strings.Contains(got.out, "switched off") {
		t.Errorf("codex off: %v %q %q", got.err, got.argv, got.out)
	}
	got = doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "off"}, "joba", "--", "claude")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !slices.Equal(got.argv, []string{"claude"}) || len(envOf(got.env, "GH_CONFIG_DIR")) != 1 || len(envOf(got.env, "CLAUDE_CONFIG_DIR")) != 1 {
		t.Errorf("off: %q %q", got.argv, got.env)
	}
	// orchd not answering about the switches applies the rules.
	got = doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), enfErr: errors.New("x")}, "joba", "--", "codex")
	if got.err != nil || len(got.argv) != 5 {
		t.Errorf("unknown switch: %v %q", got.err, got.argv)
	}
}

// TestRunUsage: only claude and codex start, and the harness follows --.
func TestRunUsage(t *testing.T) {
	home, policy := runWorld(t)
	c := &runClient{applied: policy, placement: unmatched(), sandbox: "on"}
	for _, args := range [][]string{{"joba", "claude"}, {"joba", "--"}, {"--", "claude"}, {"joba", "x", "--", "claude"}} {
		if got := doLaunch(t, home, c, args...); got.err == nil || !strings.Contains(got.err.Error(), "usage") {
			t.Errorf("%q: %v", args, got.err)
		}
	}
	if got := doLaunch(t, home, c, "joba", "--", "bash"); got.err == nil || !strings.Contains(got.err.Error(), "claude or codex") {
		t.Errorf("bash: %v", got.err)
	}
}

// TestRunFollowsSandboxSetting (SP-046.4.a2, D-61): the launcher writes the
// rules as the checkouts get them. With auto it gives them only when another
// isolated space's folders are denied; with off never; the space's gh
// profile and temp folder are set either way. With the rules, a space on the
// default gh login may read that folder and one naming its own profile is
// denied it (RC-208 option a).
func TestRunFollowsSandboxSetting(t *testing.T) {
	home, policy := runWorld(t)
	def := filepath.Join(home, ".config", "gh")
	layout := spacesettings.Layout{Dir: filepath.Join(home, "data", "spaces"), GHDefault: def}

	got := doLaunchIn(t, home, layout, &runClient{applied: policy, placement: unmatched(), sandbox: "on", setting: "auto"}, "jobb", "--", "claude")
	if got.err != nil {
		t.Fatal(got.err)
	}
	set := claudeSettings(got.argv)
	if !strings.Contains(set, `"allowRead":["`+def+`"]`) || !strings.Contains(set, `"enableWeakerNetworkIsolation":true`) ||
		!strings.Contains(set, `"allowedDomains":["api.github.com","github.com"]`) {
		t.Errorf("auto, two jobs, default login: %s", set)
	}
	got = doLaunchIn(t, home, layout, &runClient{applied: policy, placement: unmatched(), sandbox: "on", setting: "auto"}, "joba", "--", "claude")
	if set := claudeSettings(got.argv); !strings.Contains(set, `"Read(/`+def+`/**)"`) || strings.Contains(set, `"allowRead":["`+def) {
		t.Errorf("auto, own profile: %s", set)
	}

	solo := fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q],"gh_config_dir":%q}]}`,
		filepath.Join(home, "code", "a"), filepath.Join(home, "gh", "a"))
	for _, tt := range []struct {
		setting, says string
	}{
		{"auto", "no other isolated space's folders to deny"},
		{"off", "the sandbox setting is off"},
	} {
		got := doLaunchIn(t, home, layout, &runClient{applied: solo, placement: unmatched(), sandbox: "on", setting: tt.setting}, "joba", "--", "claude")
		if got.err != nil || claudeSettings(got.argv) != "" || !strings.Contains(got.out, tt.says) {
			t.Errorf("%s: %v %q %q", tt.setting, got.err, got.argv, got.out)
		}
		if gh := envOf(got.env, "GH_CONFIG_DIR"); len(gh) != 1 || gh[0] != filepath.Join(home, "gh", "a") || len(envOf(got.env, "TMPDIR")) != 1 {
			t.Errorf("%s: env %q", tt.setting, got.env)
		}
		got = doLaunchIn(t, home, layout, &runClient{applied: solo, placement: unmatched(), sandbox: "on", setting: tt.setting}, "joba", "--", "codex")
		if got.err != nil || !slices.Equal(got.argv, []string{"codex"}) {
			t.Errorf("%s codex: %v %q", tt.setting, got.err, got.argv)
		}
	}
	got = doLaunchIn(t, home, layout, &runClient{applied: solo, placement: unmatched(), sandbox: "on", setting: "on"}, "joba", "--", "claude")
	if got.err != nil || claudeSettings(got.argv) == "" {
		t.Errorf("on, one job: %v %q", got.err, got.argv)
	}
}

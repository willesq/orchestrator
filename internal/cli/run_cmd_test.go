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
	diffErr   error
	enfErr    error
	placeErr  error
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
	return &orchdv1.EnforcementState{Switches: []*orchdv1.EnforcementSwitch{
		{Layer: "guard", State: "block-other-jobs"}, {Layer: "sandbox", State: c.sandbox},
	}}, nil
}

// launch is one launcher run with its exec captured.
type launch struct {
	path       string
	argv, env  []string
	out        string
	err        error
	userClaude string
	layout     spacesettings.Layout
}

// runWorld is a temp home with job roots code/a (joba, gh profile gh/a)
// and code/b (jobb).
func runWorld(t *testing.T) (home, policy string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy = fmt.Sprintf(`{"spaces":[{"name":"joba","kind":"isolated","roots":[%q],"gh_config_dir":%q},{"name":"jobb","kind":"isolated","roots":[%q]}]}`,
		filepath.Join(home, "code", "a"), filepath.Join(home, "gh", "a"), filepath.Join(home, "code", "b"))
	return home, policy
}

func doLaunch(t *testing.T, home string, client *runClient, args ...string) launch {
	t.Helper()
	var got launch
	got.userClaude = filepath.Join(home, "dot-claude")
	got.layout = spacesettings.Layout{Dir: filepath.Join(home, "data", "spaces")}
	l := &launcher{
		cf:        func() (orchdv1.OrchdClient, func(), error) { return client, func() {}, nil },
		layout:    got.layout,
		claudeDir: func() (string, error) { return got.userClaude, nil },
		getwd:     func() (string, error) { return filepath.Join(home, "scratch"), nil },
		environ: func() []string {
			return []string{"PATH=/usr/bin", "HOME=" + home, "GH_CONFIG_DIR=/parent/gh", "TMPDIR=/parent/tmp", "CLAUDE_CONFIG_DIR=/parent/claude"}
		},
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

// TestRunSetsProfileForChildOnly (SP-046.2.a1): 'orch run <space> --
// claude' gives the harness the space's CLAUDE_CONFIG_DIR, GH_CONFIG_DIR and
// TMPDIR, replacing inherited values, and leaves orch's own environment
// alone.
func TestRunSetsProfileForChildOnly(t *testing.T) {
	home, policy := runWorld(t)
	t.Setenv("GH_CONFIG_DIR", "/parent/gh")
	got := doLaunch(t, home, &runClient{applied: policy, placement: unmatched(), sandbox: "on"}, "joba", "--", "claude", "-p", "hi")
	if got.err != nil {
		t.Fatal(got.err)
	}
	wantDir := got.layout.ClaudeDir("joba")
	if v := envOf(got.env, "CLAUDE_CONFIG_DIR"); !slices.Equal(v, []string{wantDir}) {
		t.Errorf("CLAUDE_CONFIG_DIR = %v, want [%s]", v, wantDir)
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
	if os.Getenv("GH_CONFIG_DIR") != "/parent/gh" || os.Getenv("CLAUDE_CONFIG_DIR") == wantDir {
		t.Error("the launcher changed its own environment")
	}
	if got.path != "/bin/claude" || !slices.Equal(got.argv, []string{"claude", "-p", "hi"}) {
		t.Errorf("exec %s %v", got.path, got.argv)
	}
	settings, err := os.ReadFile(filepath.Join(wantDir, "settings.json"))
	if err != nil || !strings.Contains(string(settings), "Read(/"+filepath.Join(home, "code", "b")+"/**)") {
		t.Errorf("profile settings: %s %v", settings, err)
	}
	if info, err := os.Stat(got.layout.TmpDir("joba")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("tmp dir: %v %v", info, err)
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
	if len(envOf(got.env, "CLAUDE_CONFIG_DIR")) != 0 {
		t.Errorf("codex got CLAUDE_CONFIG_DIR")
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
	b, _ := os.ReadFile(filepath.Join(got.layout.ClaudeDir("joba"), "settings.json"))
	if strings.Contains(string(b), "Read(") || len(envOf(got.env, "GH_CONFIG_DIR")) != 1 {
		t.Errorf("off profile: %s", b)
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

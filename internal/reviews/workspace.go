package reviews

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
)

// Where a review's project setup (CLAUDE.md, skills, agents, settings) comes
// from. The PR's own commits are other people's, so by default the reviewer
// runs with the repo's default branch, which was reviewed before it merged.
const (
	FromDefault = "default"
	FromBase    = "base"
	FromHead    = "head"
	FromNone    = "none"
)

// What a review's diff is taken against.
const (
	CompareBase    = "base"    // the PR's own changes
	CompareDefault = "default" // everything the PR would bring to the default branch, its stack included
)

// Whether a review runs with the repo's own Claude Code settings, which
// carry its hooks, MCP servers, plugins and permissions.
const (
	HooksAuto = ""    // on for a repo whose owner is the user or bound to one of their spaces
	HooksOn   = "on"  // always
	HooksOff  = "off" // never: instructions, skills and agents only
)

// projectSettings are the files through which a checkout makes Claude Code run
// its code: hooks, MCP servers, plugins, helpers. A checkout whose settings
// are off has them removed before the session starts.
var projectSettings = []string{".claude/settings.json", ".claude/settings.local.json", ".mcp.json"}

// Setup is how a PR's reviews are set up. Its zero value is the default.
type Setup struct {
	InstructionsFrom string
	CompareAgainst   string
	ProjectHooks     string
}

// Normalized fills in the defaults, so the stored and shown value is explicit.
func (s Setup) Normalized() Setup {
	if s.InstructionsFrom == "" {
		s.InstructionsFrom = FromDefault
	}
	if s.CompareAgainst == "" {
		s.CompareAgainst = CompareBase
	}
	return s
}

// Validate reports a value no review can be set up with.
func (s Setup) Validate() error {
	switch s.InstructionsFrom {
	case "", FromDefault, FromBase, FromHead, FromNone:
	default:
		return fmt.Errorf("instructions_from must be %s, %s, %s or %s", FromDefault, FromBase, FromHead, FromNone)
	}
	switch s.CompareAgainst {
	case "", CompareBase, CompareDefault:
	default:
		return fmt.Errorf("compare_against must be %s or %s", CompareBase, CompareDefault)
	}
	switch s.ProjectHooks {
	case HooksAuto, HooksOn, HooksOff:
	default:
		return fmt.Errorf("project_hooks must be empty (automatic), %s or %s", HooksOn, HooksOff)
	}
	return nil
}

// CheckoutSpec is what one review needs checked out.
type CheckoutSpec struct {
	Repo       string // owner/name
	Head, Base string // the PR's head and base commits
	From       string // FromDefault, FromBase, FromHead or FromNone
	Compare    string // CompareBase or CompareDefault
	Hooks      bool   // keep the setup checkout's settings, hooks and MCP servers
}

// Checkout is a review's prepared workspace and how its session runs in it.
type Checkout struct {
	// Dir holds everything below; it is removed once its task is done.
	Dir string
	// Cwd is the session's working directory: the setup checkout, so its
	// CLAUDE.md (with its imports), skills and agents load, or an empty
	// directory when there is none.
	Cwd string
	// HeadDir is the PR's head commit. It is never under Cwd: Claude Code
	// would load the PR author's instructions and skills from it.
	HeadDir string
	// SetupRef and SetupSHA name what the project setup came from; both are
	// empty with FromNone.
	SetupRef, SetupSHA string
	// CompareRef and CompareSHA are what the diff is taken against.
	CompareRef, CompareSHA string
	// Hooks is whether the setup checkout keeps its settings, hooks and MCP
	// servers.
	Hooks bool
	// Note says how the setup differs from what the user chose, and why.
	Note string
}

// GitRunner runs git in dir with env (nil: orchd's own) and returns stdout.
type GitRunner func(ctx context.Context, env []string, dir string, args ...string) ([]byte, error)

// Checkouts prepares review workspaces from a cache of each repo that orchd
// owns, so a review never clones on its own nor touches the user's checkouts.
type Checkouts struct {
	// Root holds repos/<owner>/<name>.git caches and work/<id> workspaces.
	Root string
	Git  GitRunner
	// Env is the environment for fetching owner's repos: nil for the default
	// GitHub login, or a gh profile's.
	Env func(ctx context.Context, owner string) ([]string, error)
}

// workID names a workspace directory; Sweep removes only names of this shape.
var workID = regexp.MustCompile(`^[0-9a-f]{24}$`)

// refName is a branch name safe to put in a refspec: what git accepts, minus
// anything that could read as an option or a second refspec.
var refName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,250}$`)

// credentialArgs make gh, as the environment's account, the only credential
// helper, as the docs sync does.
var credentialArgs = []string{"-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}

// safeArgs keep repo content from running code or reaching outside its
// checkout while orchd handles it: no hooks, no filesystem monitor, no LFS
// downloads from a URL the repo names, symlinks checked out as plain files,
// and no automatic gc pruning commits a clone still borrows. A clone keeps
// them in its own config, so they hold for the review session's git too.
var safeArgs = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.symlinks=false",
	"-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false", "-c", "gc.auto=0"}

// Prepare fetches what spec needs into the repo's cache and checks it out
// under a new workspace. On error nothing is left behind.
func (c *Checkouts) Prepare(ctx context.Context, spec CheckoutSpec) (co Checkout, err error) {
	owner, name, ok := strings.Cut(spec.Repo, "/")
	if !ok || !github.ValidLogin(owner) || !github.ValidRepo(name) {
		return Checkout{}, fmt.Errorf("invalid GitHub repo %q", spec.Repo)
	}
	if !sha(spec.Head) || !sha(spec.Base) {
		return Checkout{}, errors.New("the PR's head or base commit is unknown")
	}
	var env []string
	if c.Env != nil {
		if env, err = c.Env(ctx, owner); err != nil {
			return Checkout{}, err
		}
	}
	cache, err := c.cache(ctx, env, owner, name)
	if err != nil {
		return Checkout{}, err
	}
	def, err := c.defaultBranch(ctx, env, cache)
	if err != nil {
		return Checkout{}, err
	}
	fetch := append(append(append([]string{}, credentialArgs...), safeArgs...), "fetch", "--no-tags", "--quiet", "origin",
		"+refs/heads/"+def+":refs/orch/default", spec.Base, spec.Head)
	if _, err := c.Git(ctx, env, cache, fetch...); err != nil {
		return Checkout{}, fmt.Errorf("fetch %s: %w", spec.Repo, err)
	}
	defSHA, err := c.revParse(ctx, cache, "refs/orch/default")
	if err != nil {
		return Checkout{}, err
	}

	id := NewNonce()
	co.Dir = filepath.Join(c.Root, "work", id)
	if err := os.MkdirAll(co.Dir, 0o700); err != nil {
		return Checkout{}, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(co.Dir)
			co = Checkout{}
		}
	}()
	co.HeadDir = filepath.Join(co.Dir, "head")
	if err = c.checkout(ctx, env, cache, co.HeadDir, spec.Head); err != nil {
		return co, err
	}
	switch spec.From {
	case FromBase:
		co.SetupRef, co.SetupSHA = "PR base", spec.Base
	case FromHead:
		co.SetupRef, co.SetupSHA = "PR head", spec.Head
	case FromNone:
	default:
		co.SetupRef, co.SetupSHA = def, defSHA
	}
	co.CompareRef, co.CompareSHA = "PR base", spec.Base
	if spec.Compare == CompareDefault {
		co.CompareRef, co.CompareSHA = def, defSHA
	}

	session := filepath.Join(co.Dir, "session")
	if err = os.Mkdir(session, 0o700); err != nil {
		return co, err
	}
	co.Cwd = session
	if co.SetupSHA == "" {
		return co, nil
	}
	setup := filepath.Join(co.Dir, "setup")
	if err = c.checkout(ctx, env, cache, setup, co.SetupSHA); err != nil {
		return co, err
	}
	co.Hooks, co.Cwd = spec.Hooks, setup
	err = stripSetup(setup, spec.Hooks)
	return co, err
}

// cache returns the repo's bare cache, creating it on first use.
func (c *Checkouts) cache(ctx context.Context, env []string, owner, name string) (string, error) {
	dir := filepath.Join(c.Root, "repos", owner, name+".git")
	url := "https://github.com/" + owner + "/" + name + ".git"
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil && c.cacheConfigOK(ctx, dir, url) {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	// A half-made cache from an interrupted run would fail every later fetch.
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", dir},
		{"-C", dir, "remote", "add", "origin", url},
	} {
		if _, err := c.Git(ctx, env, "", args...); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("create the cache of %s/%s: %w", owner, name, err)
		}
	}
	return dir, nil
}

// cacheKeys are the config a cache is made with, and the most it may hold.
var cacheKeys = map[string]bool{
	"core.repositoryformatversion": true, "core.filemode": true, "core.bare": true, "core.ignorecase": true,
	"core.precomposeunicode": true, "core.logallrefupdates": true, "remote.origin.fetch": true,
}

// cacheConfigOK reports whether the cache's config is still only what orchd
// wrote. A review session runs as the user and could change it (a proxy, an
// include, another URL) to steer orchd's next credentialed fetch, so a cache
// with anything else is made again.
func (c *Checkouts) cacheConfigOK(ctx context.Context, dir, url string) bool {
	out, err := c.Git(ctx, nil, "", "config", "--file", filepath.Join(dir, "config"), "--list")
	if err != nil {
		return false
	}
	for _, kv := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(kv, "=")
		if k == "remote.origin.url" && v == url {
			continue
		}
		if !cacheKeys[k] {
			return false
		}
	}
	return true
}

// defaultBranch asks GitHub which branch the repo's HEAD names.
func (c *Checkouts) defaultBranch(ctx context.Context, env []string, cache string) (string, error) {
	args := append(append([]string{}, credentialArgs...), "ls-remote", "--symref", "origin", "HEAD")
	out, err := c.Git(ctx, env, cache, args...)
	if err != nil {
		return "", fmt.Errorf("read the default branch: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		ref, ok := strings.CutPrefix(line, "ref: refs/heads/")
		if !ok {
			continue
		}
		branch, _, _ := strings.Cut(ref, "\t")
		if !refName.MatchString(branch) || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") {
			return "", fmt.Errorf("unusable default branch name %q", github.Printable(branch, 100))
		}
		return branch, nil
	}
	return "", errors.New("GitHub named no default branch")
}

func (c *Checkouts) revParse(ctx context.Context, cache, ref string) (string, error) {
	out, err := c.Git(ctx, nil, cache, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	s := strings.TrimSpace(string(out))
	if err != nil || !sha(s) {
		return "", fmt.Errorf("read %s: %v", ref, err)
	}
	return s, nil
}

// checkout clones cache to dir and checks commit out, detached. The clone
// borrows the cache's objects but has its own config, refs and hooks, so git
// run in it (by the review session too) cannot change the cache orchd
// fetches with its credentials.
func (c *Checkouts) checkout(ctx context.Context, env []string, cache, dir, commit string) error {
	clone := []string{"clone", "--quiet", "--shared", "--no-checkout"}
	for i := 0; i < len(safeArgs); i += 2 {
		clone = append(clone, "--config", safeArgs[i+1])
	}
	clone = append(clone, "--", cache, dir)
	if _, err := c.Git(ctx, env, "", clone...); err != nil {
		return fmt.Errorf("clone for %s: %w", shortSHA(commit), err)
	}
	args := append(append([]string{}, safeArgs...), "checkout", "--quiet", "--detach", commit)
	if _, err := c.Git(ctx, env, dir, args...); err != nil {
		return fmt.Errorf("check out %s: %w", shortSHA(commit), err)
	}
	return nil
}

// Sweep removes every workspace not in keep. A workspace is kept while its
// review task may still run or resume; everything else under work/ is left
// over.
func (c *Checkouts) Sweep(_ context.Context, keep map[string]bool) {
	work := filepath.Join(c.Root, "work")
	entries, err := os.ReadDir(work)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(work, e.Name())
		if e.IsDir() && workID.MatchString(e.Name()) && !keep[dir] {
			_ = os.RemoveAll(dir)
		}
	}
}

// ExecGit runs the git binary; outside tests it is Checkouts.Git.
func ExecGit(ctx context.Context, env []string, dir string, args ...string) ([]byte, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	// Reason: a fetch must never stop to ask for a password nobody can type,
	// replace refs must not swap what a SHA checks out, and LFS must not
	// download from wherever the repo says.
	cmd.Env = append(envOrSelf(env), "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_LFS_SKIP_SMUDGE=1")
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, github.Printable(string(ee.Stderr), 500))
	}
	return out, err
}

func envOrSelf(env []string) []string {
	if env == nil {
		return os.Environ()
	}
	return env
}

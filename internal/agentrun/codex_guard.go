package agentrun

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitGuard protects what a worktree session may write but must not change.
// The sandbox has to let it write the shared refs and its worktree's git
// directory to commit, which would also let it move other branches, or repoint
// the worktree at a git directory (and so config and hooks) of its making for
// the next git command run outside the sandbox. The guard records that state
// before the run and puts it back after.
type gitGuard struct {
	r       *Runner
	dir     string
	gitDir  string
	files   map[string][]byte // worktree git-dir file → contents; nil when absent
	refs    map[string]string // ref → object, excluding the worktree's branch
	ownRef  string
	enabled bool
}

// guardedFiles are the worktree git-dir files that decide which repository,
// config and hooks git uses there.
var guardedFiles = []string{"commondir", "gitdir", "config.worktree"}

func (r *Runner) newGitGuard(ctx context.Context, dir string) *gitGuard {
	g := &gitGuard{r: r, dir: dir}
	roots := r.codexWritableRoots(ctx, dir)
	if len(roots) == 0 {
		return g // nothing outside dir is writable, so nothing to guard
	}
	g.gitDir, g.enabled = roots[0], true
	g.files = map[string][]byte{}
	for _, f := range guardedFiles {
		b, err := os.ReadFile(filepath.Join(g.gitDir, f))
		if err != nil {
			b = nil
		}
		g.files[f] = b
	}
	if out, err := r.git(ctx, dir, "symbolic-ref", "-q", "HEAD"); err == nil {
		g.ownRef = strings.TrimSpace(string(out))
	}
	g.refs = g.snapshotRefs(ctx)
	return g
}

func (g *gitGuard) snapshotRefs(ctx context.Context) map[string]string {
	out, err := g.r.git(ctx, g.dir, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, obj, ok := strings.Cut(line, " ")
		if ok && name != g.ownRef {
			refs[name] = obj
		}
	}
	return refs
}

// restore puts guarded state back and names what had changed. New branches
// are allowed (they move nothing that existed); a moved or deleted ref, or a
// new ref outside refs/heads/, is reverted.
func (g *gitGuard) restore(ctx context.Context) []string {
	if !g.enabled {
		return nil
	}
	// Git state is restored even if the run was canceled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var changed []string
	for _, f := range guardedFiles {
		path := filepath.Join(g.gitDir, f)
		now, err := os.ReadFile(path)
		if err != nil {
			now = nil
		}
		if bytes.Equal(now, g.files[f]) {
			continue
		}
		changed = append(changed, "worktree "+f)
		if g.files[f] == nil {
			_ = os.Remove(path)
		} else {
			_ = os.WriteFile(path, g.files[f], 0o644)
		}
	}
	if g.refs == nil {
		return changed
	}
	after := g.snapshotRefs(ctx)
	for name, obj := range g.refs {
		if after[name] != obj {
			changed = append(changed, name)
			_, _ = g.r.git(ctx, g.dir, "update-ref", "--no-deref", name, obj)
		}
	}
	for name := range after {
		if _, existed := g.refs[name]; !existed && !strings.HasPrefix(name, "refs/heads/") {
			changed = append(changed, name)
			_, _ = g.r.git(ctx, g.dir, "update-ref", "--no-deref", "-d", name)
		}
	}
	return changed
}

// git runs git in dir with no inherited GIT_* settings, stopping its
// repository search above dir's parent.
func (r *Runner) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	env := []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir), "GIT_CONFIG_NOSYSTEM=1"}
	for _, kv := range checkEnv(os.Environ(), r.searchPath()) {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	return cmd.Output()
}

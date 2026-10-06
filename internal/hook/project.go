package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// gitProbeTimeout bounds the git calls a hook makes to identify the project. Hooks
// sit on the prompt's critical path, so a slow filesystem must degrade to
// "no project" rather than delay the session.
const gitProbeTimeout = 500 * time.Millisecond

// project identifies the repository a session runs in, for scoping RAG.
type project struct {
	// name matches the repo:<name> tag the repo and owner source walkers attach to
	// ingested docs. It comes from the origin remote when there is one.
	name string
	// checkout is the absolute path of the main checkout. Every linked worktree
	// shares it, and unlike the remote it cannot be chosen by the repository's own
	// config, so it is the only identity trusted to label session content.
	checkout string
}

// searchTags returns the tags whose hits belong to this project: its registered
// docs and its own past sessions.
func (p project) searchTags() []string {
	var tags []string
	if p.name != "" {
		tags = append(tags, "repo:"+p.name)
	}
	if p.checkout != "" {
		tags = append(tags, "checkout:"+p.checkout)
	}
	return tags
}

// sessionTags returns the tags for content captured from a session here. The
// remote-derived repo tag is deliberately excluded: any repository can point
// origin at another project's name, and session content tagged that way would be
// promoted into that project's prompts.
func (p project) sessionTags() []string {
	if p.checkout == "" {
		return nil
	}
	return []string{"checkout:" + p.checkout}
}

// detectProject resolves cwd to its repository, or the zero project outside one
// or when git is unavailable.
func detectProject(cwd string) project {
	if cwd == "" {
		return project{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()
	common, err := gitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return project{}
	}
	checkout := filepath.Clean(common)
	if filepath.Base(checkout) == ".git" {
		checkout = filepath.Dir(checkout)
	}
	p := project{checkout: checkout}
	if url, err := gitOutput(ctx, cwd, "remote", "get-url", "origin"); err == nil {
		p.name = repoNameFromRemote(url)
	}
	if p.name == "" {
		p.name = strings.TrimSuffix(filepath.Base(checkout), ".git")
	}
	return p
}

// repoNameFromRemote extracts the repository name from a git remote URL in any of
// the scp-like, URL, or local-path forms git accepts.
func repoNameFromRemote(url string) string {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		url = url[i+1:]
	}
	return strings.TrimSuffix(url, ".git")
}

// gitLocatorEnv are the variables that override which repository git operates on.
// A hook can inherit them (for example when the harness runs inside a git hook),
// and they would make the probe describe some other repository than cwd's.
var gitLocatorEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// gitOutput runs git against dir with an argument vector (never a shell) and
// returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = withoutEnv(os.Environ(), gitLocatorEnv)
	// Bounds the wait for stdout to close after the context kills git, should a
	// descendant inherit the pipe.
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func withoutEnv(env, names []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); !slices.Contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

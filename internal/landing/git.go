package landing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/willesq/orchestrator/internal/github"
)

// Limits on what a proposal shows, so one huge merge stays readable.
const (
	maxCommits  = 50
	maxFiles    = 300
	maxDiffSize = 256 << 10
)

// GitRunner runs git in dir and returns stdout; injectable for tests.
type GitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// Commit is one commit of a proposal.
type Commit struct{ SHA, Subject, Author string }

// File is one file a proposal changes. A binary file counts no lines, so its
// change shows only as Binary.
type File struct {
	Path                 string
	Additions, Deletions int
	Binary               bool
}

// baseConfig applies to every git command. The fetch reads from a worktree an
// agent controls, and these keep anything it left in reach of this process
// from running.
var baseConfig = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.ext.allow=never"}

// Git runs the real git with no terminal prompts and no inherited repository
// environment.
func Git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	all := append([]string{}, baseConfig...)
	// Inside a landing repository, name it: git would otherwise prefer a .git
	// planted within it over the repository itself.
	if strings.HasSuffix(dir, ".git") {
		all = append(all, "--git-dir=.")
	}
	cmd := exec.CommandContext(ctx, "git", append(all, args...)...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// Replacement refs would show one commit's content and push another's.
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out, &gitError{code: exitErr.ExitCode(), text: github.Printable(stderr.String(), 512)}
		}
		return out, err
	}
	return out, nil
}

type gitError struct {
	code int
	text string
}

func (e *gitError) Error() string {
	if e.text == "" {
		return fmt.Sprintf("git exited with %d", e.code)
	}
	return "git: " + e.text
}

// exitedWith reports whether err is git exiting with code.
func exitedWith(err error, code int) bool {
	var g *gitError
	return errors.As(err, &g) && g.code == code
}

// repoConfig is the whole of a landing repository's config, written by orchd.
const repoConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"

// ensureRepo makes the task's private bare repository. It has no hooks and
// none of the worktree's config: the only thing taken from the worktree is
// the objects of the proposed commit. An agent runs as the same user and could
// write here too, so a repository that is not exactly what orchd made is thrown
// away rather than used, which leaves its config, hooks and refs unread.
func (s *Service) ensureRepo(ctx context.Context, repo string) error {
	if _, err := os.Stat(repo); err == nil {
		if repoTrusted(repo) {
			return nil
		}
		log.Printf("landing: %s is not as orchd left it; rebuilding it", filepath.Base(repo))
		if err := os.RemoveAll(repo); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if _, err := s.Git(ctx, s.Dir, "init", "--quiet", "--bare", "--template=", repo); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(repo, "config"), []byte(repoConfig), 0o600)
}

// repoTrusted reports whether repo has orchd's config and nothing that would
// change what its commands read or run. Git keeps these in fixed places, so
// their absence is checked rather than their content.
func repoTrusted(repo string) bool {
	config, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil || string(config) != repoConfig {
		return false
	}
	for _, path := range []string{".git", "hooks", "config.worktree", "shallow", "worktrees", "refs/replace",
		"info/grafts", "info/attributes", "objects/info/alternates", "objects/info/commit-graph",
		"objects/info/commit-graphs", "commondir", "gitdir"} {
		if _, err := os.Lstat(filepath.Join(repo, path)); err == nil {
			return false
		}
	}
	return true
}

// verifyForPush checks repo just before a push. Beyond repoTrusted, it rehashes
// every object: a replaced loose object would show one change and send another,
// since GitHub may already hold the real object elsewhere and accept the commit.
func (s *Service) verifyForPush(ctx context.Context, repo string) error {
	if !repoTrusted(repo) {
		return fmt.Errorf("%w: the copy of this commit has been changed; check it again", ErrRefused)
	}
	if _, err := s.Git(ctx, repo, "fsck", "--full", "--no-dangling", "--no-progress"); err != nil {
		return fmt.Errorf("%w: the copy of this commit does not check out: %s", ErrRefused, problemText(err))
	}
	return nil
}

// minGitVersion is the first git whose upload-pack refuses a lazy fetch by
// default. Older versions could run commands a hostile source repository names
// while orchd fetches from it.
var minGitVersion = [3]int{2, 45, 1}

// gitVersionSafe reads `git version` output.
func gitVersionSafe(output string) bool {
	fields := strings.Fields(output)
	if len(fields) < 3 {
		return false
	}
	parts := strings.Split(fields[2], ".")
	for i, floor := range minGitVersion {
		if i >= len(parts) {
			return false
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		if n != floor {
			return n > floor
		}
	}
	return true
}

// fetchProposal copies the proposed commit into repo once, and reports false
// when the worktree's HEAD is not that commit any more.
func (s *Service) fetchProposal(ctx context.Context, repo string, p Proposal) (bool, error) {
	if s.hasCommit(ctx, repo, p.SHA) {
		return true, nil
	}
	if err := checkPath(p.Path, s.Home); err != nil {
		return false, err
	}
	version, err := s.Git(ctx, repo, "version")
	if err != nil {
		return false, err
	}
	if !gitVersionSafe(string(version)) {
		return false, fmt.Errorf("%w: orch needs git %d.%d.%d or newer to read an agent's worktree safely", ErrRefused, minGitVersion[0], minGitVersion[1], minGitVersion[2])
	}
	if _, err := s.Git(ctx, repo, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-commit-graph", "--", p.Path, "HEAD"); err != nil {
		return false, err
	}
	out, err := s.Git(ctx, repo, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(out)) != p.SHA {
		return false, nil
	}
	// A ref keeps the commit through any later gc.
	_, err = s.Git(ctx, repo, "update-ref", "refs/orch/proposal", p.SHA)
	return true, err
}

func (s *Service) hasCommit(ctx context.Context, repo, sha string) bool {
	_, err := s.Git(ctx, repo, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// builtOn reports whether sha descends from base. A base repo does not have
// (the PR moved on after the agent started) does not.
func (s *Service) builtOn(ctx context.Context, repo, base, sha string) (bool, error) {
	if !s.hasCommit(ctx, repo, base) {
		return false, nil
	}
	_, err := s.Git(ctx, repo, "merge-base", "--is-ancestor", base, sha)
	if err == nil {
		return true, nil
	}
	if exitedWith(err, 1) {
		return false, nil
	}
	return false, err
}

// fillChange adds what going from base to sha changes.
func (s *Service) fillChange(ctx context.Context, repo, base, sha string, l *Landing) error {
	span := base + ".." + sha
	out, err := s.Git(ctx, repo, "rev-list", "--count", span)
	if err != nil {
		return err
	}
	l.TotalCommits, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	if out, err = s.Git(ctx, repo, "log", "--no-show-signature", "--format=%H%x00%s%x00%an", "--max-count="+strconv.Itoa(maxCommits), span); err != nil {
		return err
	}
	l.Commits = parseCommits(string(out))
	if out, err = s.Git(ctx, repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--numstat", "-z", base, sha); err != nil {
		return err
	}
	files := parseNumstat(string(out))
	l.TotalFiles, l.Additions, l.Deletions, l.BinaryFiles = len(files), 0, 0, 0
	for _, f := range files {
		l.Additions += f.Additions
		l.Deletions += f.Deletions
		if f.Binary {
			l.BinaryFiles++
		}
	}
	l.Files = files[:min(len(files), maxFiles)]
	if out, err = s.Git(ctx, repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", base, sha); err != nil {
		return err
	}
	l.Diff, l.DiffTruncated = capDiff(out)
	return nil
}

func parseCommits(out string) []Commit {
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) == 3 {
			commits = append(commits, Commit{SHA: parts[0], Subject: strings.ToValidUTF8(parts[1], "?"), Author: strings.ToValidUTF8(parts[2], "?")})
		}
	}
	return commits
}

// parseNumstat reads `git diff --numstat -z --no-renames`: one
// "added\tdeleted\tpath" record per file, NUL-terminated, "-" counts for binary.
func parseNumstat(out string) []File {
	var files []File
	for _, record := range strings.Split(out, "\x00") {
		parts := strings.SplitN(record, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		files = append(files, File{Path: strings.ToValidUTF8(parts[2], "?"), Additions: added, Deletions: deleted, Binary: parts[0] == "-"})
	}
	return files
}

// capDiff keeps whole lines up to the limit, as valid UTF-8 since an RPC string
// must be and a diff of a binary-ish file need not be.
func capDiff(out []byte) (string, bool) {
	truncated := len(out) > maxDiffSize
	if truncated {
		out = out[:maxDiffSize]
		if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
			out = out[:i+1]
		}
	}
	return strings.ToValidUTF8(string(out), "?"), truncated
}

// push sends sha to the PR's branch: plain refspec, so GitHub refuses anything
// but a fast-forward. It names the repository and the credential helper
// itself rather than using anything from the agent's clone.
func (s *Service) push(ctx context.Context, repo, ownerRepo, sha, branch string) error {
	_, err := s.Git(ctx, repo, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
		"push", "--quiet", "--no-verify", "https://github.com/"+ownerRepo+".git", sha+":refs/heads/"+branch)
	return err
}

// restoreCommit makes a commit on top of sha with exactly the tree of prior,
// signed when the user signs commits. It works whatever sha is (a merge, say)
// and needs no working tree, and pushing it rewrites nothing.
func (s *Service) restoreCommit(ctx context.Context, repo, prior, sha string) (string, error) {
	out, err := s.Git(ctx, repo, "rev-parse", "--verify", prior+"^{tree}")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(string(out)), "-p", sha,
		"-m", fmt.Sprintf("revert: undo the agent's landing of %s\n\nRestores the tree of %s.", short(sha), prior)}
	if signs, err := s.Git(ctx, repo, "config", "--type=bool", "--get", "commit.gpgsign"); err == nil && strings.TrimSpace(string(signs)) == "true" {
		args = append(args, "-S")
	}
	if out, err = s.Git(ctx, repo, args...); err != nil {
		return "", err
	}
	revert := strings.TrimSpace(string(out))
	if !isSHA(revert) {
		return "", fmt.Errorf("git commit-tree gave no commit")
	}
	return revert, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

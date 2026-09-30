package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
)

// Repo is a GitHub repository surfaced by enumeration.
type Repo struct {
	Name       string
	HeadSHA    string // default-branch HEAD commit oid ("" for an empty repo)
	IsArchived bool
	IsFork     bool
}

// GitHub enumerates an owner's repositories (one API call per page, HEAD SHAs
// included so a sync can skip unchanged repos without any extra call).
type GitHub interface {
	ListRepos(ctx context.Context, owner string) ([]Repo, error)
}

// Git materializes a repo's working tree at a destination (authenticated clone).
type Git interface {
	Sync(ctx context.Context, owner, repo, dest string) error
}

// Ingester stores text into RAG (implemented by *rag.Service).
type Ingester interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
}

// SyncOpts carries the dependencies + policy for a sync.
type SyncOpts struct {
	RepoCache       string
	IncludeArchived bool
	IncludeForks    bool
	GH              GitHub
	Git             Git
	Force           bool         // ignore repo-SHA + file-hash caches
	Now             func() int64 // injectable clock
}

// SyncOwner enumerates the owner's repos (one gh call/page), clones only the
// repos whose HEAD moved, and ingests their markdown (skipping files whose
// content hash is unchanged). Returns the owner's status ("ok" | "error").
func SyncOwner(ctx context.Context, reg *Registry, o Owner, ing Ingester, opts SyncOpts) (string, error) {
	now := opts.Now
	if now == nil {
		now = func() int64 { return 0 }
	}
	repos, err := opts.GH.ListRepos(ctx, o.Owner)
	if err != nil {
		return "error", err // enumerate failed → flag, retain existing chunks
	}
	for _, rp := range repos {
		if rp.IsArchived && !opts.IncludeArchived {
			continue
		}
		if rp.IsFork && !opts.IncludeForks {
			continue
		}
		prev, _ := reg.RepoSHA(ctx, o.Owner, rp.Name)
		if !opts.Force && rp.HeadSHA != "" && prev == rp.HeadSHA {
			continue // repo unchanged → skip entirely (no clone, no re-ingest)
		}
		dest := filepath.Join(opts.RepoCache, o.Owner, rp.Name)
		if err := opts.Git.Sync(ctx, o.Owner, rp.Name, dest); err != nil {
			_ = reg.SetRepoState(ctx, RepoState{Owner: o.Owner, Repo: rp.Name, HeadSHA: prev, LastSynced: now(), Status: "error"})
			continue
		}
		files := ingestRepoMarkdown(ctx, reg, dest, o, rp.Name, ing, opts)
		_ = reg.SetRepoState(ctx, RepoState{
			Owner: o.Owner, Repo: rp.Name, HeadSHA: rp.HeadSHA,
			LastSynced: now(), Files: files, Status: "ok",
		})
	}
	return "ok", nil
}

// ingestRepoMarkdown walks a cloned repo for *.md and ingests each file, skipping
// files whose content hash is unchanged (unless opts.Force). Returns file count.
func ingestRepoMarkdown(ctx context.Context, reg *Registry, repoPath string, o Owner, repo string, ing Ingester, opts SyncOpts) int {
	now := opts.Now
	if now == nil {
		now = func() int64 { return 0 }
	}
	n := 0
	_ = filepath.WalkDir(repoPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		if strings.Contains(p, "/.git/") || strings.Contains(p, "/node_modules/") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoPath, p)
		uri := fmt.Sprintf("repo://%s/%s/%s", o.Owner, repo, rel)
		h := sha256Hex(raw)
		if !opts.Force {
			if prev, _ := reg.Hash(ctx, uri); prev == h {
				return nil // content unchanged → skip embed + upsert
			}
		}
		content, redactions := redact.Redact(string(raw))
		if redactions > redact.Threshold {
			return nil // keystore-like: skip entirely
		}
		tags := append([]string{}, o.Tags...)
		tags = append(tags, "owner:"+o.Owner, "repo:"+repo)
		if _, err := ing.IngestText(ctx, content, rag.Meta{
			SourceURI:  uri,
			SourceType: "repo_doc",
			Scope:      o.Scope,
			Tags:       tags,
		}); err == nil {
			_ = reg.SetHash(ctx, uri, h, now())
			n++
		}
		return nil
	})
	return n
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- default gh/git implementations (behind the seams above) -----------------

// runFunc runs an external command and returns its stdout.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// execRun runs a command and folds its stderr into the returned error, so a
// failure reported as an owner's "error" status carries the reason (bad auth,
// missing binary, clone rejection) instead of just an exit code.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		// Reason: git/gh error text can echo a credential-bearing remote URL.
		msg, _ := redact.Redact(strings.TrimSpace(string(ee.Stderr)))
		return out, fmt.Errorf("%w: %s", err, msg)
	}
	return out, err
}

// NewGitHub returns the default gh-CLI-backed enumerator.
func NewGitHub() GitHub { return ghGitHub{run: execRun} }

// NewGit returns the default gh-CLI-backed cloner.
func NewGit() Git { return ghGit{run: execRun} }

const repoQuery = `query($owner:String!,$cursor:String){repositoryOwner(login:$owner){repositories(first:100,after:$cursor,ownerAffiliations:OWNER){pageInfo{hasNextPage endCursor}nodes{name isArchived isFork defaultBranchRef{target{oid}}}}}}`

type ghGitHub struct{ run runFunc }

// ListRepos pulls every repo (name, HEAD oid, archived, fork) via one GraphQL
// call per 100 repos — the maximum page size, to minimize API calls.
func (g ghGitHub) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	var all []Repo
	cursor := ""
	for {
		args := []string{"api", "graphql", "-f", "query=" + repoQuery, "-F", "owner=" + owner}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, err := g.run(ctx, "gh", args...)
		if err != nil {
			return nil, fmt.Errorf("gh api graphql (repo list for %s): %w", owner, err)
		}
		repos, hasNext, end, err := parseGraphQLPage(out)
		if err != nil {
			return nil, err
		}
		all = append(all, repos...)
		if !hasNext || end == "" {
			break
		}
		cursor = end
	}
	return all, nil
}

// parseGraphQLPage decodes one page of the repositories GraphQL response.
func parseGraphQLPage(b []byte) (repos []Repo, hasNext bool, endCursor string, err error) {
	var resp struct {
		Data struct {
			RepositoryOwner struct {
				Repositories struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						Name             string `json:"name"`
						IsArchived       bool   `json:"isArchived"`
						IsFork           bool   `json:"isFork"`
						DefaultBranchRef *struct {
							Target *struct {
								Oid string `json:"oid"`
							} `json:"target"`
						} `json:"defaultBranchRef"`
					} `json:"nodes"`
				} `json:"repositories"`
			} `json:"repositoryOwner"`
		} `json:"data"`
	}
	if err = json.Unmarshal(b, &resp); err != nil {
		return nil, false, "", fmt.Errorf("parse gh graphql response: %w", err)
	}
	page := resp.Data.RepositoryOwner.Repositories
	for _, n := range page.Nodes {
		r := Repo{Name: n.Name, IsArchived: n.IsArchived, IsFork: n.IsFork}
		if n.DefaultBranchRef != nil && n.DefaultBranchRef.Target != nil {
			r.HeadSHA = n.DefaultBranchRef.Target.Oid
		}
		repos = append(repos, r)
	}
	return repos, page.PageInfo.HasNextPage, page.PageInfo.EndCursor, nil
}

type ghGit struct{ run runFunc }

// Sync (re)clones the repo shallowly to dest. Since callers only sync repos whose
// HEAD moved, a fresh shallow clone is simplest and always correct.
func (g ghGit) Sync(ctx context.Context, owner, repo, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// `gh repo clone` uses the user's gh auth so private repos work.
	if _, err := g.run(ctx, "gh", "repo", "clone", owner+"/"+repo, dest, "--", "--depth", "1"); err != nil {
		return fmt.Errorf("gh repo clone %s/%s: %w", owner, repo, err)
	}
	return nil
}

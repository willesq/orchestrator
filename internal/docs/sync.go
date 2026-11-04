package docs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/willesq/orchestrator/internal/gitdates"
	"github.com/willesq/orchestrator/internal/mdwalk"
	"github.com/willesq/orchestrator/internal/prunecap"
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

// Remover deletes a source's chunks (implemented by *rag.Service). An
// Ingester that is also a Remover has the chunks of docs removed from a repo
// deleted on the next sync.
type Remover interface {
	DeleteSource(ctx context.Context, uri string) (int, error)
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
	// ExcludeDirs names directories never walked for markdown (ingest.exclude_dirs).
	ExcludeDirs []string
	// Progress, when set, hears after each repo is handled: how many of the
	// owner's repos are done, of how many, and which one was last.
	Progress func(done, total int, repo string)
	// Dates reads when each file in a clone last changed; nil is gitdates.Read.
	Dates func(ctx context.Context, dir string) gitdates.Dates
	// Guard caps deletions across syncs; nil caps each sync on its own.
	Guard PruneGuard
}

// Result is how one owner's sync went.
type Result struct {
	// Status is "ok" when every repo synced, "partial" when some failed, and
	// "error" when the repos could not be listed or the sync stopped early.
	Status string
	// Reason says why, for anything but "ok".
	Reason string
	// Repos counts the repos considered; Synced those cloned and ingested;
	// Unchanged those skipped because their HEAD had not moved; Failed those
	// whose clone failed; NotTried those left untouched after an early stop.
	Repos, Synced, Unchanged, Failed, NotTried int
}

// maxNetworkFailures in a row stop an owner's sync: when GitHub cannot be
// reached, every further clone would only wait out its own connection timeout.
const maxNetworkFailures = 3

// cloneTimeout bounds one clone, so a connection that hangs cannot stall the
// sync. A variable so tests need not wait it out.
var cloneTimeout = 3 * time.Minute

// SyncOwner enumerates the owner's repos (one gh call/page), clones only the
// repos whose HEAD moved, and ingests their markdown (skipping files whose
// content hash is unchanged).
//
// A repo whose clone fails is recorded with the reason and logged, and the
// sync goes on with the next. After maxNetworkFailures network failures in a
// row it stops: the remaining repos keep their previous state and the owner's
// status is "error", with the reason.
func SyncOwner(ctx context.Context, reg *Registry, o Owner, ing Ingester, opts SyncOpts) Result {
	now := opts.Now
	if now == nil {
		now = func() int64 { return 0 }
	}
	repos, err := opts.GH.ListRepos(ctx, o.Owner)
	if err != nil {
		// Enumeration failed: flag it, and keep every repo's existing chunks.
		reason := classify(err).describe("listing repos", err)
		log.Printf("docs: %s: %s", o.Owner, reason)
		return Result{Status: "error", Reason: reason}
	}
	var todo []Repo
	for _, rp := range repos {
		if (rp.IsArchived && !opts.IncludeArchived) || (rp.IsFork && !opts.IncludeForks) || !o.Covers(rp.Name) {
			continue
		}
		todo = append(todo, rp)
	}
	res := Result{Repos: len(todo)}
	networkRun := 0
	// Refusals this sync still stands by: those it made, and those of repos
	// skipped as unchanged (the same tree would be refused again).
	var current []string
	for i, rp := range todo {
		if ctx.Err() != nil {
			res.NotTried = len(todo) - i
			res.Status, res.Reason = "error", "stopped: "+ctx.Err().Error()
			return res
		}
		prev, _ := reg.RepoSHA(ctx, o.Owner, rp.Name)
		if !opts.Force && rp.HeadSHA != "" && prev == rp.HeadSHA && !overwritten(ctx, reg, ing, o.Owner, rp.Name) {
			res.Unchanged++ // repo unchanged → skip entirely (no clone, no re-ingest)
			current = append(current, "repo://"+o.Owner+"/"+rp.Name+"/")
			report(opts, i+1, len(todo), rp.Name)
			continue
		}
		dest := filepath.Join(opts.RepoCache, o.Owner, rp.Name)
		cloneCtx, cancel := context.WithTimeout(ctx, cloneTimeout)
		err := opts.Git.Sync(cloneCtx, o.Owner, rp.Name, dest)
		if err != nil && cloneCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = fmt.Errorf("clone took longer than %s: %w", cloneTimeout, err)
		}
		cancel()
		if err != nil {
			f := classify(err)
			reason := f.describe("clone", err)
			log.Printf("docs: %s/%s: %s", o.Owner, rp.Name, reason)
			_ = reg.SetRepoState(ctx, RepoState{Owner: o.Owner, Repo: rp.Name, HeadSHA: prev, LastSynced: now(), Status: "error", Error: reason})
			res.Failed++
			report(opts, i+1, len(todo), rp.Name)
			if f.kind == kindNetwork || f.kind == kindStalled {
				networkRun++
				if networkRun >= maxNetworkFailures {
					res.NotTried = len(todo) - i - 1
					res.Status = "error"
					res.Reason = fmt.Sprintf("stopped after %d repos in a row could not be cloned (%s); %d not tried: %s",
						networkRun, f.kind, res.NotTried, f.hint)
					log.Printf("docs: %s: %s", o.Owner, res.Reason)
					return res
				}
			} else {
				networkRun = 0
			}
			continue
		}
		networkRun = 0
		files, refused := ingestRepoMarkdown(ctx, reg, dest, o, rp.Name, ing, opts)
		if refused != "" {
			current = append(current, "repo://"+o.Owner+"/"+rp.Name+"/")
			// The repo records its new HEAD, so it is not cloned again until
			// HEAD moves or the operator confirms the prune (which clears it),
			// and shows as failing meanwhile.
			_ = reg.SetRepoState(ctx, RepoState{
				Owner: o.Owner, Repo: rp.Name, HeadSHA: rp.HeadSHA, LastSynced: now(), Files: files, Status: "error",
				Error: "prune refused: " + refused + "; the operator can confirm it with orch docs sync " + o.Owner + " --confirm-prune",
			})
			res.Failed++
			report(opts, i+1, len(todo), rp.Name)
			continue
		}
		_ = reg.SetRepoState(ctx, RepoState{
			Owner: o.Owner, Repo: rp.Name, HeadSHA: rp.HeadSHA,
			LastSynced: now(), Files: files, Status: "ok",
		})
		res.Synced++
		report(opts, i+1, len(todo), rp.Name)
	}
	if opts.Guard != nil {
		if err := opts.Guard.Forget(ctx, prunecap.DocsKey(o.Owner), current); err != nil {
			log.Printf("docs: %s: prune guard: %v", o.Owner, err)
		}
	}
	res.Status = "ok"
	if res.Failed > 0 {
		res.Status = "partial"
		res.Reason = fmt.Sprintf("%d of %d repos failed; see each repo's error", res.Failed, res.Repos)
	}
	return res
}

func report(opts SyncOpts, done, total int, repo string) {
	if opts.Progress != nil {
		opts.Progress(done, total, repo)
	}
}

// Failure kinds a sync tells apart, because each has a different fix.
const (
	kindStalled = "stalled"
	kindNetwork = "network"
	kindAuth    = "authentication"
	kindAccess  = "access"
	kindOther   = "other"
)

type failure struct{ kind, hint string }

// describe is the reason recorded for a failed step: what failed, the kind,
// what to do, and the command's own message, made safe by cleanReason.
func (f failure) describe(step string, err error) string {
	return fmt.Sprintf("%s failed (%s): %s — %s", step, f.kind, f.hint, cleanReason(err.Error(), 300))
}

// urlUserinfo matches the credentials part of a URL (scheme://user:secret@).
var urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`)

// cleanReason makes command output safe to record, log and show: credentials
// in URLs and known token shapes redacted, control and format characters
// removed, valid UTF-8, one line, at most max characters. gh and git output can
// echo a credential-bearing remote, and git relays the server's own text.
func cleanReason(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = urlUserinfo.ReplaceAllString(s, "${1}[REDACTED]@")
	s, _ = redact.Redact(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// classify sorts a gh or git failure by what would fix it. Network problems
// are checked first: git reports "Could not read from remote repository" for
// an unreachable host as well as for a refused key.
func classify(err error) failure {
	msg := strings.ToLower(err.Error())
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(msg, p) {
				return true
			}
		}
		return false
	}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return failure{kindOther, "gh or git is not installed, or not on orchd's PATH"}
	case errors.Is(err, errStalled):
		return failure{kindStalled, "the clone stopped making progress; your SSH agent may be waiting for approval (for example a 1Password prompt), or GitHub stopped responding"}
	case errors.Is(err, context.DeadlineExceeded) || has("operation timed out", "connection timed out", "timed out",
		"could not resolve host", "network is unreachable", "no route to host", "connection refused",
		"connection reset", "error connecting to", "took longer than", "temporary failure in name resolution"):
		return failure{kindNetwork, "GitHub could not be reached from this machine; check the network connection"}
	case has("permission denied (publickey", "host key verification failed", "agent refused", "sign_and_send_pubkey",
		"authentication failed", "could not read username", "gh auth login"):
		return failure{kindAuth, "GitHub did not accept the credentials; check that your SSH agent (for example 1Password) is unlocked and allows this key, or run gh auth status"}
	case has("repository not found", "could not read from remote repository", "http 404", "http 403"):
		return failure{kindAccess, "GitHub refused the repository; check that your account can still read it"}
	default:
		return failure{kindOther, "see the error"}
	}
}

// ingestRepoMarkdown walks a cloned repo for *.md and ingests each file, skipping
// files whose content hash is unchanged (unless opts.Force). Returns file count.
func ingestRepoMarkdown(ctx context.Context, reg *Registry, repoPath string, o Owner, repo string, ing Ingester, opts SyncOpts) (int, string) {
	now := opts.Now
	if now == nil {
		now = func() int64 { return 0 }
	}
	readDates := opts.Dates
	if readDates == nil {
		readDates = gitdates.Read
	}
	var dates *gitdates.Dates
	prefix := fmt.Sprintf("repo://%s/%s/", o.Owner, repo)
	seen := map[string]bool{}
	n := 0
	complete := mdwalk.FilesComplete(repoPath, opts.ExcludeDirs, func(p string) {
		rel, _ := filepath.Rel(repoPath, p)
		uri := prefix + rel
		seen[uri] = true
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		h := sha256Hex(raw)
		content, redactions := redact.Redact(string(raw))
		if redactions > redact.Threshold {
			return // keystore-like: skip entirely
		}
		if !opts.Force {
			if prev, _ := reg.Hash(ctx, uri); prev == h && storedAsSynced(ctx, ing, uri, content) {
				return // content unchanged and still ours → skip embed + upsert
			}
		}
		tags := append([]string{}, o.Tags...)
		tags = append(tags, "owner:"+o.Owner, "repo:"+repo)
		if dates == nil { // read once, and only when a doc changed
			d := readDates(ctx, repoPath)
			dates = &d
		}
		changed := dates.For(rel)
		if _, err := ing.IngestText(ctx, content, rag.Meta{
			SourceURI:  uri,
			SourceType: "repo_doc",
			Scope:      o.Scope,
			Tags:       tags,
			StartedAt:  changed,
			EndedAt:    changed,
			Writer:     rag.WriterDocs,
		}); err == nil {
			_ = reg.SetHash(ctx, uri, h, now())
			n++
		}
	})
	refused := ""
	if complete {
		refused = pruneRemoved(ctx, reg, ing, opts, o, prefix, seen)
	}
	return n, refused
}

// Copier reads what is stored for a source (implemented by *rag.Service).
// With one, an unchanged file is skipped only while its stored copy is still
// this sync's: a client that overwrote the doc's URI is overwritten back.
type Copier interface {
	Copy(ctx context.Context, uri string) (rag.StoredCopy, bool, error)
}

// storedAsSynced reports whether uri still holds this sync's copy of text.
// Without a Copier it trusts the registry.
func storedAsSynced(ctx context.Context, ing Ingester, uri, text string) bool {
	c, ok := ing.(Copier)
	if !ok {
		return true
	}
	cp, found, err := c.Copy(ctx, uri)
	if err != nil {
		return true // a store error is not a reason to rewrite every doc
	}
	return found && cp.Writer == rag.WriterDocs && cp.ContentHash == rag.ContentHash(text)
}

// overwritten reports whether another writer (a client) has replaced any of
// a repo's synced docs, so an unchanged repo is synced again to restore them.
// Docs stored before writers were recorded do not count; a doc deleted on
// purpose is not restored.
func overwritten(ctx context.Context, reg *Registry, ing Ingester, owner, repo string) bool {
	c, ok := ing.(Copier)
	if !ok {
		return false
	}
	uris, err := reg.HashURIs(ctx, fmt.Sprintf("repo://%s/%s/", owner, repo))
	if err != nil {
		return false
	}
	for _, uri := range uris {
		if cp, found, err := c.Copy(ctx, uri); err == nil && found && cp.Writer != "" && cp.Writer != rag.WriterDocs {
			log.Printf("docs: %s was overwritten by a %s write; syncing %s/%s again to restore it", uri, cp.Writer, owner, repo)
			return true
		}
	}
	return false
}

// PruneGuard bounds deletions (implemented by *prunecap.Guard).
type PruneGuard interface {
	Decide(ctx context.Context, key, prefix string, stored, gone int) (bool, string, error)
	Settled(ctx context.Context, prefix string, count int) error
	// Forget clears key's refusals outside current, the prefixes the
	// latest complete walk refused again.
	Forget(ctx context.Context, key string, current []string) error
}

// minPruneCap is how many docs a sync may always remove at once.
const minPruneCap = 5

// pruneRemoved deletes the chunks of docs this repo's earlier syncs stored
// that its clone no longer has, and forgets their hashes. A walk that found
// nothing deletes nothing: an empty or unreadable clone is not a repo whose
// docs were all removed.
func pruneRemoved(ctx context.Context, reg *Registry, ing Ingester, opts SyncOpts, o Owner, prefix string, seen map[string]bool) string {
	rm, ok := ing.(Remover)
	if !ok || len(seen) == 0 || ctx.Err() != nil {
		return ""
	}
	known, err := reg.HashURIs(ctx, prefix)
	if err != nil {
		log.Printf("docs: listing %s docs: %v", prefix, err)
		return ""
	}
	var gone []string
	for _, uri := range known {
		if !seen[uri] {
			gone = append(gone, uri)
		}
	}
	if len(gone) == 0 {
		if opts.Guard != nil {
			_ = opts.Guard.Settled(ctx, prefix, len(known))
		}
		return ""
	}
	// A clone that suddenly lacks many of its docs is more likely broken
	// than a repo that lost them.
	if opts.Guard != nil {
		allowed, why, err := opts.Guard.Decide(ctx, prunecap.DocsKey(o.Owner), prefix, len(known), len(gone))
		if err != nil {
			log.Printf("docs: %s: prune guard: %v; not pruning", prefix, err)
			return ""
		}
		if !allowed {
			log.Printf("docs: %s: %s", prefix, why)
			return why
		}
	} else if len(gone) > max(minPruneCap, len(known)/2) {
		return fmt.Sprintf("%d of %d docs are gone at once; none were removed", len(gone), len(known))
	}
	// The guard recorded the prune as made; only a clean walk settles it.
	for _, uri := range gone {
		if _, err := rm.DeleteSource(ctx, uri); err != nil {
			log.Printf("docs: removing %s, gone from its repo: %v", uri, err)
			continue
		}
		_ = reg.DeleteHash(ctx, uri)
		log.Printf("docs: removed %s, gone from its repo", uri)
	}
	return ""
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
		return out, fmt.Errorf("%w: %s", err, cleanReason(string(ee.Stderr), 1000))
	}
	return out, err
}

// NewGitHub returns the default gh-CLI-backed enumerator.
func NewGitHub() GitHub { return ghGitHub{run: execRun} }

// NewGit returns the default gh-CLI-backed cloner. Its clones run under a
// stall watchdog, so one stuck on an unanswered SSH agent prompt or a silent
// connection is stopped and reported instead of holding up the sync.
func NewGit() Git {
	return ghGit{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runWatched(ctx, stallTimeout, name, args...)
	}}
}

// stallTimeout is how long a clone may go without writing anything before it
// counts as stalled. git reports progress continuously while it transfers
// (the clone passes --progress), so silence means it is waiting on something:
// usually SSH authentication, or a connection that stopped responding.
var stallTimeout = 45 * time.Second

// waitDelay bounds how long a stopped clone may keep its output pipe open
// through a process that escaped its group. A variable so tests need not wait.
var waitDelay = 5 * time.Second

// errStalled marks a command stopped by the stall watchdog.
var errStalled = errors.New("stalled")

// runWatched runs a command like execRun, but stops it once it has written
// nothing to stderr for stall. The command runs in its own process group, so
// stopping it also stops what it started (gh starts git, git starts ssh) and
// leaves nothing behind; WaitDelay bounds anything that escaped the group and
// still holds the output pipe.
func runWatched(ctx context.Context, stall time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	tail := &stderrTail{activity: make(chan struct{}, 1)}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timer := time.NewTimer(stall)
	defer timer.Stop()
	var err error
	stalled := false
wait:
	for {
		select {
		case <-tail.activity:
			timer.Reset(stall)
		case <-timer.C:
			stalled = true
			cancel()
			err = <-waited
			break wait
		case err = <-waited:
			break wait
		}
	}
	if stalled {
		return nil, fmt.Errorf("%w: no progress for %s", errStalled, stall)
	}
	if err != nil {
		if msg := lastLines(tail.bytes()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, cleanReason(msg, 1000))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// stderrTail keeps the end of a command's stderr and signals each write.
type stderrTail struct {
	mu       sync.Mutex
	buf      []byte
	activity chan struct{}
}

func (w *stderrTail) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > 8192 {
		w.buf = w.buf[len(w.buf)-8192:]
	}
	w.mu.Unlock()
	select {
	case w.activity <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *stderrTail) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...)
}

// lastLines is the end of a command's stderr without progress chatter: git
// rewrites its progress lines with carriage returns.
func lastLines(b []byte) string {
	var keep []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "%") {
			continue
		}
		keep = append(keep, line)
	}
	if len(keep) > 6 {
		keep = keep[len(keep)-6:]
	}
	return strings.Join(keep, " ")
}

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
	// --progress keeps git reporting while it transfers, which the stall
	// watchdog relies on.
	if _, err := g.run(ctx, "gh", "repo", "clone", owner+"/"+repo, dest, "--", "--depth", "1", "--progress"); err != nil {
		return fmt.Errorf("gh repo clone %s/%s: %w", owner, repo, err)
	}
	return nil
}

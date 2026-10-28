package sources

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/gitdates"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/mdwalk"
	"github.com/willesq/orchestrator/internal/prunecap"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
)

// Ingester stores text into RAG (implemented by *rag.Service).
type Ingester interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
}

// Pruner lists the sources a writer stored and deletes sources
// (implemented by *rag.Service). An Ingester that is also a Pruner has the
// chunks of markdown files removed from a walked repo deleted on its next
// refresh: only chunks this refresh stored, never another writer's.
type Pruner interface {
	SourcesWrittenBy(ctx context.Context, prefix, writer string) ([]string, error)
	DeleteSource(ctx context.Context, uri string) (int, error)
}

// RefreshOpts carries dependencies for refreshing sources.
type RefreshOpts struct {
	KnownRoots []string // dirs that contain owner/repo trees, e.g. ~/code
	HTTP       *http.Client
	// ExcludeDirs names directories never walked for markdown (ingest.exclude_dirs).
	ExcludeDirs []string
	// Dates reads when each file in a repo last changed; nil is gitdates.Read.
	Dates func(ctx context.Context, dir string) gitdates.Dates
	// Guard caps deletions across scans; nil caps each refresh on its own.
	Guard PruneGuard
}

// CheckoutURI is the URI a repo or owner source stores a local file under:
// its absolute path. A local checkout is not the GitHub repo it was cloned
// from (it may be behind, ahead or on another branch), so it never shares the
// docs sync's repo://<owner>/<repo> names, and two checkouts of the same
// name never share one.
func CheckoutURI(path string) string {
	return "checkout://" + filepath.ToSlash(filepath.Clean(path))
}

// Refresh brings one source up to date and returns its new status: "ok",
// "missing" or "error"; "skipped: <why>" for a checkout left as last indexed
// (not on its default branch); "attention: <why>" when a prune was refused
// and waits for an operator. On absence it flags without deleting chunks.
func Refresh(ctx context.Context, s Source, ing Ingester, opts RefreshOpts) (string, error) {
	switch s.Kind {
	case KindWeb:
		return refreshWeb(ctx, s, ing, opts)
	case KindRepo:
		return refreshRepo(ctx, s, s.URI, s.Owner, ing, opts)
	case KindOwner:
		return refreshOwner(ctx, s, ing, opts)
	default:
		return "error", fmt.Errorf("unknown source kind %q", s.Kind)
	}
}

func refreshWeb(ctx context.Context, s Source, ing Ingester, opts RefreshOpts) (string, error) {
	client := opts.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URI, nil)
	if err != nil {
		return "error", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "missing", nil // unreachable → flag, retain existing chunks
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "missing", nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "error", err
	}
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = stripHTML(text)
	}
	text, _ = redact.Redact(text)
	if _, err := ing.IngestText(ctx, text, rag.Meta{
		SourceURI: s.URI, SourceType: KindWeb, Scope: s.Scope, Tags: s.Tags, Writer: rag.WriterSources,
	}); err != nil {
		return "error", err
	}
	return "ok", nil
}

// repoResult is how one repo directory's refresh went.
type repoResult struct {
	stored  int    // docs stored
	found   bool   // it had markdown to index
	skipped string // why it was left as last indexed, if it was
	refused string // why a prune was refused, if it was
}

// refreshOwner ingests markdown from every repo directory under <root>/<owner>.
// Worktree containers (<repo>.worktrees) and linked worktrees are not repos
// of their own: their branches are not the repo's docs.
func refreshOwner(ctx context.Context, s Source, ing Ingester, opts RefreshOpts) (string, error) {
	base := ""
	for _, root := range opts.KnownRoots {
		cand := filepath.Join(root, s.URI)
		if isDir(cand) {
			base = cand
			break
		}
	}
	if base == "" {
		return "missing", nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "missing", nil
	}
	found := false
	var skipped, refused []string
	// One listing of what this refresh stored under the owner serves every
	// repo in it, rather than one scan of the store per repo.
	listing := &storedListing{prefix: CheckoutURI(resolve(base)) + "/"}
	for _, e := range entries {
		if !e.IsDir() || mdwalk.IsWorktreesDir(e.Name()) {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if co, ok := handoff.CheckoutAt(dir); ok && co.Linked {
			continue
		}
		r := ingestRepoMarkdown(ctx, dir, s.URI, s, ing, opts, listing)
		found = found || r.found
		if r.skipped != "" {
			skipped = append(skipped, e.Name()+" "+r.skipped)
		}
		if r.refused != "" {
			refused = append(refused, e.Name()+": "+r.refused)
		}
	}
	switch {
	case len(refused) > 0:
		return attention(s, refused), nil
	case !found && len(skipped) == 0:
		return "missing", nil
	case len(skipped) > 0:
		return fmt.Sprintf("ok; skipped %d: %s", len(skipped), strings.Join(skipped, "; ")), nil
	}
	return "ok", nil
}

func refreshRepo(ctx context.Context, s Source, repoPath, owner string, ing Ingester, opts RefreshOpts) (string, error) {
	if !isDir(repoPath) {
		return "missing", nil
	}
	r := ingestRepoMarkdown(ctx, repoPath, owner, s, ing, opts, nil)
	switch {
	case r.refused != "":
		return attention(s, []string{r.refused}), nil
	case r.skipped != "":
		return "skipped: " + r.skipped, nil
	case !r.found:
		return "missing", nil
	}
	return "ok", nil
}

// attention is the status of a source whose prune was refused.
func attention(s Source, reasons []string) string {
	return "attention: " + strings.Join(reasons, "; ") + "; to remove them run: orch source refresh " + s.ID + " --confirm-prune"
}

// indexable reports whether a repo directory may be indexed now: a git
// checkout rooted there only on its default branch, judged from its own .git
// (never an enclosing repository's), so a PR or feature branch is never
// indexed or pruned as the repo's docs; a plain directory as it is. When not,
// it says why.
func indexable(repoPath string) (bool, string) {
	co, ok := handoff.CheckoutAt(repoPath)
	if !ok {
		return true, ""
	}
	return co.DefaultBranchState()
}

// ingestRepoMarkdown walks a repo dir for *.md files, outside the excluded
// directories and other checkouts, and ingests each with repo + owner
// metadata under its checkout:// URI.
func ingestRepoMarkdown(ctx context.Context, repoPath, owner string, s Source, ing Ingester, opts RefreshOpts, listing *storedListing) repoResult {
	// Stored under the resolved path, the one the edit hand-off maps to.
	repoPath = resolve(repoPath)
	if ok, why := indexable(repoPath); !ok {
		log.Printf("sources: %s: %s; left as last indexed", repoPath, why)
		return repoResult{skipped: why}
	}
	repoName := filepath.Base(repoPath)
	readDates := opts.Dates
	if readDates == nil {
		readDates = gitdates.Read
	}
	var dates *gitdates.Dates
	prefix := CheckoutURI(repoPath) + "/"
	seen := map[string]bool{}
	r := repoResult{}
	complete := mdwalk.OwnFiles(repoPath, opts.ExcludeDirs, func(p string) {
		rel, _ := filepath.Rel(repoPath, p)
		seen[CheckoutURI(p)] = true
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		content, redactions := redact.Redact(string(raw))
		if redactions > redact.Threshold {
			return // keystore-like: skip entirely
		}
		tags := append([]string{}, s.Tags...)
		if owner != "" {
			tags = append(tags, "owner:"+owner)
		}
		tags = append(tags, "repo:"+repoName)
		if dates == nil { // read once, and only when a doc is stored
			d := readDates(ctx, repoPath)
			dates = &d
		}
		changed := dates.For(rel)
		if _, err := ing.IngestText(ctx, content, rag.Meta{
			SourceURI:  CheckoutURI(p),
			SourceType: "repo_doc",
			Scope:      s.Scope,
			Tags:       tags,
			StartedAt:  changed,
			EndedAt:    changed,
			Writer:     rag.WriterSources,
		}); err == nil {
			r.stored++
		}
	})
	r.found = len(seen) > 0
	if complete {
		if listing == nil {
			listing = &storedListing{prefix: prefix}
		}
		r.refused = pruneRemoved(ctx, ing, opts, s, prefix, seen, listing)
	}
	return r
}

// PruneGuard bounds deletions (implemented by *prunecap.Guard).
type PruneGuard interface {
	Allow(ctx context.Context, key, prefix string, stored, gone int) (bool, string, error)
	Done(ctx context.Context, prefix string, n int) error
}

// pruneRemoved deletes the docs this refresh stored under prefix that the
// walk no longer found, and returns why it refused to, if it did. Only
// chunks the sources refresh wrote are candidates, never an agent's note or
// another writer's doc. A walk that found nothing deletes nothing. With a
// guard, deletions are capped (see prunecap); without one, a refresh never
// deletes more than half of what is stored at once.
func pruneRemoved(ctx context.Context, ing Ingester, opts RefreshOpts, s Source, prefix string, seen map[string]bool, listing *storedListing) string {
	p, ok := ing.(Pruner)
	if !ok || len(seen) == 0 || ctx.Err() != nil {
		return ""
	}
	all, err := listing.get(ctx, p)
	if err != nil {
		log.Printf("sources: listing %s: %v; not pruning", listing.prefix, err)
		return ""
	}
	var stored, gone []string
	for _, uri := range all {
		if strings.HasPrefix(uri, prefix) {
			stored = append(stored, uri)
			if !seen[uri] {
				gone = append(gone, uri)
			}
		}
	}
	if len(gone) == 0 {
		return ""
	}
	if opts.Guard != nil {
		allowed, why, err := opts.Guard.Allow(ctx, prunecap.SourceKey(s.ID), prefix, len(stored), len(gone))
		if err != nil {
			log.Printf("sources: %s: prune guard: %v; not pruning", prefix, err)
			return ""
		}
		if !allowed {
			log.Printf("sources: %s: %s", prefix, why)
			return why
		}
	} else if len(gone) > max(minPruneCap, len(stored)/2) {
		return fmt.Sprintf("%d of %d stored docs are gone at once; none were removed", len(gone), len(stored))
	}
	removed := 0
	for _, uri := range gone {
		if _, err := p.DeleteSource(ctx, uri); err != nil {
			log.Printf("sources: removing %s, gone from its repo: %v", uri, err)
			continue
		}
		removed++
		log.Printf("sources: removed %s, gone from its repo", uri)
	}
	if opts.Guard != nil {
		_ = opts.Guard.Done(ctx, prefix, removed)
	}
	return ""
}

// storedListing is what the sources refresh stored under a prefix, listed
// once on first use.
type storedListing struct {
	prefix string
	uris   []string
	err    error
	done   bool
}

func (l *storedListing) get(ctx context.Context, p Pruner) ([]string, error) {
	if !l.done {
		l.uris, l.err = p.SourcesWrittenBy(ctx, l.prefix, rag.WriterSources)
		l.done = true
	}
	return l.uris, l.err
}

// minPruneCap is how many docs a refresh without a guard may always remove.
const minPruneCap = 5

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// stripHTML removes tags, leaving text content (a minimal extractor).
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

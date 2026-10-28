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
}

// CheckoutURI is the URI a repo or owner source stores a local file under:
// its absolute path. A local checkout is not the GitHub repo it was cloned
// from (it may be behind, ahead or on another branch), so it never shares the
// docs sync's repo://<owner>/<repo> names, and two checkouts of the same
// name never share one.
func CheckoutURI(path string) string {
	return "checkout://" + filepath.ToSlash(filepath.Clean(path))
}

// Refresh brings one source up to date and returns its new status
// ("ok" | "missing" | "error"). On absence it flags without deleting chunks.
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

// refreshOwner ingests markdown from every repo directory under <root>/<owner>.
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
	ingested, found := 0, false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, ok := ingestRepoMarkdown(ctx, filepath.Join(base, e.Name()), s.URI, s, ing, opts)
		ingested += n
		found = found || ok
	}
	if !found {
		return "missing", nil
	}
	return "ok", nil
}

func refreshRepo(ctx context.Context, s Source, repoPath, owner string, ing Ingester, opts RefreshOpts) (string, error) {
	if !isDir(repoPath) {
		return "missing", nil
	}
	if _, ok := ingestRepoMarkdown(ctx, repoPath, owner, s, ing, opts); !ok {
		return "missing", nil
	}
	return "ok", nil
}

// onDefaultBranch reports whether a repo directory may be indexed: a git
// checkout only on its default branch, so a PR or feature branch checked out
// there is never indexed (or pruned) as the repo's docs; a plain directory
// as it is.
func onDefaultBranch(repoPath string) bool {
	co, ok := handoff.FindCheckout(filepath.Join(repoPath, ".probe"))
	return !ok || co.OnDefaultBranch()
}

// ingestRepoMarkdown walks a repo dir for *.md files, outside the excluded
// directories, and ingests each with repo + owner metadata under its
// checkout:// URI. It returns how many files were stored, and whether the
// repo had any markdown to index (false also for a checkout not on its
// default branch, which is left as it was).
func ingestRepoMarkdown(ctx context.Context, repoPath, owner string, s Source, ing Ingester, opts RefreshOpts) (int, bool) {
	if !onDefaultBranch(repoPath) {
		log.Printf("sources: %s is not on its default branch; left as last indexed", repoPath)
		return 0, true
	}
	// Stored under the resolved path, the one the edit hand-off maps to.
	repoPath = resolve(repoPath)
	repoName := filepath.Base(repoPath)
	readDates := opts.Dates
	if readDates == nil {
		readDates = gitdates.Read
	}
	var dates *gitdates.Dates
	prefix := CheckoutURI(repoPath) + "/"
	seen := map[string]bool{}
	n := 0
	complete := mdwalk.FilesComplete(repoPath, opts.ExcludeDirs, func(p string) {
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
			n++
		}
	})
	if complete {
		pruneRemoved(ctx, ing, prefix, seen)
	}
	return n, len(seen) > 0
}

// maxPruneShare bounds what one refresh may delete: a walk that suddenly
// misses more than half of a repo's stored docs (a half-finished checkout, a
// sparse one) is more likely broken than a repo that lost them.
const (
	maxPruneShare = 0.5
	minPruneCap   = 5
)

// pruneRemoved deletes the docs this refresh stored under prefix that the
// walk no longer found. Only chunks the sources refresh wrote are candidates,
// never an agent's note or another writer's doc. A walk that found nothing
// deletes nothing, and neither does one that would delete more than half of
// what is stored (above a few docs).
func pruneRemoved(ctx context.Context, ing Ingester, prefix string, seen map[string]bool) {
	p, ok := ing.(Pruner)
	if !ok || len(seen) == 0 || ctx.Err() != nil {
		return
	}
	stored, err := p.SourcesWrittenBy(ctx, prefix, rag.WriterSources)
	if err != nil {
		log.Printf("sources: listing %s: %v; not pruning", prefix, err)
		return
	}
	var gone []string
	for _, uri := range stored {
		if strings.HasPrefix(uri, prefix) && !seen[uri] {
			gone = append(gone, uri)
		}
	}
	if len(gone) > max(minPruneCap, int(maxPruneShare*float64(len(stored)))) {
		log.Printf("sources: %s: %d of %d stored docs are gone at once; not pruning", prefix, len(gone), len(stored))
		return
	}
	for _, uri := range gone {
		if _, err := p.DeleteSource(ctx, uri); err != nil {
			log.Printf("sources: removing %s, gone from its repo: %v", uri, err)
			continue
		}
		log.Printf("sources: removed %s, gone from its repo", uri)
	}
}

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

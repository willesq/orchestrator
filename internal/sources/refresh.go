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
	"github.com/willesq/orchestrator/internal/mdwalk"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
)

// Ingester stores text into RAG (implemented by *rag.Service).
type Ingester interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
}

// Pruner lists and deletes stored sources (implemented by *rag.Service). An
// Ingester that is also a Pruner has the chunks of markdown files removed
// from a walked repo deleted on its next refresh.
type Pruner interface {
	ListSources(ctx context.Context, opts rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error)
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
	// Owned reports a URI another syncer keeps, such as a GitHub-synced doc;
	// pruning leaves those to it. nil owns nothing.
	Owned func(ctx context.Context, uri string) bool
	// Shared names repo:// prefixes more than one source stores under, which
	// pruning leaves alone; the key "" disables pruning. Set per scan.
	Shared map[string]bool
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
		SourceURI: s.URI, SourceType: KindWeb, Scope: s.Scope, Tags: s.Tags,
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
	stored := storedURIs(ctx, ing, "repo://"+s.URI+"/")
	ingested := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n := ingestRepoMarkdown(ctx, filepath.Join(base, e.Name()), s.URI, s, ing, opts, stored); n > 0 {
			ingested += n
		}
	}
	if ingested == 0 {
		return "missing", nil
	}
	return "ok", nil
}

func refreshRepo(ctx context.Context, s Source, repoPath, owner string, ing Ingester, opts RefreshOpts) (string, error) {
	if !isDir(repoPath) {
		return "missing", nil
	}
	stored := storedURIs(ctx, ing, "repo://"+ownerLabel(owner)+"/"+filepath.Base(repoPath)+"/")
	if ingestRepoMarkdown(ctx, repoPath, owner, s, ing, opts, stored) == 0 {
		return "missing", nil
	}
	return "ok", nil
}

func ownerLabel(owner string) string {
	if owner == "" {
		return "local"
	}
	return owner
}

// storedURIs lists the stored sources under prefix, or nil when ing cannot
// list them or the listing fails, which disables pruning.
func storedURIs(ctx context.Context, ing Ingester, prefix string) []string {
	p, ok := ing.(Pruner)
	if !ok {
		return nil
	}
	srcs, _, _, err := p.ListSources(ctx, rag.ListSourcesOpts{URIPrefix: prefix})
	if err != nil {
		log.Printf("sources: listing %s: %v", prefix, err)
		return nil
	}
	out := make([]string, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, src.SourceURI)
	}
	return out
}

// ingestRepoMarkdown walks a repo dir for *.md files, outside the excluded
// directories, and ingests each with repo + owner metadata. Returns how many
// files were ingested.
func ingestRepoMarkdown(ctx context.Context, repoPath, owner string, s Source, ing Ingester, opts RefreshOpts, stored []string) int {
	repoName := filepath.Base(repoPath)
	readDates := opts.Dates
	if readDates == nil {
		readDates = gitdates.Read
	}
	dates := readDates(ctx, repoPath)
	prefix := fmt.Sprintf("repo://%s/%s/", ownerLabel(owner), repoName)
	seen := map[string]bool{}
	n := 0
	complete := mdwalk.FilesComplete(repoPath, opts.ExcludeDirs, func(p string) {
		rel, _ := filepath.Rel(repoPath, p)
		seen[prefix+rel] = true
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
		changed := dates.For(rel)
		if _, err := ing.IngestText(ctx, content, rag.Meta{
			SourceURI:  prefix + rel,
			SourceType: "repo_doc",
			Scope:      s.Scope,
			Tags:       tags,
			StartedAt:  changed,
			EndedAt:    changed,
		}); err == nil {
			n++
		}
	})
	if complete {
		pruneRemoved(ctx, ing, opts, prefix, stored, seen)
	}
	return n
}

// pruneRemoved deletes the stored docs under prefix that the walk no longer
// found, leaving alone what another syncer owns. A walk that found nothing
// deletes nothing: an empty or unreadable checkout is not a repo whose docs
// were all removed.
func pruneRemoved(ctx context.Context, ing Ingester, opts RefreshOpts, prefix string, stored []string, seen map[string]bool) {
	p, ok := ing.(Pruner)
	if !ok || len(seen) == 0 || ctx.Err() != nil || opts.Shared[prefix] || opts.Shared[""] {
		return
	}
	for _, uri := range stored {
		if !strings.HasPrefix(uri, prefix) || seen[uri] || (opts.Owned != nil && opts.Owned(ctx, uri)) {
			continue
		}
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

package sources

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
)

// Ingester stores text into RAG (implemented by *rag.Service).
type Ingester interface {
	IngestText(ctx context.Context, text string, m rag.Meta) (int, error)
}

// RefreshOpts carries dependencies for refreshing sources.
type RefreshOpts struct {
	KnownRoots []string // dirs that contain owner/repo trees, e.g. ~/code
	HTTP       *http.Client
}

// Refresh brings one source up to date and returns its new status
// ("ok" | "missing" | "error"). On absence it flags without deleting chunks.
func Refresh(ctx context.Context, s Source, ing Ingester, opts RefreshOpts) (string, error) {
	switch s.Kind {
	case KindWeb:
		return refreshWeb(ctx, s, ing, opts)
	case KindRepo:
		return refreshRepo(ctx, s, s.URI, s.Owner, ing)
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
	ingested := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n := ingestRepoMarkdown(ctx, filepath.Join(base, e.Name()), s.URI, s, ing); n > 0 {
			ingested += n
		}
	}
	if ingested == 0 {
		return "missing", nil
	}
	return "ok", nil
}

func refreshRepo(ctx context.Context, s Source, repoPath, owner string, ing Ingester) (string, error) {
	if !isDir(repoPath) {
		return "missing", nil
	}
	if ingestRepoMarkdown(ctx, repoPath, owner, s, ing) == 0 {
		return "missing", nil
	}
	return "ok", nil
}

// ingestRepoMarkdown walks a repo dir for *.md files and ingests each with repo +
// owner metadata. Returns how many files were ingested.
func ingestRepoMarkdown(ctx context.Context, repoPath, owner string, s Source, ing Ingester) int {
	repoName := filepath.Base(repoPath)
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
		content, redactions := redact.Redact(string(raw))
		if redactions > redact.Threshold {
			return nil // keystore-like: skip entirely
		}
		rel, _ := filepath.Rel(repoPath, p)
		ownerLabel := owner
		if ownerLabel == "" {
			ownerLabel = "local"
		}
		tags := append([]string{}, s.Tags...)
		if owner != "" {
			tags = append(tags, "owner:"+owner)
		}
		tags = append(tags, "repo:"+repoName)
		if _, err := ing.IngestText(ctx, content, rag.Meta{
			SourceURI:  fmt.Sprintf("repo://%s/%s/%s", ownerLabel, repoName, rel),
			SourceType: "repo_doc",
			Scope:      s.Scope,
			Tags:       tags,
		}); err == nil {
			n++
		}
		return nil
	})
	return n
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

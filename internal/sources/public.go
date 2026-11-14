package sources

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
)

// leavePublic deletes, before a repo or owner source walks anything, the
// docs it stored under prefix in a public space that would not be public if
// written now (SP-030): a checkout whose GitHub repo turned private, whose
// visibility cannot be read, or that lost the proof that made it public.
// Reason: a refresh leaves a checkout that is off its default branch, or
// whose org binding does not check out, as last indexed, and walks repos
// one after another, so without this a repo made private could stay public
// for as long as it is skipped, or until the repos before it are indexed.
// A walked checkout is written again, placed where it belongs now.
func leavePublic(ctx context.Context, prefix string, ing Ingester, opts RefreshOpts) {
	if opts.LeavesPublic == nil {
		return
	}
	p, ok := ing.(Pruner)
	c, ok2 := ing.(docs.Copier)
	if !ok || !ok2 {
		return
	}
	uris, err := p.SourcesWrittenBy(ctx, prefix, rag.WriterSources)
	if err != nil {
		log.Printf("sources: listing %s to check visibility: %v", prefix, err)
		return
	}
	for _, uri := range uris {
		if ctx.Err() != nil {
			return
		}
		if !strings.HasPrefix(uri, prefix) {
			continue
		}
		cp, found, err := c.Copy(ctx, uri)
		if err != nil || !found {
			continue
		}
		path := filepath.FromSlash(strings.TrimPrefix(uri, "checkout://"))
		m := rag.Meta{SourceURI: uri, SourceType: "repo_doc", Writer: rag.WriterSources,
			Origin: rag.Origin{System: rag.OriginFS, Path: path}}
		if !opts.LeavesPublic(ctx, cp.Space, m) {
			continue
		}
		if _, err := p.DeleteSource(ctx, uri); err != nil {
			log.Printf("sources: %s: taking it out of the public space: %v", uri, err)
			continue
		}
		log.Printf("sources: %s: taken out of the public space", uri)
	}
}

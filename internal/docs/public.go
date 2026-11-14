package docs

import (
	"context"
	"fmt"
	"log"

	"github.com/willesq/orchestrator/internal/rag"
)

// leavePublic takes out of the public space the docs of every listed repo
// GitHub does not report public, before any repo is synced (SP-030).
// Reason: an unchanged repo is not synced again, so a repo made private
// without a new commit would otherwise stay readable by every session until
// its HEAD moved. A repo listed without a visibility GitHub vouched for (no
// owner node ID) counts as private.
//
// Each such doc is deleted from the store and its hash and the repo's HEAD
// are forgotten, so the repo is synced again, into the space it is placed in
// now. It returns the repos whose docs were taken out.
func leavePublic(ctx context.Context, reg *Registry, ing Ingester, o Owner, repos []Repo, opts SyncOpts) map[string]bool {
	if opts.LeavesPublic == nil {
		return nil
	}
	c, ok := ing.(Copier)
	rm, ok2 := ing.(Remover)
	if !ok || !ok2 {
		return nil
	}
	out := map[string]bool{}
	for _, rp := range repos {
		if ctx.Err() != nil {
			return out
		}
		origin := repoOrigin(o.Owner, rp)
		if origin.Visibility == "public" {
			continue
		}
		uris, err := reg.HashURIs(ctx, fmt.Sprintf("repo://%s/%s/", o.Owner, rp.Name))
		if err != nil {
			log.Printf("docs: %s/%s: listing docs to check visibility: %v", o.Owner, rp.Name, err)
			continue
		}
		for _, uri := range uris {
			cp, found, err := c.Copy(ctx, uri)
			if err != nil || !found {
				continue
			}
			m := rag.Meta{SourceURI: uri, SourceType: "repo_doc", Writer: rag.WriterDocs, Origin: origin}
			if !opts.LeavesPublic(ctx, cp.Space, m) {
				continue
			}
			out[rp.Name] = true
			if _, err := rm.DeleteSource(ctx, uri); err != nil {
				// Reason: the hash is kept, so the doc is still listed and
				// checked again next sync; forgotten, a failed sync of the
				// repo would leave the public copy unchecked for good.
				log.Printf("docs: %s: taking it out of the public space: %v", uri, err)
				continue
			}
			_ = reg.DeleteHash(ctx, uri)
		}
		if out[rp.Name] {
			_ = reg.ForgetRepoSHA(ctx, o.Owner, rp.Name)
			log.Printf("docs: %s/%s: docs taken out of the public space", o.Owner, rp.Name)
		}
	}
	return out
}

// firstOut orders repos whose docs left the public space first, so they are
// placed again before any other repo is synced; the rest keep their order.
func firstOut(todo []Repo, out map[string]bool) []Repo {
	if len(out) == 0 {
		return todo
	}
	sorted := make([]Repo, 0, len(todo))
	for _, rp := range todo {
		if out[rp.Name] {
			sorted = append(sorted, rp)
		}
	}
	for _, rp := range todo {
		if !out[rp.Name] {
			sorted = append(sorted, rp)
		}
	}
	return sorted
}

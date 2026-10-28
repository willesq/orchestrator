package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
)

// duplicate is a stored source that repeats a canonical one.
type duplicate struct {
	URI, Reason string
	Chunks      int32
}

func newRAGDedupeCmd(cf clientFactory) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "dedupe",
		Short: "Find path-keyed copies of repo docs and remove them; plans unless --apply",
		Long: `Finds documents stored under a file:// or claude:// URI that repeat what the docs or
sources sync already stores under a repo:// URI, and with --apply deletes them:

  - the file exists, and its real path sits in a registered repo or owner source, or in a
    checkout (a worktree included) of a repo the docs sync tracks;
  - that source's own URI for the file is stored by the docs or sources sync;
  - the two stored copies are identical, or the file as it is now is identical to the
    synced copy (the stored copy is an older version).

Text alone never makes a duplicate. A copy whose content differs (an edit not yet
merged) is kept.

It also lists the repo:// docs earlier versions stored for local repo and owner
sources (under repo://local/, or a registered owner the docs sync does not track),
with no writer recorded and no docs-sync hash: the refresh now keeps those under
checkout:// and no longer updates or prunes the old copies.

Without --apply it lists them and changes nothing. Edits made after this release no longer
create such copies; this cleans up those made before.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			v, err := config.Load()
			if err != nil {
				return err
			}
			s, err := config.Unmarshal(v)
			if err != nil {
				return err
			}
			// The sources registry maps local repos; without orch.db (a fresh
			// install) files still map through their own git checkout.
			var conn *sql.DB
			if c, err := db.OpenReader(); err == nil {
				conn = c
				defer conn.Close()
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), 10*time.Minute)
			defer cancel()
			home, _ := os.UserHomeDir()
			dups, err := planDedupe(ctx, client, sources.Canonical(conn, s.Sources.KnownRoots), home, fileExists)
			if err != nil {
				return err
			}
			legacy, err := planLegacy(ctx, client, conn)
			if err != nil {
				return err
			}
			dups = append(dups, legacy...)
			out := c.OutOrStdout()
			var chunks int32
			for _, d := range dups {
				chunks += d.Chunks
				fmt.Fprintf(out, "%6d  %s\n        %s\n", d.Chunks, termSafe(d.URI), termSafe(d.Reason))
			}
			if !apply {
				fmt.Fprintf(out, "\n%d duplicate source(s), %d chunk(s); rerun with --apply to delete them\n", len(dups), chunks)
				return nil
			}
			deleted := 0
			for _, d := range dups {
				if _, err := client.DeleteSource(ctx, &orchdv1.DeleteSourceRequest{SourceUri: d.URI, Confirm: true}); err != nil {
					return fmt.Errorf("deleting %s: %w (deleted %d before it)", termSafe(d.URI), err, deleted)
				}
				deleted++
			}
			fmt.Fprintf(out, "\ndeleted %d duplicate source(s), %d chunk(s)\n", deleted, chunks)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "delete the duplicates (default: list them, change nothing)")
	return cmd
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// pathOf is the file a path-keyed URI names, or "" for other URIs.
func pathOf(uri, home string) string {
	if p, ok := strings.CutPrefix(uri, "file://"); ok && filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if rest, ok := strings.CutPrefix(uri, "claude://"); ok && home != "" && rest != "" {
		return filepath.Join(home, ".claude", filepath.Clean("/"+rest))
	}
	return ""
}

// planDedupe lists the path-keyed sources that repeat a synced doc. A copy
// is listed only when all of these hold:
//
//   - its file still exists, and its real path (after symlinks) sits in a
//     registered source: a repo or owner source, or a checkout of a repo the
//     docs sync tracks;
//   - that source's own URI for the file is stored, by the docs or sources
//     sync (not by a client);
//   - the two stored copies are identical.
//
// Text alone never makes a duplicate, so a boilerplate file or ~/.claude's
// own config is never listed.
func planDedupe(ctx context.Context, client orchdv1.OrchdClient, canonical sources.CanonicalFunc, home string, exists func(string) bool) ([]duplicate, error) {
	var candidates []*orchdv1.SourceSummary
	for _, prefix := range []string{"file://", "claude://"} {
		resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{UriPrefix: prefix})
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, resp.GetSources()...)
	}
	doc := func(uri string) *orchdv1.Document {
		d, err := client.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: uri})
		if err != nil || d.GetOversize() {
			return nil
		}
		return d
	}
	var out []duplicate
	for _, c := range candidates {
		uri := c.GetSourceUri()
		p := pathOf(uri, home)
		if p == "" || canonical == nil || !exists(p) {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		var mine *orchdv1.Document
		for _, cu := range canonical(ctx, real) {
			synced := doc(cu)
			if synced == nil || (synced.GetWriter() != rag.WriterDocs && synced.GetWriter() != rag.WriterSources) {
				continue
			}
			if mine == nil {
				if mine = doc(uri); mine == nil {
					break
				}
			}
			if sameContent(mine, synced) {
				out = append(out, duplicate{URI: uri, Reason: "identical to " + cu + ", stored by the " + synced.GetWriter() + " sync", Chunks: c.GetChunks()})
				break
			}
			// The file as it is now matches the synced copy: the stored copy
			// is an older version of a doc the sync now holds, as the edit
			// hand-off would find on the next edit.
			if h := synced.GetContentHash(); h != "" && fileHash(real) == h {
				out = append(out, duplicate{URI: uri, Reason: "the file now matches " + cu + ", stored by the " + synced.GetWriter() +
					" sync; the stored copy is an older version", Chunks: c.GetChunks()})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

// maxDedupeFile bounds a file read to compare it with its synced copy.
const maxDedupeFile = 4 << 20

// fileHash is the content hash the sync would store for a file: its text
// redacted as every writer redacts it. Empty when it cannot be read.
func fileHash(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDedupeFile {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text, _ := redact.Redact(string(raw))
	return rag.ContentHash(text)
}

// planLegacy lists the repo:// docs earlier versions of the sources refresh
// stored for local repo and owner sources, which it now keeps under
// checkout://, so they are duplicates the refresh no longer updates or prunes.
// A doc is listed only when it has no writer recorded (stored before writers
// were), and the docs sync has no hash for it (so it is not a synced doc),
// under repo://local/ or under the owner of a registered owner source that
// the docs sync does not track.
func planLegacy(ctx context.Context, client orchdv1.OrchdClient, conn *sql.DB) ([]duplicate, error) {
	if conn == nil {
		return nil, nil
	}
	dreg, sreg := docs.NewRegistry(conn), sources.NewRegistry(conn)
	prefixes := []string{"repo://local/"}
	tracked := map[string]bool{}
	if owners, err := dreg.ListOwners(ctx); err == nil {
		for _, o := range owners {
			tracked[strings.ToLower(o.Owner)] = true
		}
	}
	if srcs, err := sreg.List(ctx); err == nil {
		for _, src := range srcs {
			if src.Kind == sources.KindOwner && !tracked[strings.ToLower(src.URI)] {
				prefixes = append(prefixes, "repo://"+src.URI+"/")
			}
		}
	}
	var out []duplicate
	for _, prefix := range prefixes {
		resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{UriPrefix: prefix, SourceType: "repo_doc"})
		if err != nil {
			return nil, err
		}
		for _, src := range resp.GetSources() {
			uri := src.GetSourceUri()
			if h, err := dreg.Hash(ctx, uri); err != nil || h != "" {
				continue
			}
			d, err := client.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: uri})
			if err != nil || d.GetWriter() != "" {
				continue
			}
			out = append(out, duplicate{URI: uri, Chunks: src.GetChunks(),
				Reason: "stored by an earlier sources refresh; the refresh now keeps this repo's docs under checkout://"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

// sameContent reports whether two stored copies hold the same text: by their
// content hashes when both have one, else by their rebuilt text. Empty text
// is never the same as anything.
func sameContent(a, b *orchdv1.Document) bool {
	if a.GetContentHash() != "" && b.GetContentHash() != "" {
		return a.GetContentHash() == b.GetContentHash()
	}
	return strings.TrimSpace(a.GetText()) != "" && a.GetText() == b.GetText()
}

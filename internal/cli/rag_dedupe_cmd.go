package cli

import (
	"context"
	"crypto/sha256"
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
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
)

// repoDoc is a repo:// doc's repo name and in-repo path.
type repoDoc struct{ repo, rel string }

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

  - the file sits in a checkout (a worktree included) of a GitHub repo, or in a registered
    repo or owner source, and the store holds that repo's repo:// copy of it;
  - the file is gone, and a repo:// doc of the same repo has its path (an old worktree's copy);
  - its stored text is identical to a repo:// doc's.

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

// planDedupe lists the path-keyed sources that repeat a repo:// doc.
func planDedupe(ctx context.Context, client orchdv1.OrchdClient, canonical sources.CanonicalFunc, home string, exists func(string) bool) ([]duplicate, error) {
	list := func(prefix string) ([]*orchdv1.SourceSummary, error) {
		resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{UriPrefix: prefix})
		if err != nil {
			return nil, err
		}
		return resp.GetSources(), nil
	}
	repos, err := list("repo://")
	if err != nil {
		return nil, err
	}
	var candidates []*orchdv1.SourceSummary
	for _, prefix := range []string{"file://", "claude://"} {
		srcs, err := list(prefix)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, srcs...)
	}
	// repo:// docs by in-repo path, for matching stale copies.
	byRel := map[string][]repoDoc{}
	stored := map[string]bool{}
	for _, r := range repos {
		stored[r.GetSourceUri()] = true
		parts := strings.SplitN(strings.TrimPrefix(r.GetSourceUri(), "repo://"), "/", 3)
		if len(parts) == 3 && parts[1] != "" && parts[2] != "" {
			byRel[parts[2]] = append(byRel[parts[2]], repoDoc{repo: parts[1], rel: parts[2]})
		}
	}
	var repoHashes map[[sha256.Size]byte]string
	text := func(uri string) (string, bool) {
		doc, err := client.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: uri})
		if err != nil || doc.GetOversize() {
			return "", false
		}
		return doc.GetText(), true
	}
	var out []duplicate
	for _, c := range candidates {
		uri := c.GetSourceUri()
		p := pathOf(uri, home)
		if p == "" {
			continue
		}
		reason := ""
		if canonical != nil {
			for _, cu := range canonical(ctx, p) {
				if stored[cu] {
					reason = "in a checkout of the repo; " + cu + " holds the doc"
					break
				}
			}
		}
		switch {
		case reason != "":
		case !exists(p):
			if r := staleCopyOf(p, byRel); r != "" {
				reason = "file is gone; repo://…/" + r + " holds the doc"
			}
		}
		if reason == "" {
			if repoHashes == nil {
				repoHashes = map[[sha256.Size]byte]string{}
				for _, r := range repos {
					if t, ok := text(r.GetSourceUri()); ok && strings.TrimSpace(t) != "" {
						repoHashes[sha256.Sum256([]byte(t))] = r.GetSourceUri()
					}
				}
			}
			if t, ok := text(uri); ok {
				if r, dup := repoHashes[sha256.Sum256([]byte(t))]; dup {
					reason = "same text as " + r
				}
			}
		}
		if reason != "" {
			out = append(out, duplicate{URI: uri, Reason: reason, Chunks: c.GetChunks()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

// staleCopyOf returns the in-repo path of a repo:// doc that a vanished file
// was a copy of: the file's path ends with that doc's path, and a directory
// above it is named for the repo (the checkout, "<repo>.worktrees" or an
// editor's workspace for it).
func staleCopyOf(p string, byRel map[string][]repoDoc) string {
	segs := strings.Split(filepath.ToSlash(p), "/")
	for i := len(segs) - 1; i > 0; i-- {
		rel := strings.Join(segs[i:], "/")
		// A bare file name (README.md) must sit right under the repo's
		// directory; a longer path is distinctive enough anywhere below it.
		above := segs[:i]
		if i == len(segs)-1 {
			above = segs[i-1 : i]
		}
		for _, d := range byRel[rel] {
			for _, s := range above {
				if strings.EqualFold(s, d.repo) || strings.HasPrefix(strings.ToLower(s), strings.ToLower(d.repo)+".") {
					return d.rel
				}
			}
		}
	}
	return ""
}

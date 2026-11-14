package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/handoff"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Bounds on a scan. Reason: the operator names the folders, but a folder
// such as the home directory can hold millions of entries, and the reply
// must stay small enough for the app.
const (
	maxScanFolders   = 32
	defaultScanDepth = 3
	maxScanDepth     = 6
)

// Variables so tests can reach them without building huge trees.
var (
	maxScanDirs      = 20000
	maxScanCheckouts = 2000
)

// maxScanLookups bounds the GitHub repo lookups one scan makes; the rest
// answer from the cache. Reason: the lookups' per-minute budget is shared
// with ExplainPlacement, which one scan must not use up.
const maxScanLookups = 15

// osDirFS opens dir on the real filesystem, read-only (fs.FS has no way to
// write), through an os.Root so that no path, even one swapped for a
// symbolic link during the walk, resolves outside dir.
func osDirFS(dir string) (fs.FS, func(), error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	return r.FS(), func() { _ = r.Close() }, nil
}

// checkoutAt reads a checkout's .git without running git (handoff).
func checkoutAt(dir string) (handoff.Checkout, bool) { return handoff.CheckoutAt(dir) }

// spaceScanner finds git checkouts under chosen folders and says where the
// applied policy places each one. Every dependency it holds only reads: the
// filesystem through fs.FS, the policy through spacesPlacer, GitHub through
// orchd's repo lookups and gh account checks (which cache in memory only).
type spaceScanner struct {
	fsys       func(dir string) (fs.FS, func(), error)
	checkoutAt func(dir string) (handoff.Checkout, bool)
	placer     spacesPlacer
	// repo looks up a repo's current owner and visibility.
	repo func(ctx context.Context, owner, name string) *spaces.Repo
	// account returns the login and orgs of gh profile directory dir
	// (defaultProfile: the default login); nil lists no accounts.
	account func(ctx context.Context, dir string) (string, []string, error)
}

// scanWalk is one scan's progress, shared across its folders.
type scanWalk struct {
	dirs      int
	truncated bool
	seen      map[string]bool
	found     []string
	problems  []string
}

func (sc *spaceScanner) scan(ctx context.Context, req *orchdv1.ScanSpacesRequest) (*orchdv1.ScanSpacesResponse, error) {
	folders, err := scanFolders(req.GetFolders())
	if err != nil {
		return nil, err
	}
	depth := int(req.GetMaxDepth())
	switch {
	case depth < 0:
		return nil, grpcstatus.Error(codes.InvalidArgument, "max_depth cannot be negative")
	case depth == 0:
		depth = defaultScanDepth
	case depth > maxScanDepth:
		depth = maxScanDepth
	}
	res, err := sc.placer.Current()
	if err != nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "the applied spaces policy cannot be read, so nothing can be placed: "+err.Error())
	}
	if req.GetOffline() {
		ctx = withCachedRepoLookups(ctx)
	}
	w := &scanWalk{seen: map[string]bool{}}
	for _, f := range folders {
		if err := ctx.Err(); err != nil {
			return nil, grpcstatus.FromContextError(err).Err()
		}
		sc.walk(ctx, w, f, depth)
	}
	sort.Strings(w.found)
	out := &orchdv1.ScanSpacesResponse{Truncated: w.truncated, Problems: w.problems}
	lookups := 0
	for _, dir := range w.found {
		if err := ctx.Err(); err != nil {
			return nil, grpcstatus.FromContextError(err).Err()
		}
		c, err := sc.checkout(ctx, res, dir, &lookups)
		if err != nil {
			return nil, err
		}
		out.Checkouts = append(out.Checkouts, c)
	}
	if !req.GetOffline() {
		out.Accounts = sc.accounts(ctx, res)
	}
	return out, nil
}

// scanFolders checks and de-duplicates the requested folders.
func scanFolders(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "name at least one folder to scan")
	}
	if len(in) > maxScanFolders {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "at most %d folders can be scanned at once", maxScanFolders)
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range in {
		if !filepath.IsAbs(f) || len(f) > maxPlacementTarget || strings.ContainsRune(f, 0) {
			return nil, grpcstatus.Error(codes.InvalidArgument, "folders must be absolute paths")
		}
		f = filepath.Clean(f)
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// walk looks through root to depth folders down. A folder holding a .git
// entry is a checkout and is not entered; hidden folders and symbolic links
// are never followed.
func (sc *spaceScanner) walk(ctx context.Context, w *scanWalk, root string, depth int) {
	fsys, done, err := sc.fsys(root)
	if err != nil {
		w.problems = append(w.problems, fmt.Sprintf("%s: %s", root, scanErr(err)))
		return
	}
	defer done()
	var visit func(rel string, level int)
	visit = func(rel string, level int) {
		if w.truncated || ctx.Err() != nil {
			return
		}
		if w.dirs >= maxScanDirs {
			w.truncated = true
			return
		}
		w.dirs++
		entries, err := fs.ReadDir(fsys, rel)
		if err != nil {
			if rel == "." {
				w.problems = append(w.problems, fmt.Sprintf("%s: %s", root, scanErr(err)))
			}
			return
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		for _, e := range entries {
			if e.Name() == ".git" {
				if !w.seen[abs] {
					if len(w.found) >= maxScanCheckouts {
						w.truncated = true
						return
					}
					w.seen[abs] = true
					w.found = append(w.found, abs)
				}
				return
			}
		}
		if level >= depth {
			return
		}
		for _, e := range entries {
			// DirFS reports a symbolic link as itself, not a folder, so
			// IsDir never follows one out of the chosen folders.
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			visit(path.Join(rel, e.Name()), level+1)
		}
	}
	visit(".", 0)
}

// scanErr says why a folder could not be read, without the path, which the
// problem already names.
func scanErr(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// checkout describes the checkout at dir and where res places it. lookups
// counts the scan's repo lookups so far.
func (sc *spaceScanner) checkout(ctx context.Context, res *spaces.Resolved, dir string, lookups *int) (*orchdv1.ScannedCheckout, error) {
	out := &orchdv1.ScannedCheckout{Path: dir}
	o := spaces.Origin{System: spaces.SystemFS, Path: dir}
	c, ok := sc.checkoutAt(dir)
	switch {
	case !ok, c.Anonymous:
		// An anonymous checkout's .git could borrow any repo's identity.
		out.Anonymous = ok
	case c.Owner != "":
		lctx := ctx
		if *lookups >= maxScanLookups {
			lctx = withCachedRepoLookups(ctx)
		}
		*lookups++
		r := sc.repo(lctx, c.Owner, c.Repo)
		o.Remote = r
		out.Owner, out.Repo, out.Visibility = r.Owner, r.Name, string(r.Visibility)
	}
	p, err := sc.placer.PlaceUnder(ctx, res, o)
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	out.Space, out.Rule, out.Outcome, out.Reason = p.Space, string(p.Rule), string(p.Outcome), p.Reason
	return out, nil
}

// accounts lists the default gh login's orgs and each space profile's, one
// entry per profile directory.
func (sc *spaceScanner) accounts(ctx context.Context, res *spaces.Resolved) []*orchdv1.ScannedAccount {
	if sc.account == nil {
		return nil
	}
	dirs := []string{defaultProfile}
	names := map[string][]string{}
	for _, p := range res.GHProfiles() {
		if p.ConfigDir == "" {
			continue
		}
		if _, ok := names[p.ConfigDir]; !ok {
			dirs = append(dirs, p.ConfigDir)
		}
		names[p.ConfigDir] = append(names[p.ConfigDir], p.Space)
	}
	var out []*orchdv1.ScannedAccount
	for _, dir := range dirs {
		a := &orchdv1.ScannedAccount{Profile: strings.Join(names[dir], ",")}
		login, orgs, err := sc.account(ctx, dir)
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Login, a.Orgs = login, orgs
		}
		out = append(out, a)
	}
	return out
}

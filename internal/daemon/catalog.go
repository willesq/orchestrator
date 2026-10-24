package daemon

import (
	"context"
	"errors"
	"log"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
)

// documentReader rebuilds an indexed source from its chunks.
type documentReader interface {
	Document(ctx context.Context, uri string) (rag.Document, error)
}

const (
	maxSourceURI = 4096
	// maxDocumentBytes keeps a GetDocument reply under the 4 MiB clients
	// accept by default.
	maxDocumentBytes = 3 << 20
	// maxDocFiles and maxDocListBytes bound a ListDocs reply the same way.
	maxDocFiles     = 10_000
	maxDocListBytes = 3 << 20
	// refreshNudgeGap stops repeated RefreshSource calls from restarting the
	// scheduler before it can finish a refresh.
	refreshNudgeGap = 10 * time.Second
)

var (
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	commitRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func (s *server) ListDocs(ctx context.Context, _ *orchdv1.ListDocsRequest) (*orchdv1.ListDocsResponse, error) {
	if s.docsReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "docs registry unavailable")
	}
	owners, err := s.docsReg.ListOwners(ctx)
	if err != nil {
		return nil, err
	}
	out := &orchdv1.ListDocsResponse{}
	heads := map[string]string{} // owner/repo → indexed commit
	for _, o := range owners {
		repos, err := s.docsReg.ListRepos(ctx, o.Owner)
		if err != nil {
			return nil, err
		}
		pb := &orchdv1.DocOwner{
			Owner: o.Owner, RefreshSecs: o.RefreshSecs, Scope: o.Scope, Tags: o.Tags,
			LastSynced: o.LastSynced, Status: o.Status,
		}
		for _, r := range repos {
			heads[r.Owner+"/"+r.Repo] = r.HeadSHA
			pb.Repos = append(pb.Repos, &orchdv1.DocRepo{
				Repo: r.Repo, HeadSha: r.HeadSHA, LastSynced: r.LastSynced, Files: clampInt32(r.Files),
				Status: r.Status, WebUrl: gitHubRepoURL(r.Owner, r.Repo),
			})
		}
		out.Owners = append(out.Owners, pb)
	}
	if s.rag == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "rag store unavailable")
	}
	// The catalog is what the store holds, not what the registry recorded:
	// a doc whose ingest failed, or whose content was deleted, is not listed.
	stored, _, _, err := s.rag.ListSources(ctx, rag.ListSourcesOpts{SourceType: "repo_doc", URIPrefix: "repo://"})
	if err != nil {
		return nil, err
	}
	recorded, err := s.docsReg.ListDocs(ctx)
	if err != nil {
		return nil, err
	}
	changed := make(map[string]int64, len(recorded))
	for _, d := range recorded {
		changed[d.URI] = d.UpdatedAt
	}
	synced := map[string]bool{}
	for _, o := range owners {
		synced[o.Owner] = true
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].SourceURI < stored[j].SourceURI })
	size := proto.Size(out)
	for _, src := range stored {
		owner, repo, path, ok := splitRepoURI(src.SourceURI)
		// Only docs `orch docs` stored: the registry records each one, and
		// only orchd's sync writes it. Any client can store text under a
		// repo:// URI, and a local owner folder can share a synced owner's name.
		if _, recordedDoc := changed[src.SourceURI]; !ok || !synced[owner] || !recordedDoc {
			continue
		}
		if len(out.Files) == maxDocFiles || size > maxDocListBytes {
			out.Truncated = true
			break
		}
		f := &orchdv1.DocFile{
			SourceUri: src.SourceURI, Owner: owner, Repo: repo, Path: path,
			Chunks: clampInt32(src.Chunks), IngestedAt: toUnix(src.IngestedAt), UpdatedAt: changed[src.SourceURI],
		}
		if f.UpdatedAt == 0 {
			f.UpdatedAt = f.IngestedAt
		}
		if sha, tracked := heads[owner+"/"+repo]; tracked {
			f.WebUrl = gitHubFileURL(owner, repo, sha, path)
		}
		size += proto.Size(f)
		out.Files = append(out.Files, f)
	}
	return out, nil
}

func (s *server) GetDocument(ctx context.Context, req *orchdv1.GetDocumentRequest) (*orchdv1.Document, error) {
	if s.documents == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "document store unavailable")
	}
	uri := req.GetSourceUri()
	if uri == "" || len(uri) > maxSourceURI {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "source_uri must be 1 to %d characters", maxSourceURI)
	}
	doc, err := s.documents.Document(ctx, uri)
	if errors.Is(err, rag.ErrSourceNotFound) {
		return nil, grpcstatus.Errorf(codes.NotFound, "nothing is indexed under %q", uri)
	}
	if err != nil {
		return nil, err
	}
	if len(doc.Text) > maxDocumentBytes {
		doc.Text, doc.Oversize = "", true
	}
	return &orchdv1.Document{
		SourceUri: uri, SourceType: doc.Meta.SourceType, Scope: doc.Meta.Scope, Tags: doc.Meta.Tags,
		Text: doc.Text, Chunks: clampInt32(doc.Chunks), Malformed: doc.Malformed, Oversize: doc.Oversize,
		WebUrl: s.webURLFor(ctx, uri, doc.Meta.SourceType),
	}, nil
}

// webURLFor is where an indexed source's original lives: the URL itself for a
// fetched web page, the file on GitHub at the indexed commit for a synced doc,
// and nothing otherwise. Any client can store text under any URI, so only a
// web source's URI is offered as a link.
func (s *server) webURLFor(ctx context.Context, uri, sourceType string) string {
	if sourceType == sources.KindWeb && isWebURL(uri) {
		return uri
	}
	if sourceType != "repo_doc" {
		return ""
	}
	owner, repo, path, ok := splitRepoURI(uri)
	if !ok || s.docsReg == nil {
		return ""
	}
	// Only docs the GitHub sync owns: an owner source's local directory of the
	// same name is not necessarily on GitHub.
	if h, err := s.docsReg.Hash(ctx, uri); err != nil || h == "" {
		return ""
	}
	sha, err := s.docsReg.RepoSHA(ctx, owner, repo)
	if err != nil {
		return ""
	}
	return gitHubFileURL(owner, repo, sha, path)
}

func (s *server) ListRegisteredSources(ctx context.Context, _ *orchdv1.ListRegisteredSourcesRequest) (*orchdv1.ListRegisteredSourcesResponse, error) {
	if s.sourcesReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "sources registry unavailable")
	}
	list, err := s.sourcesReg.List(ctx)
	if err != nil {
		return nil, err
	}
	out := &orchdv1.ListRegisteredSourcesResponse{}
	for _, src := range list {
		out.Sources = append(out.Sources, registeredSource(src))
	}
	return out, nil
}

func (s *server) SaveSource(ctx context.Context, req *orchdv1.SaveSourceRequest) (*orchdv1.RegisteredSource, error) {
	if s.sourcesReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "sources registry unavailable")
	}
	in := req.GetSource()
	src := sources.Source{
		Kind: in.GetKind(), URI: in.GetUri(), Scope: in.GetScope(), Tags: in.GetTags(), RefreshSecs: in.GetRefreshSecs(),
	}
	if err := sources.Validate(&src); err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	src.ID = sources.ID(src.Kind, src.URI)
	replace := req.GetReplaceId()
	if replace != "" && replace != src.ID {
		if _, err := s.findSource(ctx, replace); err != nil {
			return nil, err
		}
	}
	if err := s.sourcesReg.Add(ctx, src); err != nil {
		return nil, err
	}
	// The new entry is stored before the old one goes, so a failure leaves
	// both rather than neither.
	if replace != "" && replace != src.ID {
		if err := s.sourcesReg.Remove(ctx, replace); err != nil {
			return nil, err
		}
	}
	log.Printf("orchd: source %s saved", src.ID)
	saved, err := s.findSource(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	return registeredSource(saved), nil
}

func (s *server) RemoveSource(ctx context.Context, req *orchdv1.RemoveSourceRequest) (*orchdv1.RemoveSourceResponse, error) {
	if s.sourcesReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "sources registry unavailable")
	}
	src, err := s.findSource(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	out := &orchdv1.RemoveSourceResponse{}
	if req.GetPurge() {
		if !req.GetConfirm() {
			return nil, grpcstatus.Error(codes.FailedPrecondition,
				"refusing to delete indexed content without confirm=true: this is permanent and cannot be undone")
		}
		// Purge before unregistering, so a failed purge leaves the source
		// registered and the removal can be retried.
		uris, err := s.indexedBy(ctx, src)
		if err != nil {
			return nil, err
		}
		log.Printf("orchd: DELETE %d indexed source(s) of %s — permanent, no undo", len(uris), src.ID)
		for _, uri := range uris {
			n, err := s.rag.DeleteSource(ctx, uri)
			if err != nil {
				log.Printf("orchd: DELETE %q failed: %v", uri, err)
				return nil, err
			}
			out.PurgedSources++
			out.PurgedChunks += clampInt32(n)
		}
	}
	if err := s.sourcesReg.Remove(ctx, src.ID); err != nil {
		return nil, err
	}
	log.Printf("orchd: source %s removed", src.ID)
	return out, nil
}

func (s *server) RefreshSource(ctx context.Context, req *orchdv1.RefreshSourceRequest) (*orchdv1.RefreshSourceResponse, error) {
	if s.sourcesReg == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "sources registry unavailable")
	}
	marked := 1
	if id := req.GetId(); id != "" {
		if _, err := s.findSource(ctx, id); err != nil {
			return nil, err
		}
	} else {
		list, err := s.sourcesReg.List(ctx)
		if err != nil {
			return nil, err
		}
		marked = len(list)
	}
	if err := s.sourcesReg.MarkDue(ctx, req.GetId()); err != nil {
		return nil, err
	}
	// The scheduler ticks as soon as it starts, so restarting it refreshes the
	// due sources now rather than at its next interval.
	if s.sup != nil && s.nudgeDue() {
		if err := s.sup.Restart(UnitSources); err != nil {
			log.Printf("orchd: refresh now: %v; sources refresh at the next scan", err)
		}
	}
	return &orchdv1.RefreshSourceResponse{Marked: clampInt32(marked)}, nil
}

// nudgeDue reports whether the scheduler may be restarted now, at most once
// per refreshNudgeGap; a restart in between would cut short the refresh the
// last one started, which already covers anything marked due since.
func (s *server) nudgeDue() bool {
	s.nudgeMu.Lock()
	defer s.nudgeMu.Unlock()
	now := time.Now()
	if !s.lastNudge.IsZero() && now.Sub(s.lastNudge) < refreshNudgeGap {
		return false
	}
	s.lastNudge = now
	return true
}

func (s *server) findSource(ctx context.Context, id string) (sources.Source, error) {
	if id == "" {
		return sources.Source{}, grpcstatus.Error(codes.InvalidArgument, "id is required")
	}
	list, err := s.sourcesReg.List(ctx)
	if err != nil {
		return sources.Source{}, err
	}
	for _, src := range list {
		if src.ID == id {
			return src, nil
		}
	}
	return sources.Source{}, grpcstatus.Errorf(codes.NotFound, "no source %q", id)
}

// indexedBy lists the indexed sources a registered source produced. A repo or
// owner directory's docs share the repo:// form with the GitHub docs sync, so
// docs that sync owns are left out.
func (s *server) indexedBy(ctx context.Context, src sources.Source) ([]string, error) {
	// A row stored before validation existed may not describe what it says.
	check := src
	if err := sources.Validate(&check); err != nil || check.URI != src.URI {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition,
			"source %q is not a valid source; remove it without purge", src.ID)
	}
	if src.Kind == sources.KindWeb {
		return []string{src.URI}, nil
	}
	if src.Kind == sources.KindRepo {
		// Repo sources are indexed by directory name, so two of the same name
		// share their content.
		list, err := s.sourcesReg.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, other := range list {
			if other.ID != src.ID && other.Kind == sources.KindRepo && filepath.Base(other.URI) == filepath.Base(src.URI) {
				return nil, grpcstatus.Errorf(codes.FailedPrecondition,
					"%s shares its indexed content with %s; remove it without purge", src.ID, other.ID)
			}
		}
	}
	synced, err := s.docsOwners(ctx)
	if err != nil {
		return nil, err
	}
	tag := searchTag(src)
	list, _, _, err := s.rag.ListSources(ctx, rag.ListSourcesOpts{URIPrefix: sourcePrefix(src)})
	if err != nil {
		return nil, err
	}
	var uris []string
	for _, sum := range list {
		// Only what this source's refresh stores: markdown carrying its tag.
		if sum.SourceType != "repo_doc" || !slices.Contains(sum.Tags, tag) {
			continue
		}
		// The GitHub docs sync shares the repo:// form; its docs are not this
		// source's to delete, even after its caches were cleared.
		if owner, _, _, ok := splitRepoURI(sum.SourceURI); ok && synced[owner] {
			continue
		}
		if s.docsReg != nil {
			if h, err := s.docsReg.Hash(ctx, sum.SourceURI); err != nil {
				return nil, err
			} else if h != "" {
				continue
			}
		}
		uris = append(uris, sum.SourceURI)
	}
	return uris, nil
}

// docsOwners is the set of owners the GitHub docs sync tracks.
func (s *server) docsOwners(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if s.docsReg == nil {
		return out, nil
	}
	owners, err := s.docsReg.ListOwners(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range owners {
		out[o.Owner] = true
	}
	return out, nil
}

// searchTag is the tag refresh puts on every doc a repo or owner source
// indexes (see sources.ingestRepoMarkdown).
func searchTag(src sources.Source) string {
	if src.Kind == sources.KindOwner {
		return "owner:" + src.URI
	}
	return "repo:" + filepath.Base(src.URI)
}

// sourcePrefix is the repo:// prefix a repo or owner source's docs are
// indexed under (see sources.ingestRepoMarkdown).
func sourcePrefix(src sources.Source) string {
	if src.Kind == sources.KindOwner {
		return "repo://" + src.URI + "/"
	}
	return "repo://local/" + filepath.Base(src.URI) + "/"
}

func registeredSource(src sources.Source) *orchdv1.RegisteredSource {
	pb := &orchdv1.RegisteredSource{
		Id: src.ID, Kind: src.Kind, Uri: src.URI, Scope: src.Scope, Tags: src.Tags,
		RefreshSecs: src.RefreshSecs, LastFetched: src.LastFetched, Status: src.Status,
	}
	switch src.Kind {
	case sources.KindWeb:
		pb.SearchSourceUri = src.URI
		if isWebURL(src.URI) {
			pb.WebUrl = src.URI
		}
	case sources.KindRepo, sources.KindOwner:
		pb.SearchAllTags = []string{searchTag(src)}
	}
	return pb
}

// splitRepoURI splits repo://<owner>/<repo>/<path>.
func splitRepoURI(uri string) (owner, repo, path string, ok bool) {
	rest, found := strings.CutPrefix(uri, "repo://")
	if !found {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func isWebURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

func gitHubRepoURL(owner, repo string) string {
	if !github.ValidLogin(owner) || !repoNameRe.MatchString(repo) || strings.Trim(repo, ".") == "" {
		return ""
	}
	return "https://github.com/" + owner + "/" + repo
}

// gitHubFileURL links a file at the commit it was indexed from, so the page
// matches what was indexed; without a known commit it links the default branch.
func gitHubFileURL(owner, repo, sha, path string) string {
	base := gitHubRepoURL(owner, repo)
	if base == "" {
		return ""
	}
	ref := "HEAD"
	if commitRe.MatchString(sha) {
		ref = sha
	}
	var segs []string
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ""
		}
		segs = append(segs, url.PathEscape(seg))
	}
	return base + "/blob/" + ref + "/" + strings.Join(segs, "/")
}

const (
	maxFilterTags   = 32
	maxFilterTagLen = 256
)

// checkTags bounds a search's tag filters, each of which becomes a Qdrant
// condition.
func checkTags(lists ...[]string) error {
	for _, tags := range lists {
		if len(tags) > maxFilterTags {
			return grpcstatus.Errorf(codes.InvalidArgument, "at most %d tags per filter", maxFilterTags)
		}
		for _, t := range tags {
			if len(t) > maxFilterTagLen {
				return grpcstatus.Errorf(codes.InvalidArgument, "tag filters are at most %d bytes", maxFilterTagLen)
			}
		}
	}
	return nil
}

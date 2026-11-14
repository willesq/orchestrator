package daemon

import (
	"context"
	"slices"
	"sort"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// ListSpaces lists the applied policy's spaces for the Spaces pane (SP-050).
// When the applied policy cannot be read the reply carries the status, whose
// error says why, and no spaces.
func (s *server) ListSpaces(ctx context.Context, _ *orchdv1.ListSpacesRequest) (*orchdv1.ListSpacesResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_ListSpaces_FullMethodName, "list the spaces"); err != nil {
		return nil, err
	}
	if s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	out := &orchdv1.ListSpacesResponse{Status: s.spacesStatus(ctx)}
	res, err := s.spaces.Current()
	if err != nil || !res.Configured() {
		return out, nil
	}
	out.PublicRootSessions = res.PublicRootSessions
	names := make([]string, 0, len(res.Policy.Spaces))
	for _, sp := range res.Policy.Spaces {
		names = append(names, sp.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		sp, _ := res.Space(name)
		var readable []string
		for _, other := range names {
			if res.CanRead(name, other) {
				readable = append(readable, other)
			}
		}
		out.Spaces = append(out.Spaces, &orchdv1.SpaceInfo{
			Name:        sp.Name,
			Kind:        string(sp.Kind),
			GithubOrgs:  slices.Clone(sp.GitHubOrgs),
			Roots:       slices.Clone(sp.Roots),
			WebPrefixes: slices.Clone(sp.WebPrefixes),
			Chat:        slices.Clone(sp.Chat),
			Reads:       slices.Clone(sp.Reads),
			Readable:    readable,
			TermsFile:   sp.TermsFile,
			GhConfigDir: sp.GHConfigDir,
		})
	}
	return out, nil
}

// ScanSpaces walks the folders the operator chose and reports what it finds
// (SP-050). See spaceScanner for what it reads; it writes nothing.
func (s *server) ScanSpaces(ctx context.Context, req *orchdv1.ScanSpacesRequest) (*orchdv1.ScanSpacesResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_ScanSpaces_FullMethodName, "scan folders for spaces"); err != nil {
		return nil, err
	}
	if s.spaces == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	// Reason: a scan can read thousands of folders and start gh checks, so
	// a client repeating the request must not run several at once.
	select {
	case scanSlot <- struct{}{}:
		defer func() { <-scanSlot }()
	default:
		return nil, grpcstatus.Error(codes.Aborted, "a scan is already running; try again when it ends")
	}
	return newSpaceScanner(s).scan(ctx, req)
}

// scanSlot admits one ScanSpaces at a time.
var scanSlot = make(chan struct{}, 1)

// newSpaceScanner reads the real filesystem, orchd's repo lookups and the
// gh profiles' cached account checks.
func newSpaceScanner(s *server) *spaceScanner {
	sc := &spaceScanner{
		fsys:       osDirFS,
		checkoutAt: checkoutAt,
		placer:     s.spaces,
		repo:       s.repos.lookup,
	}
	if s.ghProfiles != nil {
		sc.account = func(ctx context.Context, dir string) (string, []string, error) {
			c := s.ghProfiles.check(ctx, dir)
			return c.login, slices.Clone(c.orgs), c.err
		}
	}
	return sc
}

// spacesPlacer is the part of the spaces engine a scan uses: it only reads.
type spacesPlacer interface {
	Current() (*spaces.Resolved, error)
	PlaceUnder(ctx context.Context, res *spaces.Resolved, o spaces.Origin) (spaces.Placement, error)
}

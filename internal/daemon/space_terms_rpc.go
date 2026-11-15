package daemon

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"sync"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The write-only terms field of the app's Spaces editor (SP-053). A term
// never leaves orchd (INV-07): GetSpaceTerms answers with counts and keyed
// fingerprints, and SetSpaceTerms takes a whole file and answers the same.
// Only the applied policy's terms files are read or written, so a draft
// (which an agent session may have saved) cannot point orchd at a file.

// termsKeyOnce holds the fingerprint key, read or made on first use.
var termsKeyOnce struct {
	sync.Mutex
	key []byte
}

// termsFingerprintKey is the secret fingerprints are keyed with, kept
// (0600) in orchd's data folder so a fingerprint stays the same across
// restarts while the file does.
var termsFingerprintKey = func() ([]byte, error) {
	termsKeyOnce.Lock()
	defer termsKeyOnce.Unlock()
	if termsKeyOnce.key != nil {
		return termsKeyOnce.key, nil
	}
	// ensureQdrantAPIKey persists any random hex key; it is not Qdrant's.
	k, err := ensureQdrantAPIKey(filepath.Join(paths.DataDir(), "terms-fingerprint-key"))
	if err != nil {
		return nil, err
	}
	termsKeyOnce.key = []byte(k)
	return termsKeyOnce.key, nil
}

// Reason: names each space's terms file path; operator-only like ListSpaces.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_GetSpaceTerms_FullMethodName, "read the terms files",
	spaceDecision{spaceOperator, "every space's terms file path, term count and fingerprint, never a term"}))

// GetSpaceTerms describes every applied space's terms file.
func (s *server) GetSpaceTerms(ctx context.Context, _ *orchdv1.GetSpaceTermsRequest) (*orchdv1.SpaceTermsList, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_GetSpaceTerms_FullMethodName, "read the terms files"); err != nil {
		return nil, err
	}
	res, key, err := s.termsSetup()
	if err != nil {
		return nil, err
	}
	out := &orchdv1.SpaceTermsList{}
	if !res.Configured() {
		return out, nil
	}
	names := res.Names()
	sort.Strings(names)
	for _, name := range names {
		sp, _ := res.Space(name)
		t := &orchdv1.SpaceTerms{Space: name, TermsFile: sp.TermsFile}
		if path := res.TermsFile(name, s.home); path != "" {
			info := spaces.DescribeTerms(name, path, key)
			t.Exists, t.Count, t.Fingerprint, t.Problem = info.Exists, int32(info.Count), info.Fingerprint, info.Problem
		}
		out.Spaces = append(out.Spaces, t)
	}
	return out, nil
}

// Reason (SP-053, D-78): the terms decide what the outbound check refuses,
// and replacing them can drop some, so this stays Orch.app's alone.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_SetSpaceTerms_FullMethodName, "change a space's terms",
	spaceDecision{spaceOperator, "replaces one space's terms file; answers with its count and fingerprint only"}))

// SetSpaceTerms replaces one applied space's terms file.
func (s *server) SetSpaceTerms(ctx context.Context, req *orchdv1.SetSpaceTermsRequest) (*orchdv1.SpaceTerms, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_SetSpaceTerms_FullMethodName, "change a space's terms"); err != nil {
		return nil, err
	}
	res, key, err := s.termsSetup()
	if err != nil {
		return nil, err
	}
	name := req.GetSpace()
	sp, ok := res.Space(name)
	if !ok {
		return nil, grpcstatus.Error(codes.NotFound, "no applied space has that name")
	}
	path := res.TermsFile(name, s.home)
	if path == "" {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition,
			"space %q names no terms file: set terms_file in the policy and apply it first", name)
	}
	info, err := spaces.WriteTerms(name, path, s.home, []byte(req.GetTerms()), key)
	if err != nil {
		// The error names the space and a line at most (INV-07).
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	log.Printf("orchd: the operator replaced the terms file of space %q (%d terms)", name, info.Count)
	return &orchdv1.SpaceTerms{Space: name, TermsFile: sp.TermsFile, Exists: true, Count: int32(info.Count),
		Fingerprint: info.Fingerprint}, nil
}

// termsSetup is the applied policy and the fingerprint key.
func (s *server) termsSetup() (*spaces.Resolved, []byte, error) {
	if s.spaces == nil {
		return nil, nil, grpcstatus.Error(codes.Unavailable, "spaces are unavailable")
	}
	res, err := s.spaces.Current()
	if err != nil {
		return nil, nil, grpcstatus.Error(codes.FailedPrecondition, "the applied spaces policy cannot be read")
	}
	key, err := termsFingerprintKey()
	if err != nil {
		return nil, nil, grpcstatus.Error(codes.Internal, "the terms fingerprint key cannot be read")
	}
	return res, key, nil
}

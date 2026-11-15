package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Some commands read orch's files directly (the docs registry in orch.db,
// the PR cache), which hold every space's records. Under a spaces policy
// they show only what orchd lists for the caller, which orchd decides from
// the caller's process (SP-026, RC-712); with no policy ever applied they
// read the files as before (INV-10).

// policyApplied reports whether orch.db has applied a spaces policy, read
// only; tests replace it. A store it cannot read counts as applied, so the
// commands ask orchd rather than show every space's records.
var policyApplied = func(ctx context.Context) bool {
	applied, err := spaces.HasApplied(ctx, paths.DBFile())
	return err != nil || applied
}

// spacesListTimeout bounds asking orchd what a list may show.
const spacesListTimeout = 10 * time.Second

// orchdLists is what orchd lets the caller see, asked only under a policy.
type orchdLists struct {
	cf clientFactory
}

// errListFromOrchd explains a list that cannot be shown because orchd did
// not answer under a policy.
func errListFromOrchd(what string, err error) error {
	return fmt.Errorf("under a spaces policy %s come from orchd, which did not answer: %w", what, err)
}

// docs is orchd's ListDocs reply for the caller.
func (l orchdLists) docs(ctx context.Context) (*orchdv1.ListDocsResponse, error) {
	client, cleanup, err := l.cf()
	if err != nil {
		return nil, errListFromOrchd("synced docs", err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, spacesListTimeout)
	defer cancel()
	rep, err := client.ListDocs(ctx, &orchdv1.ListDocsRequest{})
	if err != nil {
		return nil, errListFromOrchd("synced docs", err)
	}
	return rep, nil
}

// docOwners is the set of docs owners orchd lists for the caller.
func (l orchdLists) docOwners(ctx context.Context) (map[string]bool, error) {
	rep, err := l.docs(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range rep.GetOwners() {
		out[o.GetOwner()] = true
	}
	return out, nil
}

// prURLs is the set of PR URLs orchd lists for the caller.
func (l orchdLists) prURLs(ctx context.Context) (map[string]bool, error) {
	client, cleanup, err := l.cf()
	if err != nil {
		return nil, errListFromOrchd("PRs", err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, spacesListTimeout)
	defer cancel()
	rep, err := client.ListPRs(ctx, &orchdv1.ListPRsRequest{})
	if err != nil {
		return nil, errListFromOrchd("PRs", err)
	}
	out := map[string]bool{}
	for _, pr := range rep.GetPrs() {
		out[pr.GetUrl()] = true
	}
	return out, nil
}

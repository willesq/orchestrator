package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// listsClient answers ListDocs and ListPRs with what orchd lets the caller
// see.
type listsClient struct {
	*fakeClient
	docs *orchdv1.ListDocsResponse
	prs  *orchdv1.ListPRsResponse
}

func (c listsClient) ListDocs(context.Context, *orchdv1.ListDocsRequest, ...grpc.CallOption) (*orchdv1.ListDocsResponse, error) {
	return c.docs, nil
}

func (c listsClient) ListPRs(context.Context, *orchdv1.ListPRsRequest, ...grpc.CallOption) (*orchdv1.ListPRsResponse, error) {
	return c.prs, nil
}

func underPolicy(t *testing.T, applied bool) {
	t.Helper()
	old := policyApplied
	t.Cleanup(func() { policyApplied = old })
	policyApplied = func(context.Context) bool { return applied }
}

// TestListsUnderAPolicyComeFromOrchd is RC-712 and SP-026's CLI side: under
// a spaces policy 'orch docs list', 'orch docs owners list', 'orch docs
// status' and 'orch prs' show only what orchd lists for the caller, and
// fail when orchd does not answer, rather than read every space's records
// from orch's files; with no policy they read the files as before.
func TestListsUnderAPolicyComeFromOrchd(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	reg, closeFn, err := openDocsRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, owner := range []string{"acme", "globex"} {
		if err := reg.AddOwner(ctx, docs.Owner{Owner: owner, RefreshSecs: 3600}); err != nil {
			t.Fatal(err)
		}
		if err := reg.SetHash(ctx, "repo://"+owner+"/app/README.md", "h", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	closeFn()
	if err := prs.SaveState(paths.PRStateFile(), prs.State{PRs: map[string]prs.PR{
		"https://github.com/acme/app/pull/1":   {URL: "https://github.com/acme/app/pull/1", Title: "acme pr", Repo: "acme/app", Reasons: []string{prs.ReasonAuthored}},
		"https://github.com/globex/app/pull/2": {URL: "https://github.com/globex/app/pull/2", Title: "globex pr", Repo: "globex/app", Reasons: []string{prs.ReasonAuthored}},
	}}); err != nil {
		t.Fatal(err)
	}
	client := listsClient{fakeClient: &fakeClient{},
		docs: &orchdv1.ListDocsResponse{
			Owners: []*orchdv1.DocOwner{{Owner: "acme"}},
			Files:  []*orchdv1.DocFile{{SourceUri: "repo://acme/app/README.md", Owner: "acme", Repo: "app", UpdatedAt: time.Now().Unix()}},
		},
		prs: &orchdv1.ListPRsResponse{Prs: []*orchdv1.PullRequest{{Url: "https://github.com/acme/app/pull/1"}}},
	}
	commands := [][]string{{"docs", "list"}, {"docs", "owners", "list"}, {"docs", "status"}, {"prs"}}

	underPolicy(t, true)
	for _, args := range commands {
		out, err := run(factory(client, nil), "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out, "acme") || strings.Contains(out, "globex") {
			t.Errorf("%v under a policy:\n%s", args, out)
		}
		if _, err := run(factory(nil, errors.New("no socket")), "", args...); err == nil || !strings.Contains(err.Error(), "under a spaces policy") {
			t.Errorf("%v with orchd down: %v", args, err)
		}
	}
	client.docs = &orchdv1.ListDocsResponse{Truncated: true}
	if out, _ := run(factory(client, nil), "", "docs", "list"); !strings.Contains(out, "no docs ingested") {
		t.Errorf("an empty list from orchd:\n%s", out)
	}

	underPolicy(t, false)
	for _, args := range commands {
		out, err := run(factory(client, nil), "", args...)
		if err != nil || !strings.Contains(out, "globex") {
			t.Errorf("%v with no policy: %v\n%s", args, err, out)
		}
	}
}

// TestPolicyAppliedReadsOrchDB: with no orch.db nothing was applied.
func TestPolicyAppliedReadsOrchDB(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if policyApplied(context.Background()) {
		t.Error("a missing orch.db applied a policy")
	}
}

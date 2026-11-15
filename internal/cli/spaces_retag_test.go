package cli

import (
	"errors"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSpacesRetagSendsTheRequestAndPrintsCounts: 'orch spaces retag' asks
// orchd to move the source, passes --confirm and --expect-from, and prints
// counts and space names only.
func TestSpacesRetagSendsTheRequestAndPrintsCounts(t *testing.T) {
	saved := fakeRetag
	t.Cleanup(func() { fakeRetag = saved })
	var got []*orchdv1.RetagSourceRequest
	fakeRetag = func(req *orchdv1.RetagSourceRequest) (*orchdv1.RetagSourceResponse, error) {
		got = append(got, req)
		return &orchdv1.RetagSourceResponse{From: []string{"quarantine"}, Chunks: 12, Moved: true}, nil
	}
	cf := factory(&fakeClient{}, nil)
	out, err := run(cf, "", "spaces", "retag", "manual://plan", "acme", "--confirm", "--expect-from", "quarantine")
	if err != nil {
		t.Fatal(err)
	}
	if out != "moved 12 chunk(s) to acme (was in quarantine)\n" {
		t.Fatalf("output = %q", out)
	}
	if len(got) != 1 || got[0].GetSourceUri() != "manual://plan" || got[0].GetSpace() != "acme" || !got[0].GetConfirm() || got[0].GetExpectFrom() != "quarantine" {
		t.Fatalf("requests = %v", got)
	}
	if _, err := run(cf, "", "spaces", "retag", "manual://plan"); err == nil {
		t.Fatal("a retag with no space ran")
	}
	fakeRetag = func(*orchdv1.RetagSourceRequest) (*orchdv1.RetagSourceResponse, error) {
		return nil, errors.New("refused")
	}
	if _, err := run(cf, "", "spaces", "retag", "manual://plan", "acme"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refusal returned %v", err)
	}
	t.Setenv("ORCH_TASK_ID", "t-1")
	if _, err := run(cf, "", "spaces", "retag", "manual://plan", "acme"); err == nil {
		t.Fatal("a queued task ran a retag")
	}
}

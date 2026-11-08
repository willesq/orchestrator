package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// spacesFake is what fakeClient's spaces calls answer and record.
var spacesFake struct {
	draftReq *orchdv1.SetSpacesDraftRequest
	applyReq *orchdv1.ApplySpacesRequest
	draft    *orchdv1.SpacesDraft
	diff     *orchdv1.SpacesDiff
	err      error
}

func resetSpacesFake(t *testing.T) {
	t.Helper()
	clear := func() {
		spacesFake.draftReq, spacesFake.applyReq, spacesFake.draft, spacesFake.diff, spacesFake.err = nil, nil, nil, nil, nil
	}
	clear()
	t.Cleanup(clear)
}

func (f *fakeClient) SetSpacesDraft(_ context.Context, req *orchdv1.SetSpacesDraftRequest, _ ...grpc.CallOption) (*orchdv1.SpacesDraft, error) {
	spacesFake.draftReq = req
	return spacesFake.draft, spacesFake.err
}

func (f *fakeClient) DiffSpaces(context.Context, *orchdv1.DiffSpacesRequest, ...grpc.CallOption) (*orchdv1.SpacesDiff, error) {
	return spacesFake.diff, spacesFake.err
}

func (f *fakeClient) ApplySpaces(_ context.Context, req *orchdv1.ApplySpacesRequest, _ ...grpc.CallOption) (*orchdv1.ApplySpacesResponse, error) {
	spacesFake.applyReq = req
	if spacesFake.err != nil {
		return nil, spacesFake.err
	}
	return &orchdv1.ApplySpacesResponse{AppliedRevision: req.GetReviewedRevision()}, nil
}

func TestSpacesDraft(t *testing.T) {
	resetSpacesFake(t)
	spacesFake.draft = &orchdv1.SpacesDraft{Revision: 3}
	doc := `{"spaces":[]}`
	out, err := run(factory(&fakeClient{}, nil), doc, "spaces", "draft")
	if err != nil || spacesFake.draftReq.GetPolicyJson() != doc {
		t.Fatalf("draft: %q, %v, sent %v", out, err, spacesFake.draftReq)
	}
	for _, want := range []string{"saved draft revision 3", "orch spaces apply --revision 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}

	spacesFake.draft = &orchdv1.SpacesDraft{Revision: 4, Problems: []*orchdv1.SpacesProblem{{Rule: "unknown_kind", Message: "space \"x\" has unknown kind"}}}
	out, err = run(factory(&fakeClient{}, nil), doc, "spaces", "draft")
	if err != nil || !strings.Contains(out, "1 problem(s)") || !strings.Contains(out, "[unknown_kind]") || strings.Contains(out, "apply --revision") {
		t.Fatalf("draft with problems: %q, %v", out, err)
	}

	spacesFake.err = errors.New("bad json")
	if _, err := run(factory(&fakeClient{}, nil), doc, "spaces", "draft"); err == nil {
		t.Fatal("want the RPC error")
	}
	if _, err := run(factory(nil, errors.New("no daemon")), doc, "spaces", "draft"); err == nil {
		t.Fatal("want the dial error")
	}
}

func TestSpacesDiff(t *testing.T) {
	tests := []struct {
		name string
		diff *orchdv1.SpacesDiff
		args []string
		want []string
		not  []string
	}{
		{
			name: "nothing at all",
			diff: &orchdv1.SpacesDiff{},
			want: []string{"applied: none", "no draft newer"},
		},
		{
			name: "a valid draft",
			diff: &orchdv1.SpacesDiff{AppliedRevision: 2, DraftRevision: 5, DraftByOperator: true, Changes: []*orchdv1.SpacesChange{
				{Kind: "changed", Fields: []string{"public_root_sessions"}},
				{Space: "acme", Kind: "changed", Fields: []string{"roots", "reads"}, Details: []string{`roots: +"~/code/acme"`}},
				{Space: "globex", Kind: "added"},
			}},
			want: []string{"applied: revision 2", "draft:   revision 5, saved", "by you", "changed (policy): public_root_sessions",
				"changed acme: roots, reads", `      roots: +"~/code/acme"`, "added   globex", "orch spaces apply --revision 5\n"},
			not: []string{"draft policy:", "agent"},
		},
		{
			name: "an agent's draft",
			diff: &orchdv1.SpacesDiff{DraftRevision: 6, DraftCreatedBy: "process 7 in claude", Changes: []*orchdv1.SpacesChange{
				{Space: "acme", Kind: "changed", Fields: []string{"reads"}, Details: []string{`reads: +"personal"`}},
			}},
			want: []string{"by an agent session (process 7 in claude)", `reads: +"personal"`,
				"orch spaces apply --revision 6 --accept-agent-draft"},
		},
		{
			name: "an invalid draft, in full",
			diff: &orchdv1.SpacesDiff{DraftRevision: 1, DraftByOperator: true, DraftJson: `{"spaces":[1]}`,
				Problems: []*orchdv1.SpacesProblem{{Rule: "bad", Message: "nope"}}},
			args: []string{"--full"},
			want: []string{"no changes", "[bad] nope", "applied policy:\nnone", `{"spaces":[1]}`},
			not:  []string{"apply --revision"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetSpacesFake(t)
			spacesFake.diff = tt.diff
			out, err := run(factory(&fakeClient{}, nil), "", append([]string{"spaces", "diff"}, tt.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in %q", want, out)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(out, not) {
					t.Errorf("unexpected %q in %q", not, out)
				}
			}
		})
	}
	resetSpacesFake(t)
	spacesFake.err = errors.New("down")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "diff"); err == nil {
		t.Fatal("want the RPC error")
	}
}

func TestSpacesApply(t *testing.T) {
	resetSpacesFake(t)
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "apply"); err == nil || !strings.Contains(err.Error(), "--revision") {
		t.Fatalf("apply without a revision: %v", err)
	}
	if spacesFake.applyReq != nil {
		t.Fatal("apply without a revision reached orchd")
	}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "apply", "--revision", "7")
	if err != nil || spacesFake.applyReq.GetReviewedRevision() != 7 || !strings.Contains(out, "applied spaces revision 7") {
		t.Fatalf("apply: %q, %v", out, err)
	}
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "apply", "--revision", "8", "--accept-agent-draft"); err != nil ||
		!spacesFake.applyReq.GetAcceptAgentDraft() {
		t.Fatalf("apply --accept-agent-draft: %v, sent %v", err, spacesFake.applyReq)
	}
	if spacesFake.applyReq.GetReviewedRevision() != 8 {
		t.Fatalf("sent %v", spacesFake.applyReq)
	}
	spacesFake.err = errors.New("only the operator can apply the spaces policy")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "apply", "--revision", "7"); err == nil {
		t.Fatal("want the refusal")
	}
}

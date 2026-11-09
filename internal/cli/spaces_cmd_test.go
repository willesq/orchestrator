package cli

import (
	"context"
	"errors"
	"slices"
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
	whyReq   *orchdv1.ExplainPlacementRequest
	why      *orchdv1.PlacementExplanation
	err      error
}

func resetSpacesFake(t *testing.T) {
	t.Helper()
	clear := func() {
		spacesFake.draftReq, spacesFake.applyReq, spacesFake.draft, spacesFake.diff, spacesFake.err = nil, nil, nil, nil, nil
		spacesFake.whyReq, spacesFake.why = nil, nil
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

func (f *fakeClient) ExplainPlacement(_ context.Context, req *orchdv1.ExplainPlacementRequest, _ ...grpc.CallOption) (*orchdv1.PlacementExplanation, error) {
	spacesFake.whyReq = req
	return spacesFake.why, spacesFake.err
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
			name: "unconfirmed GitHub orgs",
			diff: &orchdv1.SpacesDiff{AppliedRevision: 3, UnconfirmedGithubOrgs: []string{"acme", "globex\x1b[2J"}},
			want: []string{"not confirmed: GitHub org(s) acme, globex [2J", "--confirm-github-org <org>", "no draft newer"},
			not:  []string{"\x1b"},
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
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "apply", "--revision", "9",
		"--confirm-github-org", "acme", "--confirm-github-org", "globex"); err != nil ||
		!slices.Equal(spacesFake.applyReq.GetConfirmGithubOrgs(), []string{"acme", "globex"}) {
		t.Fatalf("apply --confirm-github-org: %v, sent %v", err, spacesFake.applyReq)
	}
	spacesFake.err = errors.New("only the operator can apply the spaces policy")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "apply", "--revision", "7"); err == nil {
		t.Fatal("want the refusal")
	}
}

func TestSpacesWhy(t *testing.T) {
	resetSpacesFake(t)
	spacesFake.why = &orchdv1.PlacementExplanation{
		TargetKind: "path",
		Roots: []*orchdv1.PlacementRoot{
			{Root: "/home/u/code", Space: "oss", Kind: "public"},
			{Root: "/home/u/code/company1", Space: "acme", Kind: "isolated"},
		},
		Remote:       &orchdv1.PlacementRemote{Owner: "acme", Repo: "app", Visibility: "private", Space: "acme", Kind: "isolated", Effect: "owner bound to acme (isolated)"},
		Rule:         "root_and_remote_agree",
		Reason:       "its root and its remote's owner are both bound to acme",
		Outcome:      "placed",
		Space:        "acme",
		SessionSpace: "acme",
		SessionReads: "acme",
	}
	out, err := run(factory(&fakeClient{}, nil), "", "spaces", "why", "/home/u/code/company1/app")
	if err != nil || spacesFake.whyReq.GetTarget() != "/home/u/code/company1/app" {
		t.Fatalf("why: %q, %v, sent %v", out, err, spacesFake.whyReq)
	}
	want := []string{
		"target: /home/u/code/company1/app (path)",
		"roots, shallowest first:\n  /home/u/code  oss (public)\n  /home/u/code/company1  acme (isolated)  <- deepest, counts",
		"remote: acme/app (private): owner bound to acme (isolated)",
		"rule: root_and_remote_agree: its root and its remote's owner are both bound to acme",
		"result: acme (placed)",
		"a session started here: content goes to acme; reads as acme",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}

	spacesFake.why = &orchdv1.PlacementExplanation{TargetKind: "path", Rule: "public_root_session", Outcome: "quarantined", Space: "quarantine", SessionSpace: "quarantine"}
	out, err = run(factory(&fakeClient{}, nil), "", "spaces", "why", "~/x")
	if err != nil || !strings.HasSuffix(spacesFake.whyReq.GetTarget(), "/x") || !strings.HasPrefix(spacesFake.whyReq.GetTarget(), "/") {
		t.Fatalf("~ not expanded: %v, %v", spacesFake.whyReq, err)
	}
	for _, w := range []string{"roots: none hold it", "remote: none", "reads as no space (public only)"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}

	spacesFake.why = &orchdv1.PlacementExplanation{TargetKind: "repo", Remote: &orchdv1.PlacementRemote{Owner: "oss-org", Repo: "lib", Effect: "owner bound to oss"}, Space: "quarantine"}
	if out, _ = run(factory(&fakeClient{}, nil), "", "spaces", "why", "./sub"); !strings.HasPrefix(spacesFake.whyReq.GetTarget(), "/") {
		t.Fatalf("relative path sent as %q", spacesFake.whyReq.GetTarget())
	}
	if !strings.Contains(out, "visibility not checked") || strings.Contains(out, "roots:") {
		t.Errorf("repo output: %s", out)
	}
	for _, target := range []string{"acme/app", "https://docs.oss.dev/a", "slack:T1"} {
		if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "why", target); err != nil || spacesFake.whyReq.GetTarget() != target {
			t.Errorf("%s sent as %q, %v", target, spacesFake.whyReq.GetTarget(), err)
		}
	}

	spacesFake.err = errors.New("permission denied")
	if _, err := run(factory(&fakeClient{}, nil), "", "spaces", "why", "acme/app"); err == nil {
		t.Error("orchd's refusal was not returned")
	}
}

func (f *fakeClient) PinSession(context.Context, *orchdv1.PinSessionRequest, ...grpc.CallOption) (*orchdv1.SessionPin, error) {
	return &orchdv1.SessionPin{}, nil
}

func (f *fakeClient) EndSession(context.Context, *orchdv1.EndSessionRequest, ...grpc.CallOption) (*orchdv1.EndSessionResponse, error) {
	return &orchdv1.EndSessionResponse{}, nil
}

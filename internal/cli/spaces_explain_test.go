package cli

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSpacesExplainNamesPublicPath is SP-076.1.a4 in the CLI: 'orch spaces
// explain' (an alias of why) prints the rule and reason orchd gives, which
// name the public_paths pattern that made a path public or say none matched.
func TestSpacesExplainNamesPublicPath(t *testing.T) {
	resetSpacesFake(t)
	for _, c := range []struct {
		why  *orchdv1.PlacementExplanation
		want string
	}{
		{&orchdv1.PlacementExplanation{TargetKind: "path", Rule: "public_path", Outcome: "placed", Space: "oss",
			Reason: `a public_paths pattern of public space oss matches it (public_paths pattern "~/code/oss/**")`},
			`rule: public_path: a public_paths pattern of public space oss matches it (public_paths pattern "~/code/oss/**")`},
		{&orchdv1.PlacementExplanation{TargetKind: "path", Rule: "no_public_proof", Outcome: "quarantined", Space: "quarantine",
			Reason: "no public_paths pattern of public space oss matches it"},
			"rule: no_public_proof: no public_paths pattern of public space oss matches it"},
	} {
		spacesFake.why = c.why
		out, err := run(factory(&fakeClient{}, nil), "", "spaces", "explain", "/home/u/code/oss/a.md")
		if err != nil || spacesFake.whyReq.GetTarget() != "/home/u/code/oss/a.md" {
			t.Fatalf("explain: %q, %v, sent %v", out, err, spacesFake.whyReq)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("missing %q in:\n%s", c.want, out)
		}
	}
}

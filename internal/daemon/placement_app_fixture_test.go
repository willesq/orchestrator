package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// appPlacementFixture is what the macOS app's directory validator test
// (PlacementValidatorTests, SP-072.2.a1) replays: ExplainPlacement's answers
// for the nested-root fixture, with the fixture's folder written as $CODE.
const appPlacementFixture = "../../macos/Tests/Fixtures/placement-explanations.json"

// appPlacementPaths covers every way a path is placed: each nested root,
// a root's own folder, a path under no root, a remote stricter than its
// root, a root stricter than its remote, a conflict and quarantine.
var appPlacementPaths = []string{
	"$CODE",
	"$CODE/tools",
	"$CODE/willesq/deep/x",
	"$CODE/company1/svc/x",
	"$CODE/company1/app/main.go",
	"$CODE/willesq/app",
	"$CODE/company1/lib",
	"$CODE/company1/x",
	"$CODE/lib",
	"$CODE/secret",
	"$CODE/moved",
	"/nonexistent-root/x",
}

type appPlacementCase struct {
	Path        string          `json:"path"`
	Explanation json.RawMessage `json:"explanation"`
}

// TestAppPlacementFixtureMatchesExplainPlacement keeps the app's fixture
// equal to what ExplainPlacement answers today, so the app's test compares
// its validator with the daemon and not with a stale copy. Run with
// ORCH_UPDATE_APP_FIXTURE=1 to rewrite it after a deliberate change.
func TestAppPlacementFixtureMatchesExplainPlacement(t *testing.T) {
	s, code, _ := placementServer(t)
	var live []appPlacementCase
	for _, p := range appPlacementPaths {
		e, err := s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: strings.ReplaceAll(p, "$CODE", code)})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		raw, err := protojson.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		// protojson's spacing varies between runs; a round trip through
		// encoding/json makes the file stable.
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		norm, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, appPlacementCase{Path: p, Explanation: []byte(strings.ReplaceAll(string(norm), code, "$CODE"))})
	}
	if os.Getenv("ORCH_UPDATE_APP_FIXTURE") != "" {
		out, err := json.MarshalIndent(live, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.FromSlash(appPlacementFixture), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.FromSlash(appPlacementFixture))
	if err != nil {
		t.Fatalf("%v (run with ORCH_UPDATE_APP_FIXTURE=1 to write it)", err)
	}
	var saved []appPlacementCase
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != len(live) {
		t.Fatalf("fixture has %d paths, ExplainPlacement answered %d", len(saved), len(live))
	}
	for i, want := range live {
		got := saved[i]
		if got.Path != want.Path {
			t.Errorf("case %d: fixture path %q, want %q", i, got.Path, want.Path)
			continue
		}
		var a, b orchdv1.PlacementExplanation
		if err := protojson.Unmarshal(bytes.TrimSpace(got.Explanation), &a); err != nil {
			t.Fatalf("%s: %v", got.Path, err)
		}
		if err := protojson.Unmarshal(want.Explanation, &b); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(&a, &b) {
			t.Errorf("%s: fixture %v, ExplainPlacement %v (rerun with ORCH_UPDATE_APP_FIXTURE=1)", got.Path, &a, &b)
		}
	}
}

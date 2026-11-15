package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// termsServer applies acme (isolated, with a terms file in a temporary
// folder that does not exist yet) and oss (public, none).
func termsServer(t *testing.T) (*server, string) {
	t.Helper()
	s := spacesServerWith(t, orgNodes{})
	s.home = t.TempDir()
	path := filepath.Join(s.home, "terms", "acme.terms")
	doc, err := json.Marshal(spaces.Policy{Spaces: []spaces.Space{
		{Name: "acme", Kind: spaces.Isolated, TermsFile: path},
		{Name: "oss", Kind: spaces.Public},
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := draft(t, s, string(doc))
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestSpaceTermsAreWrittenAndDescribedWithoutTerms(t *testing.T) {
	s, path := termsServer(t)
	list, err := s.GetSpaceTerms(operatorCtx, &orchdv1.GetSpaceTermsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetSpaces()) != 2 || list.GetSpaces()[0].GetSpace() != "acme" || list.GetSpaces()[0].GetExists() ||
		list.GetSpaces()[0].GetTermsFile() != path || list.GetSpaces()[1].GetTermsFile() != "" {
		t.Fatalf("before = %v", list)
	}

	const terms = "ProjectNightjar\nre:acme-[0-9]+\n"
	got, err := s.SetSpaceTerms(operatorCtx, &orchdv1.SetSpaceTermsRequest{Space: "acme", Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCount() != 2 || !got.GetExists() || len(got.GetFingerprint()) != 12 {
		t.Fatalf("set = %v", got)
	}
	if b, _ := os.ReadFile(path); string(b) != terms {
		t.Fatalf("file = %q", b)
	}
	list, err = s.GetSpaceTerms(operatorCtx, &orchdv1.GetSpaceTermsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a := list.GetSpaces()[0]; a.GetCount() != 2 || a.GetFingerprint() != got.GetFingerprint() || a.GetProblem() != "" {
		t.Fatalf("after = %v", a)
	}
	// No answer carries a term (INV-07).
	for _, raw := range []string{protojson.Format(got), protojson.Format(list)} {
		if strings.Contains(raw, "Nightjar") || strings.Contains(raw, "acme-[0-9]") {
			t.Fatalf("an answer names a term: %s", raw)
		}
	}
}

func TestSetSpaceTermsRefusals(t *testing.T) {
	s, path := termsServer(t)
	session := callerCtx(callerAuth{pid: 7, harness: "claude"})
	if _, err := s.SetSpaceTerms(session, &orchdv1.SetSpaceTermsRequest{Space: "acme", Terms: "x\n"}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("from a session: %v", err)
	}
	if _, err := s.GetSpaceTerms(session, &orchdv1.GetSpaceTermsRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("read from a session: %v", err)
	}
	for name, c := range map[string]struct {
		req  *orchdv1.SetSpaceTermsRequest
		code codes.Code
	}{
		"unknown space": {&orchdv1.SetSpaceTermsRequest{Space: "nope", Terms: "x\n"}, codes.NotFound},
		"no terms file": {&orchdv1.SetSpaceTermsRequest{Space: "oss", Terms: "x\n"}, codes.FailedPrecondition},
		"bad term":      {&orchdv1.SetSpaceTermsRequest{Space: "acme", Terms: "re:Hidden(\n"}, codes.InvalidArgument},
	} {
		_, err := s.SetSpaceTerms(operatorCtx, c.req)
		if grpcstatus.Code(err) != c.code {
			t.Errorf("%s: %v, want %v", name, err, c.code)
		}
		if err != nil && strings.Contains(err.Error(), "Hidden") {
			t.Errorf("%s: the refusal names the term: %v", name, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused write created the file: %v", err)
	}
	if _, err := (&server{}).GetSpaceTerms(operatorCtx, &orchdv1.GetSpaceTermsRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("without spaces: %v", err)
	}
}

func TestGetSpaceTermsWithoutAPolicy(t *testing.T) {
	s := spacesServerWith(t, orgNodes{})
	list, err := s.GetSpaceTerms(operatorCtx, &orchdv1.GetSpaceTermsRequest{})
	if err != nil || len(list.GetSpaces()) != 0 {
		t.Fatalf("= %v, %v", list, err)
	}
	if _, err := s.SetSpaceTerms(operatorCtx, &orchdv1.SetSpaceTermsRequest{Space: "acme", Terms: "x\n"}); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("set = %v", err)
	}
}

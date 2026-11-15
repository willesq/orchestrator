package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// parityFake answers the RPCs behind the commands SP-055 added and records
// their requests; every other RPC is fakeClient's.
type parityFake struct {
	*fakeClient
	list     *orchdv1.ListSpacesResponse
	scan     *orchdv1.ScanSpacesResponse
	scanReq  *orchdv1.ScanSpacesRequest
	terms    *orchdv1.SpaceTermsList
	setTerms *orchdv1.SetSpaceTermsRequest
	config   *orchdv1.ConfigState
	setCfg   *orchdv1.SetConfigRequest
	err      error
}

func (f *parityFake) ListSpaces(context.Context, *orchdv1.ListSpacesRequest, ...grpc.CallOption) (*orchdv1.ListSpacesResponse, error) {
	return f.list, f.err
}

func (f *parityFake) ScanSpaces(_ context.Context, req *orchdv1.ScanSpacesRequest, _ ...grpc.CallOption) (*orchdv1.ScanSpacesResponse, error) {
	f.scanReq = req
	return f.scan, f.err
}

func (f *parityFake) GetSpaceTerms(context.Context, *orchdv1.GetSpaceTermsRequest, ...grpc.CallOption) (*orchdv1.SpaceTermsList, error) {
	return f.terms, f.err
}

func (f *parityFake) SetSpaceTerms(_ context.Context, req *orchdv1.SetSpaceTermsRequest, _ ...grpc.CallOption) (*orchdv1.SpaceTerms, error) {
	f.setTerms = req
	if f.err != nil {
		return nil, f.err
	}
	return &orchdv1.SpaceTerms{Space: req.GetSpace(), Exists: true, Count: 2, Fingerprint: "ab12"}, nil
}

func (f *parityFake) GetConfig(context.Context, *orchdv1.GetConfigRequest, ...grpc.CallOption) (*orchdv1.ConfigState, error) {
	return f.config, f.err
}

func (f *parityFake) SetConfig(_ context.Context, req *orchdv1.SetConfigRequest, _ ...grpc.CallOption) (*orchdv1.ConfigState, error) {
	f.setCfg = req
	return &orchdv1.ConfigState{}, f.err
}

func newParityFake() *parityFake { return &parityFake{fakeClient: &fakeClient{}} }

func wantAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// TestSpacesList prints the Overview's spaces, bindings and logins, and
// keeps a control character from the policy off the terminal.
func TestSpacesList(t *testing.T) {
	f := newParityFake()
	f.list = &orchdv1.ListSpacesResponse{
		Status: &orchdv1.SpacesStatus{Configured: true, AppliedRevision: 3, DraftRevision: 4, Drift: []string{"draft"}},
		Spaces: []*orchdv1.SpaceInfo{
			{Name: "acme", Kind: "isolated", GithubOrgs: []string{"acme"}, Roots: []string{"~/code/acme\x1b[2J"},
				WebPrefixes: []string{"https://acme.example/"}, Chat: []string{"slack:T1"}, Readable: []string{"acme", "public"},
				TermsFile: "~/.config/orch/acme.terms"},
			{Name: "public", Kind: "public"},
		},
		PublicRootSessions: "public",
		Logins: []*orchdv1.SpaceLogin{{Space: "acme",
			Gh:     &orchdv1.LoginState{Own: true, Dir: "~/.config/gh-acme", State: "logged in", Account: "me"},
			Claude: &orchdv1.LoginState{State: "not logged in", Detail: "run claude auth login"}}},
	}
	out, err := run(factory(f, nil), "", "spaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, out, "applied: revision 3; draft revision 4 is not applied", "drift: draft", "acme (isolated)",
		"github orgs:  acme", "roots:        ~/code/acme [2J", "web prefixes: https://acme.example/", "chat:         slack:T1",
		"reads:        acme, public", "terms file:   ~/.config/orch/acme.terms",
		"gh login:     its own (~/.config/gh-acme): logged in as me",
		"claude:       the default: not logged in (run claude auth login)", "public (public)",
		"sessions pinned under a public root write to public")
	if strings.Contains(out, "\x1b") {
		t.Error("a control character reached the terminal")
	}
	f.list = &orchdv1.ListSpacesResponse{Status: &orchdv1.SpacesStatus{Error: "the applied policy cannot be read"}}
	out, err = run(factory(f, nil), "", "spaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, out, "warning: the applied policy cannot be read", "no spaces policy is applied")
	f.err = errors.New("refused: only the operator")
	if _, err := run(factory(f, nil), "", "spaces", "list"); err == nil || !strings.Contains(err.Error(), "only the operator") {
		t.Errorf("err = %v, want orchd's refusal", err)
	}
	if got := loginLine(nil); got != "unknown" {
		t.Errorf("loginLine(nil) = %q", got)
	}
}

// TestSpacesScan sends absolute folders and prints what the scan found.
func TestSpacesScan(t *testing.T) {
	f := newParityFake()
	f.scan = &orchdv1.ScanSpacesResponse{
		Checkouts: []*orchdv1.ScannedCheckout{
			{Path: "/w/app", Owner: "acme", Repo: "app", Visibility: "private", Space: "acme", Rule: "github_org", Outcome: "placed", Reason: "org acme"},
			{Path: "/w/odd", Anonymous: true, Outcome: "quarantined"},
			{Path: "/w/local"},
		},
		Accounts:  []*orchdv1.ScannedAccount{{Login: "me", Orgs: []string{"acme", "oss"}}, {Profile: "acme", Error: "gh failed"}, {Profile: "x"}},
		Problems:  []string{"/w/locked: permission denied"},
		Truncated: true,
	}
	out, err := run(factory(f, nil), "", "spaces", "scan", "/w", "./rel", "--offline", "--depth", "4")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.scanReq.GetFolders(); len(got) != 2 || got[0] != "/w" || !strings.HasPrefix(got[1], "/") || !strings.HasSuffix(got[1], "/rel") {
		t.Errorf("folders = %v, want absolute", got)
	}
	if !f.scanReq.GetOffline() || f.scanReq.GetMaxDepth() != 4 {
		t.Errorf("request = %v", f.scanReq)
	}
	wantAll(t, out, "3 checkout(s)", "/w/app  acme/app (private)", "placed in acme by github_org: org acme",
		"/w/odd  remote not believed", "    quarantined", "/w/local  no GitHub remote", "default gh login: me, orgs acme, oss",
		"gh login of space acme: could not be read: gh failed", "gh login of space x: not logged in",
		"could not read: /w/locked: permission denied", "the scan stopped at a limit")
	if _, err := run(factory(f, nil), "", "spaces", "scan", "/w", "--depth", "7"); err == nil {
		t.Error("--depth 7 was accepted")
	}
	if _, err := run(factory(f, nil), "", "spaces", "scan"); err == nil {
		t.Error("a scan with no folder was accepted")
	}
	f.err = errors.New("a scan is already running")
	if _, err := run(factory(f, nil), "", "spaces", "scan", "/w"); err == nil {
		t.Error("orchd's error was lost")
	}
}

// TestSpacesTermsFiles lists the terms files without a term and replaces
// one space's terms from input, never from an argument.
func TestSpacesTermsFiles(t *testing.T) {
	f := newParityFake()
	f.terms = &orchdv1.SpaceTermsList{Spaces: []*orchdv1.SpaceTerms{
		{Space: "acme", TermsFile: "~/t/acme.txt", Exists: true, Count: 12, Fingerprint: "f00d"},
		{Space: "beta", TermsFile: "~/t/beta.txt"},
		{Space: "gamma", TermsFile: "~/t/g.txt", Problem: "the file is a symbolic link"},
		{Space: "public"},
	}}
	out, err := run(factory(f, nil), "", "spaces", "terms", "list")
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, out, "acme  ~/t/acme.txt\n  12 term(s), fingerprint f00d", "beta  ~/t/beta.txt\n  file missing",
		"the file is a symbolic link", "public\n  no terms file named in the policy")
	f.terms = &orchdv1.SpaceTermsList{}
	out, _ = run(factory(f, nil), "", "spaces", "terms", "list")
	wantAll(t, out, "no applied space")

	out, err = run(factory(f, nil), "secret-one\nsecret-two\n", "spaces", "terms", "set", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if f.setTerms.GetSpace() != "acme" || f.setTerms.GetTerms() != "secret-one\nsecret-two\n" {
		t.Errorf("request = %v", f.setTerms)
	}
	wantAll(t, out, "replaced the terms of acme: 2 term(s), fingerprint ab12")
	if strings.Contains(out, "secret") {
		t.Errorf("a term was printed:\n%s", out)
	}
	if _, err := run(factory(f, nil), "", "spaces", "terms", "set", "acme"); err == nil || !strings.Contains(err.Error(), "no terms on input") {
		t.Errorf("empty input: err = %v", err)
	}
	f.err = errors.New("only Orch.app can change a space's terms")
	if _, err := run(factory(f, nil), "x\n", "spaces", "terms", "set", "acme"); err == nil || !strings.Contains(err.Error(), "Orch.app") {
		t.Errorf("err = %v, want orchd's D-78 refusal", err)
	}
	if _, err := run(factory(f, nil), "", "spaces", "terms", "list"); err == nil {
		t.Error("orchd's error was lost")
	}
}

// TestSpacesLogRetention shows the setting and sets it as setup does.
func TestSpacesLogRetention(t *testing.T) {
	f := newParityFake()
	f.config = &orchdv1.ConfigState{Fields: []*orchdv1.ConfigField{
		{Key: "other", Value: `"x"`},
		{Key: retentionKey, Value: `"30d"`, Options: []string{"3d", "7d", "30d"}},
	}}
	out, err := run(factory(f, nil), "", "spaces", "log", "retention")
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, out, "kept 30d (choices: 3d, 7d, 30d)")
	f.config = &orchdv1.ConfigState{Fields: []*orchdv1.ConfigField{{Key: retentionKey, Value: "bare"}}}
	out, _ = run(factory(f, nil), "", "spaces", "log", "retention")
	wantAll(t, out, "kept bare")
	f.config = &orchdv1.ConfigState{}
	if _, err := run(factory(f, nil), "", "spaces", "log", "retention"); err == nil {
		t.Error("a missing setting was not reported")
	}

	out, err = run(factory(f, nil), "", "spaces", "log", "retention", "3d")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.setCfg.GetValues()[retentionKey]; got != `"3d"` || !f.setCfg.GetConfirmSensitive() {
		t.Errorf("request = %v", f.setCfg)
	}
	wantAll(t, out, "now kept 3d")
	f.err = errors.New("only Orch.app may change orch's sensitive settings")
	if _, err := run(factory(f, nil), "", "spaces", "log", "retention", "3d"); err == nil {
		t.Error("orchd's refusal was lost")
	}
	if _, err := run(factory(f, nil), "", "spaces", "log", "retention"); err == nil {
		t.Error("orchd's error was lost")
	}
}

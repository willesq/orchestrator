package spaces

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// orgFake stands in for gh: it answers each login with GitHub's spelling
// (lowercase here) and a node ID, by default "N_<login>".
type orgFake struct {
	mu    sync.Mutex
	nodes map[string]string
	fail  error
	calls int
	// profiles is the gh profile each login was last looked up with.
	profiles map[string]GHProfile
}

func fakeOrgs(nodes map[string]string) *orgFake {
	if nodes == nil {
		nodes = map[string]string{}
	}
	return &orgFake{nodes: nodes}
}

func (f *orgFake) lookup(_ context.Context, login string, prof GHProfile) (OrgIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.profiles == nil {
		f.profiles = map[string]GHProfile{}
	}
	f.profiles[strings.ToLower(login)] = prof
	if f.fail != nil {
		return OrgIdentity{}, f.fail
	}
	k := strings.ToLower(login)
	n, ok := f.nodes[k]
	if !ok {
		n = "N_" + k
	}
	return OrgIdentity{Login: k, NodeID: n}, nil
}

func (f *orgFake) set(login, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[login] = node
}

// orgEngine opens an engine whose GitHub lookups go to f.
func orgEngine(t *testing.T, conn *sql.DB, f *orgFake) *Engine {
	t.Helper()
	env := testEnv()
	env.LookupOrg = f.lookup
	e, err := Open(context.Background(), conn, env)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type bindingRow struct {
	login, nodeID string
	revision      int64
	suspended     bool
	seen          string
}

func bindings(t *testing.T, conn *sql.DB) map[string]bindingRow {
	t.Helper()
	rows, err := conn.Query(`SELECT org, login, node_id, revision, suspended_at IS NOT NULL, COALESCE(seen_node_id, '') FROM spaces_org_bindings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bindingRow{}
	for rows.Next() {
		var k string
		var b bindingRow
		if err := rows.Scan(&k, &b.login, &b.nodeID, &b.revision, &b.suspended, &b.seen); err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

var orgPolicy = Policy{Spaces: []Space{
	{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"Acme-Corp"}},
	{Name: "oss", Kind: Public, GitHubOrgs: []string{"oss-org"}},
}}

// TestApplyBindsOrgIdentity is SP-021.1.a1: apply looks up each bound org
// with gh and stores its login and node ID; logins match case-insensitively.
func TestApplyBindsOrgIdentity(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	f := fakeOrgs(map[string]string{"acme-corp": "O_acme"})
	e := orgEngine(t, conn, f)
	r := saveAndApply(t, e, orgPolicy)

	got := bindings(t, conn)
	want := map[string]bindingRow{
		"acme-corp": {login: "acme-corp", nodeID: "O_acme", revision: r.Number},
		"oss-org":   {login: "oss-org", nodeID: "N_oss-org", revision: r.Number},
	}
	if len(got) != len(want) || got["acme-corp"] != want["acme-corp"] || got["oss-org"] != want["oss-org"] {
		t.Fatalf("bindings = %+v, want %+v", got, want)
	}
	for _, login := range []string{"acme-corp", "ACME-CORP", "Acme-Corp"} {
		if space, err := e.OrgSpace(ctx, login); err != nil || space != "acme" {
			t.Errorf("OrgSpace(%q) = %q, %v; want acme", login, space, err)
		}
		if space, err := e.CheckOrg(ctx, login); err != nil || space != "acme" {
			t.Errorf("CheckOrg(%q) = %q, %v; want acme", login, space, err)
		}
	}
	if space, err := e.CheckOrg(ctx, "globex"); err != nil || space != "" {
		t.Errorf("CheckOrg(unbound) = %q, %v; want none", space, err)
	}
	if st := status(t, e); st.Error != "" {
		t.Errorf("status error = %q", st.Error)
	}

	// Applying again keeps the bindings and asks GitHub again.
	before := f.calls
	saveAndApply(t, e, Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"acme-corp"}}}})
	if f.calls != before+1 {
		t.Errorf("re-apply made %d lookups, want 1", f.calls-before)
	}
	if got := bindings(t, conn); got["acme-corp"].nodeID != "O_acme" || got["oss-org"].revision != r.Number {
		t.Errorf("after re-apply bindings = %+v (a dropped org's row is kept)", got)
	}
}

// TestDroppedOrgReaddedNeedsConfirm: dropping an org and adding it back
// cannot bind a different account without the operator confirming it.
func TestDroppedOrgReaddedNeedsConfirm(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	f := fakeOrgs(nil)
	e := orgEngine(t, conn, f)
	saveAndApply(t, e, orgPolicy)
	saveAndApply(t, e, Policy{Spaces: []Space{{Name: "acme", Kind: Isolated, GitHubOrgs: []string{"acme-corp"}}}})
	f.set("oss-org", "O_impostor")
	r, _, err := e.SaveDraft(ctx, orgPolicy, "op", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, r.Number, "op", false, nil); !errors.Is(err, ErrOrgChanged) {
		t.Fatalf("re-adding a changed org = %v, want ErrOrgChanged", err)
	}
	if _, err := e.Apply(ctx, r.Number, "op", false, []string{"oss-org"}); err != nil {
		t.Fatalf("confirmed = %v", err)
	}
	if b := bindings(t, conn)["oss-org"]; b.nodeID != "O_impostor" || b.revision != r.Number {
		t.Fatalf("binding = %+v", b)
	}
}

// TestApplyRefusesUnconfirmedOrgs: an org that cannot be looked up is never
// bound, and the enforced policy stays as it was.
func TestApplyRefusesUnconfirmedOrgs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		lookup func(context.Context, string, GHProfile) (OrgIdentity, error)
	}{
		{"gh fails", func(context.Context, string, GHProfile) (OrgIdentity, error) {
			return OrgIdentity{}, errors.New("HTTP 404")
		}},
		{"other account", func(context.Context, string, GHProfile) (OrgIdentity, error) {
			return OrgIdentity{Login: "someone-else", NodeID: "O_x"}, nil
		}},
		{"no node id", func(_ context.Context, l string, _ GHProfile) (OrgIdentity, error) { return OrgIdentity{Login: l}, nil }},
		{"no lookup", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := openStore(t)
			env := testEnv()
			env.LookupOrg = tc.lookup
			e, err := Open(ctx, conn, env)
			if err != nil {
				t.Fatal(err)
			}
			r, _, err := e.SaveDraft(ctx, orgPolicy, "op", true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Apply(ctx, r.Number, "op", false, nil); !errors.Is(err, ErrOrgLookup) {
				t.Fatalf("Apply = %v, want ErrOrgLookup", err)
			}
			if current(t, e) != nil || len(bindings(t, conn)) != 0 {
				t.Fatal("a refused apply changed the enforced policy or the bindings")
			}
		})
	}
}

// TestSyncMismatchSuspendsBinding is SP-021.1.a2: a node ID mismatch at
// sync suspends the binding, quarantines the org's new content and raises a
// status item, until the operator confirms at an apply.
func TestSyncMismatchSuspendsBinding(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	f := fakeOrgs(map[string]string{"acme-corp": "O_acme"})
	e := orgEngine(t, conn, f)
	saveAndApply(t, e, orgPolicy)

	// The login is deleted and registered again by someone else.
	f.set("acme-corp", "O_impostor")
	if space, err := e.CheckOrg(ctx, "acme-corp"); err != nil || space != Quarantine {
		t.Fatalf("CheckOrg after mismatch = %q, %v; want quarantine", space, err)
	}
	b := bindings(t, conn)["acme-corp"]
	if !b.suspended || b.seen != "O_impostor" || b.nodeID != "O_acme" {
		t.Fatalf("binding = %+v, want suspended with the seen node ID", b)
	}
	if space, _ := e.OrgSpace(ctx, "Acme-Corp"); space != Quarantine {
		t.Errorf("OrgSpace = %q, want quarantine", space)
	}
	if space, _ := e.CheckOrg(ctx, "oss-org"); space != "oss" {
		t.Errorf("an unchanged org is affected: %q", space)
	}
	st := status(t, e)
	if !strings.Contains(st.Error, "1 bound GitHub org(s) not confirmed") {
		t.Errorf("status error = %q, want the suspension reported", st.Error)
	}
	if strings.Contains(strings.ToLower(st.Error), "acme") {
		t.Errorf("status names the org: %q", st.Error)
	}
	d, err := e.Diff(ctx)
	if err != nil || !slices.Equal(d.UnconfirmedOrgs, []string{"Acme-Corp"}) {
		t.Errorf("Diff unconfirmed = %v, %v", d.UnconfirmedOrgs, err)
	}

	// It stays suspended, even if GitHub answers with the old node again.
	f.set("acme-corp", "O_acme")
	if space, _ := e.CheckOrg(ctx, "acme-corp"); space != Quarantine {
		t.Errorf("suspension lifted without the operator: %q", space)
	}
	f.set("acme-corp", "O_impostor")

	// An apply made without confirming the org is refused.
	r, _, err := e.SaveDraft(ctx, orgPolicy, "op", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, r.Number, "op", false, []string{"oss-org"}); !errors.Is(err, ErrOrgChanged) {
		t.Fatalf("Apply without confirming = %v, want ErrOrgChanged", err)
	}
	if !bindings(t, conn)["acme-corp"].suspended {
		t.Fatal("a refused apply cleared the suspension")
	}

	// The operator confirms the new identity.
	if _, err := e.Apply(ctx, r.Number, "op", false, []string{"ACME-CORP"}); err != nil {
		t.Fatalf("Apply confirming = %v", err)
	}
	if b := bindings(t, conn)["acme-corp"]; b.suspended || b.nodeID != "O_impostor" {
		t.Fatalf("after confirm binding = %+v", b)
	}
	if space, _ := e.CheckOrg(ctx, "acme-corp"); space != "acme" {
		t.Errorf("CheckOrg after confirm = %q", space)
	}
	if st := status(t, e); st.Error != "" {
		t.Errorf("status after confirm = %q", st.Error)
	}
}

// TestApplyClearsSuspensionWhenIdentityReturns: when GitHub again answers
// with the bound node ID, an apply lifts the suspension without a confirm.
func TestApplyClearsSuspensionWhenIdentityReturns(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	f := fakeOrgs(nil)
	e := orgEngine(t, conn, f)
	r := saveAndApply(t, e, orgPolicy)
	f.set("oss-org", "O_other")
	if space, _ := e.CheckOrg(ctx, "oss-org"); space != Quarantine {
		t.Fatalf("CheckOrg = %q", space)
	}
	f.set("oss-org", "N_oss-org")
	if _, err := e.Apply(ctx, r.Number, "op", false, nil); err != nil {
		t.Fatalf("Apply = %v", err)
	}
	if bindings(t, conn)["oss-org"].suspended {
		t.Fatal("suspension kept")
	}
}

// TestUnboundAppliedOrgIsQuarantined: an org applied with no binding (before
// bindings were recorded) is never trusted until applied again.
func TestUnboundAppliedOrgIsQuarantined(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	e := orgEngine(t, conn, fakeOrgs(nil))
	saveAndApply(t, e, orgPolicy)
	exec(t, conn, `DELETE FROM spaces_org_bindings`)
	for _, check := range []func(context.Context, string) (string, error){e.OrgSpace, e.CheckOrg} {
		if space, err := check(ctx, "acme-corp"); err != nil || space != Quarantine {
			t.Errorf("space = %q, %v; want quarantine", space, err)
		}
	}
	if got, _ := e.UnconfirmedOrgs(ctx); len(got) != 2 {
		t.Errorf("UnconfirmedOrgs = %v", got)
	}
}

func TestCheckOrgErrors(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	f := fakeOrgs(nil)
	e := orgEngine(t, conn, f)

	// No policy: nothing is bound and GitHub is not asked.
	if space, err := e.CheckOrg(ctx, "acme-corp"); err != nil || space != "" || f.calls != 0 {
		t.Fatalf("CheckOrg without policy = %q, %v (%d calls)", space, err, f.calls)
	}
	saveAndApply(t, e, orgPolicy)
	f.fail = errors.New("network down")
	if _, err := e.CheckOrg(ctx, "acme-corp"); !errors.Is(err, ErrOrgLookup) {
		t.Errorf("CheckOrg with gh failing = %v, want ErrOrgLookup", err)
	}
	if bindings(t, conn)["acme-corp"].suspended {
		t.Error("a failed lookup suspended the binding")
	}

	e.broken = "broken"
	if _, err := e.CheckOrg(ctx, "acme-corp"); !errors.Is(err, ErrBroken) {
		t.Errorf("CheckOrg on a broken engine = %v", err)
	}
	if _, err := e.OrgSpace(ctx, "acme-corp"); !errors.Is(err, ErrBroken) {
		t.Errorf("OrgSpace on a broken engine = %v", err)
	}
	e.broken = ""

	_ = conn.Close()
	if _, err := e.OrgSpace(ctx, "acme-corp"); err == nil {
		t.Error("OrgSpace on a closed db succeeded")
	}
	if _, err := e.UnconfirmedOrgs(ctx); err == nil {
		t.Error("UnconfirmedOrgs on a closed db succeeded")
	}
	if _, err := e.Status(ctx); err == nil {
		t.Error("Status on a closed db succeeded")
	}
}

func TestGitHubOrgSpace(t *testing.T) {
	var none *Resolved
	if _, ok := none.GitHubOrgSpace("acme"); ok {
		t.Error("no policy binds an org")
	}
	res, err := Validate(orgPolicy, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := res.GitHubOrgSpace(" OSS-ORG "); !ok || s != "oss" {
		t.Errorf("GitHubOrgSpace = %q, %v", s, ok)
	}
}

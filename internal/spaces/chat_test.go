package spaces

import "testing"

// TestChatTenantInPublicSpaceQuarantined: a chat tenant bound to a public
// space never places its content public (every space reads public, and
// nothing proves a chat public); it is quarantined (SP-060, INV-03).
func TestChatTenantInPublicSpaceQuarantined(t *testing.T) {
	res := resolvePlacement(t, Policy{Spaces: []Space{
		{Name: "oss", Kind: Public, Chat: []string{"slack:TOSS"}},
		{Name: "acme", Kind: Isolated, Chat: []string{"teams:acme-tenant"}},
	}})
	p := Classify(Origin{System: SystemSlack, Tenant: "toss"}, res, Options{})
	if p.Space != Quarantine || p.Outcome != Quarantined || p.Rule != PlaceNotVerifiedPublic {
		t.Fatalf("public-bound tenant placed %+v", p)
	}
	if p := Classify(Origin{System: SystemTeams, Tenant: "ACME-tenant"}, res, Options{}); p.Space != "acme" || p.Rule != PlaceChatTenant {
		t.Fatalf("isolated-bound tenant placed %+v", p)
	}
	// An operator's retag still wins.
	if p := Classify(Origin{System: SystemSlack, Tenant: "TOSS"}, res, Options{Override: "acme"}); p.Space != "acme" {
		t.Fatalf("retag ignored: %+v", p)
	}
}

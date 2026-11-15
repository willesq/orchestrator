package rag

import "testing"

// TestTenantKey (SP-083): a chat origin names its tenant as a policy binds
// it; an origin without a system or a tenant names none.
func TestTenantKey(t *testing.T) {
	for _, c := range []struct {
		o    Origin
		want string
	}{
		{Origin{System: "slack", Tenant: "T0123"}, "slack:T0123"},
		{Origin{System: "teams", Tenant: "globex-tenant", Container: "19:x"}, "teams:globex-tenant"},
		{Origin{System: "slack"}, ""},
		{Origin{Tenant: "T0123"}, ""},
		{Origin{Path: "/repo"}, ""},
	} {
		if got := c.o.TenantKey(); got != c.want {
			t.Errorf("%+v.TenantKey() = %q, want %q", c.o, got, c.want)
		}
	}
	// A stored origin read back gives the same key.
	if got := originFrom(Origin{System: "slack", Tenant: "T9"}.payload()).TenantKey(); got != "slack:T9" {
		t.Errorf("stored origin's key = %q", got)
	}
}

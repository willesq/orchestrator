package feeds

import (
	"slices"
	"testing"
)

// TestAllowlistFor: a tenant's DM switch and containers are found
// regardless of case, merged across keys that differ only in case, sorted
// and without repeats; other tenants' are not.
func TestAllowlistFor(t *testing.T) {
	a := Allowlist{
		DirectMessages: []string{"slack:TACME"},
		Containers: map[string][]string{
			"slack:tacme":  {"C2", "C1"},
			"SLACK:TACME":  {"C1", "C3"},
			"slack:TOTHER": {"C9"},
		},
	}
	dms, cs := a.For("Slack:TAcme")
	if !dms || !slices.Equal(cs, []string{"C1", "C2", "C3"}) {
		t.Errorf("For(TACME) = %v, %v", dms, cs)
	}
	if dms, cs := a.For("slack:TNONE"); dms || cs != nil {
		t.Errorf("For(TNONE) = %v, %v", dms, cs)
	}
	if dms, cs := a.For("teams:TACME"); dms || len(cs) != 0 {
		t.Errorf("For(teams:TACME) = %v, %v", dms, cs)
	}
}

func TestValidID(t *testing.T) {
	for v, want := range map[string]bool{"U123": true, "B0A1": true, "": false, "a/b": false, "a,b": false, "-x": false} {
		if got := ValidID(v); got != want {
			t.Errorf("ValidID(%q) = %v", v, got)
		}
	}
}

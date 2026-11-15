package rag

import (
	"encoding/json"
	"testing"
)

// TestLabelsFilterIsNumeric: a read by policy filters labels_v as the number
// it is stored as, an unparsable revision matches nothing, and only named
// spaces' stores take the filter (INV-08).
func TestLabelsFilterIsNumeric(t *testing.T) {
	for val, want := range map[string]string{"7": `7`, "x": `-1`} {
		f := buildFilter(map[string]string{fieldLabelsV: val}, nil, nil, TimeRange{})
		b, _ := json.Marshal(f)
		if got := string(b); got != `{"must":[{"key":"labels_v","match":{"value":`+want+`}}]}` {
			t.Errorf("labels_v %q filters as %s", val, got)
		}
	}
	stores := withLabels([]spaceStore{{space: "acme"}, {}}, 7)
	if stores[0].must(nil)[fieldLabelsV] != "7" || stores[1].must(nil)[fieldLabelsV] != "" {
		t.Fatalf("withLabels = %+v", stores)
	}
	if withLabels([]spaceStore{{space: "acme"}}, 0)[0].must(nil)[fieldLabelsV] != "" {
		t.Fatal("revision 0 filtered on labels_v")
	}
}

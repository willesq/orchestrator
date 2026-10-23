package routing

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The macOS app's dispatch form offers the same task kinds; this keeps the
// two lists from drifting apart.
func TestAppTaskKindsMatchRouting(t *testing.T) {
	src, err := os.ReadFile("../../macos/Sources/OrchKit/Tasks.swift")
	if err != nil {
		t.Skipf("app source not present: %v", err)
	}
	m := regexp.MustCompile(`(?s)taskKinds = \[([^\]]*)\]`).FindSubmatch(src)
	if m == nil {
		t.Fatal("DispatchDraft.taskKinds not found in Tasks.swift")
	}
	var app []string
	for _, part := range strings.Split(string(m[1]), ",") {
		if kind := strings.Trim(strings.TrimSpace(part), `"`); kind != "" {
			app = append(app, kind)
		}
	}
	if !slices.Equal(app, Kinds) {
		t.Errorf("app task kinds %v differ from routing.Kinds %v", app, Kinds)
	}
}

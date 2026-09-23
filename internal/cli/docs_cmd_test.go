package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDocsCLIFlow(t *testing.T) {
	tests := []struct{ name string }{{name: "add, list, status, sync, refresh, remove"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			f := factory(&fakeClient{}, nil)
			mustRun := func(args ...string) string {
				out, err := run(f, "", args...)
				if err != nil {
					t.Fatalf("%v: %v", args, err)
				}
				return out
			}
			if out := mustRun("docs", "add", "willesq", "--refresh", "6h", "--tag", "x"); !strings.Contains(out, "added docs owner willesq") {
				t.Errorf("add: %q", out)
			}
			if out := mustRun("docs", "list"); !strings.Contains(out, "willesq") {
				t.Errorf("list: %q", out)
			}
			if out := mustRun("docs", "status"); !strings.Contains(out, "willesq") || !strings.Contains(out, "last=never") {
				t.Errorf("status: %q", out)
			}
			if out := mustRun("docs", "sync", "willesq"); !strings.Contains(out, "marked for sync") {
				t.Errorf("sync: %q", out)
			}
			if out := mustRun("docs", "refresh"); !strings.Contains(out, "caches cleared") {
				t.Errorf("refresh: %q", out)
			}
			if out := mustRun("docs", "remove", "willesq"); !strings.Contains(out, "removed docs owner willesq") {
				t.Errorf("remove: %q", out)
			}
			if out := mustRun("docs", "list"); !strings.Contains(out, "no docs owners") {
				t.Errorf("list after remove: %q", out)
			}
			if out := mustRun("docs", "status"); !strings.Contains(out, "no docs owners") {
				t.Errorf("status after remove: %q", out)
			}
		})
	}
}

func TestDocsAddInvalidRefresh(t *testing.T) {
	tests := []struct{ name string }{{name: "bad --refresh errors"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			_, err := run(factory(&fakeClient{}, nil), "", "docs", "add", "willesq", "--refresh", "notaduration")
			if err == nil {
				t.Fatal("expected error for invalid --refresh")
			}
		})
	}
}

func TestHumanAgoAndNextDue(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name        string
		last        int64
		refresh     int64
		wantAgoSub  string
		wantNextSub string
	}{
		{name: "never synced", last: 0, refresh: 3600, wantAgoSub: "never", wantNextSub: "now"},
		{name: "recent, not yet due", last: now - 60, refresh: 3600, wantAgoSub: "ago", wantNextSub: "in "},
		{name: "overdue", last: now - 7200, refresh: 3600, wantAgoSub: "ago", wantNextSub: "now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanAgo(tt.last); !strings.Contains(got, tt.wantAgoSub) {
				t.Errorf("humanAgo(%d) = %q, want contains %q", tt.last, got, tt.wantAgoSub)
			}
			if got := nextDue(tt.last, tt.refresh); !strings.Contains(got, tt.wantNextSub) {
				t.Errorf("nextDue = %q, want contains %q", got, tt.wantNextSub)
			}
		})
	}
}

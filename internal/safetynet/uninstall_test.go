package safetynet

import (
	"os"
	"strings"
	"testing"
)

func TestUnmergeRuleConfigReversesMerge(t *testing.T) {
	tests := []struct {
		name   string
		before string
		remove bool
	}{
		{name: "no file", before: "", remove: true},
		{name: "user sources kept", before: "{\n  \"rules\": [\n    \"mine\"\n  ],\n  \"version\": 1\n}\n"},
		{name: "user wrapper and schema kept", before: "{\n  \"$schema\": \"s\",\n  \"transparent_wrappers\": [\n    \"time\"\n  ],\n  \"version\": 1\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged, _, err := MergeRuleConfig([]byte(tt.before))
			if err != nil {
				t.Fatal(err)
			}
			next, remove, summary, err := UnmergeRuleConfig(merged)
			if err != nil {
				t.Fatal(err)
			}
			if remove != tt.remove {
				t.Fatalf("remove = %v (%s)", remove, summary)
			}
			if !remove && string(next) != tt.before {
				t.Errorf("got %q, want %q", next, tt.before)
			}
			// Again: nothing of orch's is left.
			if !remove {
				again, rm, _, err := UnmergeRuleConfig(next)
				if err != nil || rm || string(again) != string(next) {
					t.Errorf("second unmerge changed the file: %q %v %v", again, rm, err)
				}
			}
		})
	}
	for _, bad := range []string{"{", `{"rules": "x"}`, `{"transparent_wrappers": [1]}`} {
		if _, _, _, err := UnmergeRuleConfig([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if next, rm, _, err := UnmergeRuleConfig(nil); err != nil || rm || next != nil {
		t.Error("missing file not left alone")
	}
}

func TestUninstallCommands(t *testing.T) {
	if cmds := UninstallCommands(State{}); len(cmds) != 0 {
		t.Errorf("clean state planned %v", cmds)
	}
	cmds := UninstallCommands(State{Installed: &Install{ID: PluginID}, MarketplaceKnown: true})
	if len(cmds) != 2 {
		t.Fatalf("cmds = %v", cmds)
	}
	if got := cmds[0].String(); !strings.Contains(got, "plugin uninstall "+PluginID+" --scope user") {
		t.Errorf("first = %s", got)
	}
	if got := cmds[1].String(); got != "claude plugin marketplace remove "+MarketplaceName {
		t.Errorf("second = %s", got)
	}
}

func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	link := dir + "-link"
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	if !SameDir(dir, dir+"/") || !SameDir(link, dir) {
		t.Error("the same directory under another spelling is not the same")
	}
	if SameDir(dir, t.TempDir()) || SameDir(dir, dir+"/missing") {
		t.Error("different directories are the same")
	}
}

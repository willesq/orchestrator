package safetynet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMergeRuleConfig(t *testing.T) {
	tests := []struct {
		name     string
		old      string
		wantErr  string
		rules    []string
		wrappers []string
		keep     map[string]any // other keys that must survive
		noop     bool
	}{
		{name: "missing file creates a config", old: "", rules: []string{"orch-guards"}, wrappers: []string{"rtk"}},
		{name: "whitespace only is treated as empty", old: "  \n", rules: []string{"orch-guards"}, wrappers: []string{"rtk"}},
		{
			name:     "user sources, overrides and wrappers are preserved",
			old:      `{"$schema":"s","version":1,"rules":["willesq-guards","o/r#main/team"],"overrides":{"willesq-guards/x":"off"},"transparent_wrappers":["foo"],"command_tools":["ops_bash"]}`,
			rules:    []string{"willesq-guards", "o/r#main/team", "orch-guards"},
			wrappers: []string{"foo", "rtk"},
			keep:     map[string]any{"$schema": "s", "overrides": map[string]any{"willesq-guards/x": "off"}, "command_tools": []any{"ops_bash"}},
		},
		{
			name: "already merged is a no-op", noop: true,
			old:   `{"version":1,"rules":["orch-guards"],"transparent_wrappers":["rtk"]}`,
			rules: []string{"orch-guards"}, wrappers: []string{"rtk"},
		},
		{name: "missing version is set", old: `{"rules":[]}`, rules: []string{"orch-guards"}, wrappers: []string{"rtk"}},
		{name: "unparseable is refused", old: `{"version":1,`, wantErr: "refusing"},
		{name: "unknown version is refused", old: `{"version":2}`, wantErr: "version 2"},
		{name: "non-number version is refused", old: `{"version":"1"}`, wantErr: "not a number"},
		{name: "rules of another type is refused", old: `{"version":1,"rules":"x"}`, wantErr: "not an array"},
		{name: "non-string source is refused", old: `{"version":1,"rules":[1]}`, wantErr: "not a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, summary, err := MergeRuleConfig([]byte(tt.old))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got["version"] != float64(1) {
				t.Errorf("version = %v", got["version"])
			}
			if s := toStrings(got["rules"]); !slices.Equal(s, tt.rules) {
				t.Errorf("rules = %v, want %v", s, tt.rules)
			}
			if s := toStrings(got["transparent_wrappers"]); !slices.Equal(s, tt.wrappers) {
				t.Errorf("wrappers = %v, want %v", s, tt.wrappers)
			}
			for k, v := range tt.keep {
				a, _ := json.Marshal(got[k])
				b, _ := json.Marshal(v)
				if string(a) != string(b) {
					t.Errorf("%s = %s, want %s", k, a, b)
				}
			}
			if tt.noop != strings.Contains(summary, "already lists") {
				t.Errorf("summary %q, noop %v", summary, tt.noop)
			}
			again, _, err := MergeRuleConfig(out)
			if err != nil || string(again) != string(out) {
				t.Errorf("second merge changed the file: %v\n%s", err, again)
			}
		})
	}
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := []string{}
	for _, x := range arr {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRules(t *testing.T) {
	t.Run("nothing installed", func(t *testing.T) {
		loc := Locate(t.TempDir())
		st := DetectRules(loc)
		if st.OK() || st.ConfigExists || len(st.Problems()) != 3 {
			t.Errorf("state %+v problems %v", st, st.Problems())
		}
	})
	t.Run("installed with a user rulebook and a broken one", func(t *testing.T) {
		loc := Locate(t.TempDir())
		writeFile(t, loc.RulebookPath(RulebookName), string(Rulebook()))
		writeFile(t, loc.RulebookPath("mine"), `{"rulebook_version":1,"name":"mine","version":"2.0.0","description":"d",
			"allowed_commands":["git"],"rules":[{"name":"a","command":"git","subcommand":"worktree","block_args":["add"],"reason":"r"},
			{"name":"b","command":"git","block_args":["x"],"reason":"r"}]}`)
		writeFile(t, loc.RulebookPath("v2"), `{"rulebook_version":2,"name":"v2","version":"1",
			"allowed_commands":["aws"],"rules":[{"name":"c","command":"aws","match":{"command_path":["s3","rm"],"any_args":["--recursive"]},"reason":"r"}]}`)
		writeFile(t, loc.RulebookPath("wrongname"), `{"rulebook_version":1,"name":"other","version":"1","rules":[]}`)
		writeFile(t, loc.RulebookPath("badver"), `{"rulebook_version":9,"name":"badver","version":"1","rules":[]}`)
		writeFile(t, loc.RulebookPath("notjson"), `{`)
		writeFile(t, loc.RuleConfigPath(), `{"version":1,"rules":["mine","orch-guards","v2","o/r#feat/x/team","wrongname","badver","notjson","../escape","o/r#x/.."],
			"overrides":{"mine/b":"off"},"transparent_wrappers":["rtk"]}`)
		st := DetectRules(loc)
		if !st.OK() || st.ConfigError != "" {
			t.Fatalf("not ok: %+v", st)
		}
		byName := map[string]RulebookInfo{}
		for _, rb := range st.Rulebooks {
			byName[rb.Source] = rb
		}
		if rb := byName["orch-guards"]; rb.Owner != "orch" || rb.Error != "" || len(rb.Rules) < 10 {
			t.Errorf("orch-guards %+v", rb)
		}
		mine := byName["mine"]
		if mine.Owner != "user" || mine.Version != "2.0.0" || len(mine.Rules) != 2 || mine.Rules[0].Off || !mine.Rules[1].Off {
			t.Errorf("mine %+v", mine)
		}
		if v2 := byName["v2"]; len(v2.Rules) != 1 || v2.Rules[0].Subcommand != "s3 rm" || !slices.Equal(v2.Rules[0].BlockArgs, []string{"--recursive"}) {
			t.Errorf("v2 %+v", v2)
		}
		if r := byName["o/r#feat/x/team"]; !r.Remote || r.Name != "team" || r.Error != "rulebook file missing" {
			t.Errorf("remote %+v", r)
		}
		for src, want := range map[string]string{"wrongname": "does not match", "badver": "rulebook_version 9", "notjson": "invalid JSON", "../escape": "rulebook file missing", "o/r#x/..": "invalid rulebook name"} {
			if !strings.Contains(byName[src].Error, want) {
				t.Errorf("%s error %q, want %q", src, byName[src].Error, want)
			}
		}
	})
	t.Run("a remote source named orch-guards is not orch's", func(t *testing.T) {
		loc := Locate(t.TempDir())
		writeFile(t, loc.RulebookPath(RulebookName), string(Rulebook()))
		writeFile(t, loc.RuleConfigPath(), `{"version":1,"rules":["evil/repo#main/orch-guards"],"transparent_wrappers":["rtk"]}`)
		st := DetectRules(loc)
		if st.Referenced || st.OK() || st.Rulebooks[0].Owner != "user" {
			t.Errorf("state %+v", st)
		}
	})
	t.Run("stale rulebook and missing wrapper", func(t *testing.T) {
		loc := Locate(t.TempDir())
		writeFile(t, loc.RulebookPath(RulebookName), `{"rulebook_version":1,"name":"orch-guards","version":"0.1.0","rules":[]}`)
		writeFile(t, loc.RuleConfigPath(), `{"version":1,"rules":["orch-guards"]}`)
		st := DetectRules(loc)
		if st.OK() || st.RulebookCurrent || st.RTKWrapper || !st.Referenced {
			t.Errorf("state %+v", st)
		}
	})
	t.Run("formatting differences still count as current", func(t *testing.T) {
		loc := Locate(t.TempDir())
		norm, err := NormalizeJSON(Rulebook())
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, loc.RulebookPath(RulebookName), string(norm))
		if !DetectRules(loc).RulebookCurrent {
			t.Error("reformatted rulebook read as stale")
		}
	})
	for name, body := range map[string]string{"unparseable": `{`, "bad version": `{"version":3}`} {
		t.Run("rule.json "+name, func(t *testing.T) {
			loc := Locate(t.TempDir())
			writeFile(t, loc.RuleConfigPath(), body)
			if st := DetectRules(loc); st.ConfigError == "" || st.OK() {
				t.Errorf("state %+v", st)
			}
		})
	}
	t.Run("oversized rule.json", func(t *testing.T) {
		loc := Locate(t.TempDir())
		writeFile(t, loc.RuleConfigPath(), strings.Repeat(" ", maxRuleFile+1))
		if st := DetectRules(loc); !strings.Contains(st.ConfigError, "larger than") {
			t.Errorf("state %+v", st)
		}
	})
}

func TestNormalizeJSON(t *testing.T) {
	if out, err := NormalizeJSON(nil); out != nil || err != nil {
		t.Errorf("empty: %q %v", out, err)
	}
	if _, err := NormalizeJSON([]byte("{")); err == nil {
		t.Error("bad JSON normalized")
	}
	if sameJSON([]byte("{"), []byte("{}")) {
		t.Error("bad JSON compared equal")
	}
}

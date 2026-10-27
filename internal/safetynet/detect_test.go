package safetynet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixture writes Claude Code plugin files into a temp home.
type fixture struct {
	t   *testing.T
	loc Locations
}

func newFixture(t *testing.T) fixture {
	return fixture{t: t, loc: Locate(t.TempDir())}
}

func (f fixture) installs(entries map[string][]map[string]string) {
	b, _ := json.Marshal(map[string]any{"version": 2, "plugins": entries})
	writeFile(f.t, filepath.Join(f.loc.pluginsDir(), "installed_plugins.json"), string(b))
}

func (f fixture) markets(m map[string]string) {
	doc := map[string]any{}
	for name, path := range m {
		doc[name] = map[string]any{"source": map[string]any{"source": "directory", "path": path}, "installLocation": path}
	}
	b, _ := json.Marshal(doc)
	writeFile(f.t, filepath.Join(f.loc.pluginsDir(), "known_marketplaces.json"), string(b))
}

func (f fixture) enabled(id string, on bool) {
	b, _ := json.Marshal(map[string]any{"enabledPlugins": map[string]any{id: on}, "other": 1})
	writeFile(f.t, f.loc.settingsPath(), string(b))
}

func ours(version, sha string) map[string][]map[string]string {
	return map[string][]map[string]string{PluginID: {{"scope": "user", "version": version, "gitCommitSha": sha, "installPath": "/p"}}}
}

func TestDetectStates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f fixture)
		want  PluginState
		check func(t *testing.T, st State)
	}{
		{name: "nothing installed", setup: func(fixture) {}, want: StateMissing},
		{name: "pinned and enabled", want: StateOK, setup: func(f fixture) {
			f.installs(ours(Pinned.Version, Pinned.SHA))
			f.markets(map[string]string{MarketplaceName: f.loc.MarketplaceDir})
			f.enabled(PluginID, true)
		}},
		{name: "no sha recorded still matches on version", want: StateOK, setup: func(f fixture) {
			f.installs(ours(Pinned.Version, ""))
			f.markets(map[string]string{MarketplaceName: f.loc.MarketplaceDir})
		}},
		{name: "older version", want: StateOutdated, setup: func(f fixture) {
			f.installs(ours("2.0.0", "abc"))
			f.markets(map[string]string{MarketplaceName: f.loc.MarketplaceDir})
		}},
		{name: "same version other commit", want: StateOutdated, setup: func(f fixture) {
			f.installs(ours(Pinned.Version, strings.Repeat("0", 40)))
			f.markets(map[string]string{MarketplaceName: f.loc.MarketplaceDir})
		}},
		{name: "marketplace registered elsewhere", want: StateOutdated, setup: func(f fixture) {
			f.installs(ours(Pinned.Version, Pinned.SHA))
			f.markets(map[string]string{MarketplaceName: "/elsewhere"})
		}, check: func(t *testing.T, st State) {
			if !st.MarketplaceMismatch || st.MarketplacePath != "/elsewhere" {
				t.Errorf("mismatch %v path %q", st.MarketplaceMismatch, st.MarketplacePath)
			}
		}},
		{name: "disabled", want: StateDisabled, setup: func(f fixture) {
			f.installs(ours(Pinned.Version, Pinned.SHA))
			f.markets(map[string]string{MarketplaceName: f.loc.MarketplaceDir})
			f.enabled(PluginID, false)
		}},
		{name: "legacy safety-net with another plugin on its marketplace", want: StateLegacy, setup: func(f fixture) {
			f.installs(map[string][]map[string]string{
				"safety-net@cc-marketplace":         {{"scope": "user", "version": "0.7.1"}},
				"just-so-you-know@cc-marketplace":   {{"scope": "user", "version": "1"}},
				"gopls-lsp@claude-plugins-official": {{"scope": "user", "version": "1"}},
			})
			f.markets(map[string]string{LegacyMarketplace: "/x"})
		}, check: func(t *testing.T, st State) {
			if len(st.Legacy) != 1 || st.Legacy[0].ID != "safety-net@cc-marketplace" || !st.LegacyMarketplaceShared || !st.LegacyMarketplaceKnown {
				t.Errorf("legacy %+v shared %v", st.Legacy, st.LegacyMarketplaceShared)
			}
		}},
		{name: "ours plus a second copy elsewhere", want: StateLegacy, setup: func(f fixture) {
			e := ours(Pinned.Version, Pinned.SHA)
			e["cc-safety-net@willesq-pinned"] = []map[string]string{{"scope": "user", "version": Pinned.Version}}
			f.installs(e)
		}},
		{name: "project-scope copy of ours is not an orch install", want: StateMissing, setup: func(f fixture) {
			f.installs(map[string][]map[string]string{PluginID: {{"scope": "project", "version": Pinned.Version}}})
		}},
		{name: "unreadable plugin files are reported", want: StateMissing, setup: func(f fixture) {
			writeFile(f.t, filepath.Join(f.loc.pluginsDir(), "installed_plugins.json"), "{")
			writeFile(f.t, filepath.Join(f.loc.pluginsDir(), "known_marketplaces.json"), "[")
			writeFile(f.t, f.loc.settingsPath(), "{")
		}, check: func(t *testing.T, st State) {
			if len(st.Errors) != 2 {
				t.Errorf("errors %v", st.Errors)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			st := Detect(f.loc)
			if st.Plugin != tt.want {
				t.Fatalf("state = %s, want %s (%+v)", st.Plugin, tt.want, st)
			}
			if tt.check != nil {
				tt.check(t, st)
			}
			if v, d := st.Summary(); v == "" || d == "" {
				t.Errorf("empty summary %q %q", v, d)
			}
		})
	}
}

func cmdStrings(cmds []Command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.Join(c.Args, " ")
		if c.Summary == "" || !strings.HasPrefix(c.String(), "claude plugin ") {
			panic("bad command " + c.String())
		}
	}
	return out
}

func TestPlanCommands(t *testing.T) {
	dir := "/data/marketplaces/orch-pinned"
	loc := Locations{MarketplaceDir: dir}
	installed := &Install{ID: PluginID, Scope: "user", Version: Pinned.Version, SHA: Pinned.SHA}
	old := &Install{ID: PluginID, Scope: "user", Version: "2.0.0"}
	legacy := Install{ID: "safety-net@cc-marketplace", Name: "safety-net", Marketplace: LegacyMarketplace, Scope: "user", Version: "0.7.1"}
	tests := []struct {
		name string
		st   State
		opts PlanOptions
		want []string
	}{
		{name: "ok plans nothing", st: State{Plugin: StateOK, Installed: installed, MarketplaceKnown: true}},
		{name: "missing", st: State{Plugin: StateMissing},
			want: []string{"plugin marketplace add " + dir, "plugin install " + PluginID + " --scope user"}},
		{name: "legacy migration removes the unused marketplace", st: State{Plugin: StateLegacy, Legacy: []Install{legacy}, LegacyMarketplaceKnown: true},
			want: []string{"plugin marketplace add " + dir, "plugin uninstall safety-net@cc-marketplace --scope user",
				"plugin install " + PluginID + " --scope user", "plugin marketplace remove cc-marketplace"}},
		{name: "legacy marketplace kept while another plugin uses it",
			st:   State{Plugin: StateLegacy, Legacy: []Install{legacy}, LegacyMarketplaceKnown: true, LegacyMarketplaceShared: true, MarketplaceKnown: true, Installed: installed},
			want: []string{"plugin uninstall safety-net@cc-marketplace --scope user"}},
		{name: "project-scope legacy is left alone, and so is its marketplace",
			st: State{Plugin: StateLegacy, LegacyMarketplaceKnown: true, MarketplaceKnown: true, Installed: installed,
				Legacy: []Install{{ID: "safety-net@cc-marketplace", Marketplace: LegacyMarketplace, Scope: "project"}}}},
		{name: "outdated upgrades", st: State{Plugin: StateOutdated, Installed: old, MarketplaceKnown: true},
			want: []string{"plugin marketplace update orch-pinned", "plugin update " + PluginID + " --scope user"}},
		{name: "legacy alongside an outdated copy of ours",
			st:   State{Plugin: StateLegacy, Installed: old, MarketplaceKnown: true, Legacy: []Install{{ID: "cc-safety-net@x", Scope: "user"}}},
			want: []string{"plugin marketplace update orch-pinned", "plugin uninstall cc-safety-net@x --scope user", "plugin update " + PluginID + " --scope user"}},
		{name: "mismatched marketplace is re-registered and reinstalled",
			st:   State{Plugin: StateOutdated, Installed: installed, MarketplaceKnown: true, MarketplaceMismatch: true, MarketplacePath: "/old"},
			want: []string{"plugin marketplace remove orch-pinned", "plugin marketplace add " + dir, "plugin install " + PluginID + " --scope user"}},
		{name: "disabled is left alone by default", st: State{Plugin: StateDisabled, Installed: installed, MarketplaceKnown: true}},
		{name: "disabled is enabled on request", st: State{Plugin: StateDisabled, Installed: installed, MarketplaceKnown: true},
			opts: PlanOptions{EnableDisabled: true}, want: []string{"plugin enable " + PluginID + " --scope user"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.st.Pinned = Pinned
			got := cmdStrings(PlanCommands(tt.st, loc, tt.opts))
			if !slices.Equal(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestCommandEnv(t *testing.T) {
	base := []string{"HOME=/real", "PATH=/bin", "CLAUDE_CONFIG_DIR=/c", "CC_SAFETY_NET_HOME=/s", "QDRANT_API_KEY=k"}
	got := CommandEnv(base, Locations{Home: "/tmp/h", Relocated: true}, []string{"QDRANT_API_KEY"})
	want := []string{"PATH=/bin", "HOME=/tmp/h"}
	if !slices.Equal(got, want) {
		t.Errorf("relocated env %q, want %q", got, want)
	}
	got = CommandEnv(base, Locations{Home: "/real"}, []string{"QDRANT_API_KEY"})
	want = []string{"HOME=/real", "PATH=/bin", "CLAUDE_CONFIG_DIR=/c", "CC_SAFETY_NET_HOME=/s"}
	if !slices.Equal(got, want) {
		t.Errorf("real env %q, want %q", got, want)
	}
}

func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// The fake prints its args and HOME, and fails when asked to.
	script := "#!/bin/sh\necho \"args:$*\"\necho \"home:$HOME\"\n[ \"$1\" = fail ] && { echo boom >&2; exit 3; }\n[ \"$1\" = slow ] && sleep 5\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	r := ExecRunner{Bin: bin, Loc: Locations{Home: home, Relocated: true}}
	out, err := r.Run(context.Background(), []string{"plugin", "install", "x; rm -rf /"})
	if err != nil {
		t.Fatalf("run: %v (%s)", err, out)
	}
	// The argument with shell metacharacters arrives as one literal argument.
	if !strings.Contains(out, "args:plugin install x; rm -rf /") || !strings.Contains(out, "home:"+home) {
		t.Errorf("output %q", out)
	}
	out, err = r.Run(context.Background(), []string{"fail"})
	if err == nil || !strings.Contains(out, "boom") {
		t.Errorf("fail: %v %q", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Run(ctx, []string{"slow"}); err == nil {
		t.Error("cancelled run succeeded")
	}
	if _, err := (ExecRunner{Bin: filepath.Join(dir, "missing")}).Run(context.Background(), nil); err != ErrNoClaude {
		t.Errorf("missing bin: %v", err)
	}
}

func TestExecRunnerStopsTheWholeGroup(t *testing.T) {
	prevTimeout, prevDelay := commandTimeout, waitDelay
	commandTimeout, waitDelay = 300*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { commandTimeout, waitDelay = prevTimeout, prevDelay })
	bin := filepath.Join(t.TempDir(), "claude")
	// A grandchild keeps the output pipe open after the command itself is
	// stopped, the way a git clone started by claude can.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n(sleep 30) &\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := ExecRunner{Bin: bin, Loc: Locations{Home: t.TempDir()}}.Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run took %s; a stuck child held it", d)
	}
}

func TestOutputCapAndCleaning(t *testing.T) {
	b := &tailBuffer{max: maxOutput}
	for range 3 {
		_, _ = b.Write([]byte(strings.Repeat("a", maxOutput)))
	}
	_, _ = b.Write([]byte("TAIL"))
	got := b.String()
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "TAIL") || len(got) > maxOutput+len("…") {
		t.Errorf("tail kept %d bytes", len(got))
	}
	if s := (&tailBuffer{max: 10}).String(); s != "" {
		t.Errorf("empty buffer %q", s)
	}
	in := "  fatal: https://bot:ghp_abcdefghijklmnopqrstuvwxyz0123@github.com/o/r.git failed \n"
	out := cleanOutput(in)
	if strings.Contains(out, "ghp_") || strings.Contains(out, "bot:") || !strings.Contains(out, "https://[REDACTED]@github.com/o/r.git") {
		t.Errorf("cleaned %q", out)
	}
}

func TestLocate(t *testing.T) {
	real, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	t.Setenv("CC_SAFETY_NET_HOME", "/custom/ccsn")
	t.Setenv("ORCH_DATA_DIR", "/custom/data")
	loc := Locate(real)
	if loc.Relocated || loc.CCSNHome != "/custom/ccsn" || loc.MarketplaceDir != "/custom/data/marketplaces/orch-pinned" {
		t.Errorf("real home %+v", loc)
	}
	tmp := t.TempDir()
	loc = Locate(tmp)
	if !loc.Relocated || loc.CCSNHome != filepath.Join(tmp, ".cc-safety-net") ||
		loc.MarketplaceDir != filepath.Join(tmp, ".local", "share", "orch", "marketplaces", "orch-pinned") {
		t.Errorf("relocated %+v", loc)
	}
	if loc.ManifestPath() != filepath.Join(loc.MarketplaceDir, ".claude-plugin", "marketplace.json") ||
		loc.RuleConfigPath() != filepath.Join(tmp, ".cc-safety-net", "rules", "rule.json") {
		t.Errorf("paths %s %s", loc.ManifestPath(), loc.RuleConfigPath())
	}
	for _, bad := range []string{"", "..", "a/b", "-x", strings.Repeat("a", 65)} {
		if loc.RulebookPath(bad) != "" {
			t.Errorf("RulebookPath(%q) accepted", bad)
		}
	}
	t.Setenv("CC_SAFETY_NET_HOME", "relative")
	if Locate(real).CCSNHome != filepath.Join(real, ".cc-safety-net") {
		t.Error("relative CC_SAFETY_NET_HOME honored")
	}
}

func TestReadSmallRefusesNonRegular(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "rule.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readSmall(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("FIFO read")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readSmall blocked on a FIFO")
	}
}

func TestManifest(t *testing.T) {
	b, err := Manifest(Pinned)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string            `json:"name"`
			Source map[string]string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != MarketplaceName || len(m.Plugins) != 1 || m.Plugins[0].Name != PluginName {
		t.Fatalf("manifest %s", b)
	}
	want := map[string]string{"source": "url", "url": Pinned.RepoURL, "ref": Pinned.Ref, "sha": Pinned.SHA}
	for k, v := range want {
		if m.Plugins[0].Source[k] != v {
			t.Errorf("source.%s = %q, want %q", k, m.Plugins[0].Source[k], v)
		}
	}
}

func TestSummary(t *testing.T) {
	okRules := RulesState{Referenced: true, RTKWrapper: true, RulebookCurrent: true}
	tests := []struct {
		st        State
		value     string
		inDetail  string
		overallOK bool
	}{
		{State{Plugin: StateOK, Pinned: Pinned, Rules: okRules}, "ok", "orch-guards active", true},
		{State{Plugin: StateOK, Pinned: Pinned}, "rules incomplete", "Repair rules", false},
		{State{Plugin: StateMissing, Pinned: Pinned, Rules: okRules}, "missing", "not installed", false},
		{State{Plugin: StateOutdated, Pinned: Pinned, Rules: okRules, Installed: &Install{Version: "2.0.0"}}, "outdated", "v2.0.0 installed", false},
		{State{Plugin: StateOutdated, Pinned: Pinned, Rules: okRules}, "outdated", "v? installed", false},
		{State{Plugin: StateDisabled, Pinned: Pinned, Rules: okRules}, "disabled", "claude plugin enable", false},
		{State{Plugin: StateLegacy, Pinned: Pinned, Rules: okRules, Legacy: []Install{{ID: "safety-net@cc-marketplace", Version: "0.7.1"}}}, "legacy", "safety-net@cc-marketplace v0.7.1", false},
		{State{Plugin: StateOK, Pinned: Pinned, Rules: RulesState{ConfigError: "boom"}, Errors: []string{"x: bad"}}, "rules incomplete", "x: bad", false},
	}
	for _, tt := range tests {
		v, d := tt.st.Summary()
		if v != tt.value || !strings.Contains(d, tt.inDetail) || tt.st.OK() != tt.overallOK {
			t.Errorf("%s: got %q %q ok=%v", tt.st.Plugin, v, d, tt.st.OK())
		}
	}
}

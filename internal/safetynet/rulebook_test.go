package safetynet

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ruleCase is one command the orch-guards rulebook must block or allow.
type ruleCase struct {
	expect  string // blocked | allowed
	command string
}

// ruleCases are the shared-policy cases verified against cc-safety-net 2.6.10
// with `explain --json` (analysis only; nothing is executed). The personal
// cases (branch creation, PDF conversion) belong to the user's own rulebook.
var ruleCases = []ruleCase{
	{"blocked", "gh auth token"},
	{"blocked", "gh auth status --show-token"},
	{"blocked", "gh auth status -t"},
	{"blocked", "git push origin :main"},
	{"blocked", "rtk gh auth token"},
	{"blocked", `sh -c "gh auth token"`},
	{"blocked", "echo $(gh auth token)"},
	{"blocked", "GH_TOKEN=$(gh auth token) ./x.sh"},
	{"blocked", "echo host=github.com | gh auth git-credential get"},
	{"blocked", "security find-generic-password -s svc -w"},
	{"blocked", "security find-generic-password -gs svc"},
	{"blocked", "security find-internet-password -s host -w"},
	{"blocked", "security dump-keychain -d"},
	{"blocked", `printf 'host=github.com\n' | git credential fill`},
	{"blocked", "git push origin main"},
	{"blocked", "rtk git push origin main"},
	{"blocked", "git push origin HEAD:main"},
	{"blocked", "git push origin refs/heads/master"},
	{"blocked", "git push --all origin"},
	{"blocked", "git push --mirror origin"},
	{"blocked", "git -C /tmp/x push origin main"},
	{"blocked", `git -c credential.helper= -c 'credential.helper=!gh auth git-credential' push https://github.com/willesq/orchestrator.git main`},
	{"blocked", "git commit --no-verify -m x"},
	{"blocked", "git commit -nm x"},
	{"blocked", "git commit --no-gpg-sign -m x"},
	{"blocked", "git push --no-verify origin feat/x"},
	{"blocked", "gh repo delete willesq/x --yes"},
	{"blocked", "gh repo edit --visibility public"},
	{"allowed", "gh auth status"},
	{"allowed", "gh api -X POST repos/o/r/issues -f title=x"},
	{"allowed", "gh api --method PATCH repos/o/r/pulls/1 -f body=x"},
	{"allowed", "gh pr comment 5 --body hi"},
	{"allowed", "gh pr edit 5 --title x"},
	{"allowed", "gh pr merge 89 --merge --admin"},
	{"allowed", "security find-generic-password -s svc"},
	{"allowed", `git -c credential.helper= -c 'credential.helper=!gh auth git-credential' push https://github.com/willesq/orchestrator.git fix/x`},
	{"allowed", "git push -u origin feat/main-fix"},
	{"allowed", `git commit -S -m "merge main into feature"`},
	{"allowed", "git checkout -b feat/x"},
	{"allowed", "git worktree add ../x -b x"},
	{"allowed", "soffice --headless --convert-to pdf resume.odt"},
	{"allowed", `grep -rn "gh auth token" docs/`},
	{"allowed", "gh repo view willesq/orchestrator"},
}

// knownGaps are commands the token matcher cannot catch, kept so the opt-in
// analyzer test notices if an upgrade starts blocking them. Rules compare
// whole arguments: a refspec naming any source branch cannot be listed, and
// config set through -c is not a flag. Branch protection and the global
// commit.gpgsign setting are the backstops; docs/INSTALL.md lists these.
var knownGaps = []string{
	"git push origin feat:main",
	"git -c commit.gpgsign=false commit -m x",
}

type rulebookDoc struct {
	RulebookVersion int      `json:"rulebook_version"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	AllowedCommands []string `json:"allowed_commands"`
	Rules           []struct {
		Name       string   `json:"name"`
		Command    string   `json:"command"`
		Subcommand string   `json:"subcommand"`
		BlockArgs  []string `json:"block_args"`
		Reason     string   `json:"reason"`
		Intent     string   `json:"intent"`
	} `json:"rules"`
	Tests []struct {
		Command string `json:"command"`
		Expect  string `json:"expect"`
		Rule    string `json:"rule"`
	} `json:"tests"`
}

func loadRulebook(t *testing.T) rulebookDoc {
	t.Helper()
	var rb rulebookDoc
	if err := json.Unmarshal(Rulebook(), &rb); err != nil {
		t.Fatalf("rulebook is not JSON: %v", err)
	}
	return rb
}

// TestRulebookSchema checks the rulebook against cc-safety-net's schema rules,
// because an invalid rulebook makes the whole source inactive silently.
func TestRulebookSchema(t *testing.T) {
	rb := loadRulebook(t)
	if rb.RulebookVersion != 1 {
		t.Errorf("rulebook_version = %d; version 1 is required so git global options are skipped", rb.RulebookVersion)
	}
	if rb.Name != RulebookName || !validName(rb.Name) || rb.Version == "" {
		t.Errorf("name %q version %q", rb.Name, rb.Version)
	}
	intents := []string{"", "hard_stop", "use_alternative", "scope_down", "manual_only", "stop_and_explain"}
	seen := map[string]bool{}
	for _, c := range rb.AllowedCommands {
		if !validName(c) || seen[c] {
			t.Errorf("allowed command %q invalid or duplicated", c)
		}
		seen[c] = true
	}
	names := map[string]bool{}
	for _, r := range rb.Rules {
		if !validName(r.Name) || names[strings.ToLower(r.Name)] {
			t.Errorf("rule name %q invalid or duplicated", r.Name)
		}
		names[strings.ToLower(r.Name)] = true
		if !slices.Contains(rb.AllowedCommands, r.Command) {
			t.Errorf("%s: command %q not in allowed_commands", r.Name, r.Command)
		}
		if r.Subcommand != "" && !validName(r.Subcommand) {
			t.Errorf("%s: subcommand %q invalid", r.Name, r.Subcommand)
		}
		if len(r.BlockArgs) == 0 || slices.Contains(r.BlockArgs, "") {
			t.Errorf("%s: block_args must be non-empty strings", r.Name)
		}
		if r.Reason == "" || len(r.Reason) > 256 {
			t.Errorf("%s: reason length %d, want 1..256", r.Name, len(r.Reason))
		}
		if !slices.Contains(intents, r.Intent) {
			t.Errorf("%s: intent %q", r.Name, r.Intent)
		}
	}
	for _, f := range rb.Tests {
		if f.Expect == "blocked" && !names[strings.ToLower(f.Rule)] {
			t.Errorf("fixture %q names unknown rule %q", f.Command, f.Rule)
		}
	}
}

// TestRulebookShape pins what each guard blocks and, as importantly, what no
// guard touches: guards deny narrowly, and over-blocking is a bug.
func TestRulebookShape(t *testing.T) {
	rb := loadRulebook(t)
	type key struct{ cmd, sub string }
	blocks := map[key][]string{}
	for _, r := range rb.Rules {
		k := key{r.Command, r.Subcommand}
		blocks[k] = append(blocks[k], r.BlockArgs...)
	}
	want := map[key][]string{
		{"gh", "auth"}:                         {"token", "git-credential", "--show-token", "-t"},
		{"security", "find-generic-password"}:  {"-w", "-g"},
		{"security", "find-internet-password"}: {"-w", "-g"},
		{"security", "dump-keychain"}:          {"-d"},
		{"git", "credential"}:                  {"fill"},
		{"git", "push"}:                        {"main", "master", "HEAD:main", "refs/heads/main", ":main", "--all", "--mirror", "--no-verify"},
		{"git", "commit"}:                      {"--no-verify", "-n", "--no-gpg-sign"},
		{"gh", "repo"}:                         {"delete", "--visibility", "--visibility=public"},
	}
	for k, args := range want {
		for _, a := range args {
			if !slices.Contains(blocks[k], a) {
				t.Errorf("%s %s does not block %q", k.cmd, k.sub, a)
			}
		}
	}
	for k := range blocks {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected guard on %s %s: orch-guards holds only the shared-policy rules", k.cmd, k.sub)
		}
	}
	// A feature branch whose name contains "main" is a different token.
	if slices.Contains(blocks[key{"git", "push"}], "feat/main-fix") {
		t.Error("feature branch blocked")
	}
	for _, sub := range []string{"pr", "api"} {
		if _, ok := blocks[key{"gh", sub}]; ok {
			t.Errorf("gh %s must stay allowed", sub)
		}
	}
	for _, personal := range []string{"soffice", "libreoffice", "unoconv"} {
		if slices.Contains(rb.AllowedCommands, personal) {
			t.Errorf("%s is a personal rule, not orch's", personal)
		}
	}
	var cases int
	for _, c := range ruleCases {
		if c.expect != "blocked" && c.expect != "allowed" {
			t.Errorf("case %q: expect %q", c.command, c.expect)
		}
		cases++
	}
	if cases < 30 {
		t.Errorf("only %d rule cases", cases)
	}
}

// TestPinnedSpec checks the pin is well formed and honors the 14-day policy.
func TestPinnedSpec(t *testing.T) {
	if len(Pinned.SHA) != 40 || strings.Trim(Pinned.SHA, "0123456789abcdef") != "" {
		t.Errorf("sha %q is not a full commit id", Pinned.SHA)
	}
	if !strings.HasPrefix(Pinned.RepoURL, "https://github.com/") || Pinned.Ref == "" || Pinned.Version == "" {
		t.Errorf("spec %+v", Pinned)
	}
	released, err := time.Parse("2006-01-02", Pinned.ReleasedOn)
	if err != nil {
		t.Fatalf("released_on: %v", err)
	}
	if time.Since(released) < 14*24*time.Hour {
		t.Errorf("pinned release %s is under 14 days old", Pinned.ReleasedOn)
	}
	if Pinned.CLIVersion != Pinned.Version {
		t.Errorf("CLI %s and plugin %s versions differ", Pinned.CLIVersion, Pinned.Version)
	}
}

// TestRulebookWithPinnedCLI runs every case through the pinned analyzer's
// `explain --json`, which parses commands and executes none. Opt in with
// ORCH_CCSN_CLI=/path/to/dist/bin/cc-safety-net.js (from the installed plugin
// or `npm pack cc-safety-net@<CLIVersion>`); it needs node.
func TestRulebookWithPinnedCLI(t *testing.T) {
	cli := os.Getenv("ORCH_CCSN_CLI")
	if cli == "" {
		t.Skip("set ORCH_CCSN_CLI to the pinned cc-safety-net.js to run the analyzer cases")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	home := t.TempDir()
	loc := Locations{CCSNHome: home}
	if err := os.MkdirAll(filepath.Dir(loc.RulebookPath(RulebookName)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loc.RulebookPath(RulebookName), Rulebook(), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := MergeRuleConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loc.RuleConfigPath(), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		cmd := exec.Command(node, append([]string{cli}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "CC_SAFETY_NET_HOME="+home)
		out, _ := cmd.Output()
		return out
	}
	for _, cmd := range knownGaps {
		ruleCases = append(ruleCases, ruleCase{"allowed", cmd})
	}
	for _, c := range ruleCases {
		var res struct {
			Result string `json:"result"`
		}
		out := run("explain", "--json", c.command)
		if err := json.Unmarshal(out, &res); err != nil {
			t.Errorf("%s: unparseable explain output: %s", c.command, out)
			continue
		}
		if res.Result != c.expect {
			t.Errorf("%s: got %s, want %s", c.command, res.Result, c.expect)
		}
	}
}

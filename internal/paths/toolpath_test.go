package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// launchdPATH is the PATH launchd hands a user agent — the environment orchd
// actually runs in when supervised by launchd or `brew services`.
const launchdPATH = "/usr/bin:/bin:/usr/sbin:/sbin"

func splitPATH(t *testing.T, p string) []string {
	t.Helper()
	return filepath.SplitList(p)
}

// existingPrefix returns a tool prefix that exists on this machine, so the test
// asserts against reality rather than assuming a Homebrew layout.
func existingPrefix(t *testing.T) string {
	t.Helper()
	for _, d := range toolPrefixes() {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	t.Skip("no known tool prefix exists on this machine")
	return ""
}

func TestToolPATH(t *testing.T) {
	want := existingPrefix(t)

	t.Run("appends missing prefix to a minimal launchd PATH", func(t *testing.T) {
		got := splitPATH(t, ToolPATH(launchdPATH))
		if !slices.Contains(got, want) {
			t.Fatalf("ToolPATH did not add %s; got %v", want, got)
		}
		if got[0] != "/usr/bin" {
			t.Fatalf("existing PATH lost precedence: got %v", got)
		}
	})

	t.Run("no duplicates when the prefix is already present", func(t *testing.T) {
		got := splitPATH(t, ToolPATH(want+":/usr/bin"))
		n := 0
		for _, d := range got {
			if d == want {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("prefix %s appears %d times: %v", want, n, got)
		}
	})

	t.Run("empty PATH still yields the tool prefixes", func(t *testing.T) {
		if got := ToolPATH(""); !strings.Contains(got, want) {
			t.Fatalf("ToolPATH(%q) = %q, want it to contain %s", "", got, want)
		}
	})

	t.Run("skips nonexistent directories", func(t *testing.T) {
		for _, d := range splitPATH(t, ToolPATH("/usr/bin")) {
			if fi, err := os.Stat(d); (err != nil || !fi.IsDir()) && d != "/usr/bin" {
				t.Fatalf("ToolPATH included nonexistent dir %s", d)
			}
		}
	})
}

// TestEnsureToolPATHResolvesGH is the regression this exists for: under the PATH
// launchd gives a daemon, exec.LookPath("gh") fails and docs sync flags the owner
// as "error" with no clone attempted.
func TestEnsureToolPATHResolvesGH(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not installed")
	}
	t.Setenv("PATH", launchdPATH)
	if _, err := exec.LookPath("gh"); err == nil {
		t.Skip("gh is on the minimal launchd PATH; nothing to fix here")
	}

	if err := EnsureToolPATH(); err != nil {
		t.Fatalf("EnsureToolPATH: %v", err)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Fatalf("gh still unresolvable after EnsureToolPATH: %v (PATH=%s)", err, os.Getenv("PATH"))
	}
}

func TestEnsureToolPATH(t *testing.T) {
	want := existingPrefix(t)
	t.Setenv("PATH", launchdPATH)

	if err := EnsureToolPATH(); err != nil {
		t.Fatalf("EnsureToolPATH: %v", err)
	}
	if got := os.Getenv("PATH"); !slices.Contains(splitPATH(t, got), want) {
		t.Fatalf("PATH = %q, want it to contain %s", got, want)
	}
}

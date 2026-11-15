package cli

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/willesq/orchestrator/internal/install"
)

// The pieces the uninstall's search for orch's git hooks needs, as vars so
// tests never run git against a real repository or read a real orch.db.
var (
	// uninstallGit runs git for the search. Under a relocated --home, git
	// sees that home as HOME and no system config, so it never reads the
	// machine's own global core.hooksPath.
	uninstallGit = func(home string, relocated bool) install.GitRunner {
		return func(ctx context.Context, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Env = gitSearchEnv(os.Environ(), home, relocated)
			out, err := cmd.Output()
			return string(out), err
		}
	}
	// uninstallPlaces reads, read-only, the places orch has a record of.
	uninstallPlaces = recordedPlaces
)

// gitSearchEnv is env less every GIT_ variable but GIT_EXEC_PATH, so
// nothing in it points git at another repository or config, and,
// relocated, with home as HOME.
func gitSearchEnv(env []string, home string, relocated bool) []string {
	out := make([]string, 0, len(env)+4)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "GIT_") && name != "GIT_EXEC_PATH":
		case relocated && (name == "HOME" || name == "XDG_CONFIG_HOME"):
		default:
			out = append(out, kv)
		}
	}
	out = append(out, "GIT_TERMINAL_PROMPT=0")
	if relocated {
		out = append(out, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_NOSYSTEM=1")
	}
	return out
}

// recordedPlaces opens orch.db read-only (never creating or migrating it,
// and with orchd down) and lists the places its records name. A missing
// or unreadable orch.db leaves the defaults and the spaces sync record.
func recordedPlaces(ctx context.Context, home, dataDir string) install.GitHookPlaces {
	var q install.Querier
	dbPath := filepath.Join(dataDir, "orch.db")
	if info, err := os.Stat(dbPath); err == nil && info.Mode().IsRegular() {
		u := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
		if conn, err := sql.Open("sqlite", u.String()); err == nil {
			defer conn.Close()
			if conn.PingContext(ctx) == nil {
				q = conn
			}
		}
	}
	return install.RecordedGitHookPlaces(ctx, q, home, dataDir)
}

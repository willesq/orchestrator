package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// toolPrefixes are the bin directories third-party CLIs the orchestrator shells
// out to (gh, brew, docker) install into. launchd hands a daemon only
// /usr/bin:/bin:/usr/sbin:/sbin, so none of these are on a supervised orchd's
// PATH and every `exec.Command("gh", ...)` fails with "executable file not found".
func toolPrefixes() []string {
	home, _ := os.UserHomeDir()
	return []string{
		"/opt/homebrew/bin", // Homebrew, Apple silicon
		"/opt/homebrew/sbin",
		"/usr/local/bin", // Homebrew, Intel; manual installs
		"/usr/local/sbin",
		"/opt/local/bin", // MacPorts
		filepath.Join(home, ".local", "bin"),
	}
}

// ToolPATH returns path with every existing tool prefix it lacks appended.
// Appended rather than prepended so an explicit PATH keeps precedence: the goal
// is to make missing tools reachable, not to reorder a caller's choices.
func ToolPATH(path string) string {
	var dirs []string
	have := make(map[string]bool)
	for _, d := range filepath.SplitList(path) {
		if d == "" || have[d] {
			continue
		}
		have[d] = true
		dirs = append(dirs, d)
	}
	for _, d := range toolPrefixes() {
		if have[d] {
			continue
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		have[d] = true
		dirs = append(dirs, d)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// EnsureToolPATH augments the current process's PATH in place, so binaries
// resolved by exec.Command in this process and its children are found under a
// minimal launchd environment. Call once at daemon startup.
func EnsureToolPATH() error {
	return os.Setenv("PATH", ToolPATH(os.Getenv("PATH")))
}

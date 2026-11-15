package install

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
)

// plistEdit writes the launchd plist for orchd, pointing at the BUILT binary so
// the daemon runs a stable compiled artifact (a rebuild doesn't recompile-on-launch).
//
// When orchd is Homebrew-managed (installed under a Cellar prefix), the formula's
// `service do` block supervises it via `brew services`, so we do NOT create a
// competing launchd agent — two KeepAlive supervisors would fight over the socket.
func plistEdit(orchdBin, home string) Edit {
	path := PlistPath(home)
	return Edit{
		Path: path,
		Compute: func(old []byte) ([]byte, string, error) {
			if isHomebrewManaged(orchdBin) {
				return old, "launchd: skipped — orchd is Homebrew-managed (use `brew services start orch`)", nil
			}
			content := plistContent(orchdBin, home)
			verb := "create"
			if len(old) > 0 {
				verb = "replace"
			}
			return []byte(content), fmt.Sprintf("launchd: %s com.orchestrator.orchd -> %s (KeepAlive)", verb, orchdBin), nil
		},
		// The agent is unloaded first, by the daemon unit's undo step.
		Reverse: func(cur []byte) (Undo, error) {
			return undoOwnedFile(cur, "launchd: com.orchestrator.orchd agent", func(b []byte) bool {
				return bytes.Contains(b, []byte("<string>"+OrchdLabel+"</string>"))
			})
		},
	}
}

// OrchdLabel is the launchd label of the agent install writes.
const OrchdLabel = "com.orchestrator.orchd"

// PlistPath is where install writes orchd's launchd agent.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", OrchdLabel+".plist")
}

// launchdUnit is orchd's launchd agent, shared by every harness.
func launchdUnit(orchdBin, home string) Unit {
	return Unit{Name: "launchd agent", Shared: true, Edits: []Edit{plistEdit(orchdBin, home)}}
}

// IsHomebrewManaged reports whether orchd at bin is supervised by `brew
// services` rather than the launchd agent install writes.
func IsHomebrewManaged(bin string) bool { return isHomebrewManaged(bin) }

// isHomebrewManaged reports whether the binary resolves to a Homebrew Cellar
// (both /opt/homebrew/Cellar and /usr/local/Cellar contain "/Cellar/"), in which
// case `brew services` is the daemon supervisor. The path may be either the
// Cellar target or the symlink in <brew prefix>/bin that points at it.
func isHomebrewManaged(bin string) bool {
	if strings.Contains(bin, "/Cellar/") {
		return true
	}
	// Reason: the path we are handed comes from os.Executable(), which on macOS
	// reports the symlink in <brew prefix>/bin rather than its Cellar target. Left
	// unresolved, a Homebrew install looks like a source build and the plan tries
	// to add a launchd agent that competes with `brew services` for the socket.
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return false
	}
	return strings.Contains(resolved, "/Cellar/")
}

func plistContent(orchdBin, home string) string {
	logDir := filepath.Join(home, "Library", "Logs", "orchestrator")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.orchestrator.orchd</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s/orchd.log</string>
  <key>StandardErrorPath</key>
  <string>%s/orchd.err</string>
</dict>
</plist>
`, orchdBin, logDir, logDir)
}

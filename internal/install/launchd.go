package install

import (
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
	path := filepath.Join(home, "Library", "LaunchAgents", "com.orchestrator.orchd.plist")
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
	}
}

// isHomebrewManaged reports whether the binary lives under a Homebrew Cellar
// (both /opt/homebrew/Cellar and /usr/local/Cellar contain "/Cellar/"), in which
// case `brew services` is the daemon supervisor.
func isHomebrewManaged(bin string) bool {
	return strings.Contains(bin, "/Cellar/")
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

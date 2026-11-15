package daemon

import "path/filepath"

// A caller in no agent harness, that orchd did not start, resolves to the
// operator by its process tree alone. That is not enough to read every
// space's memory (D-39): a process an agent detaches from its session
// (a double fork, launchctl, open, osascript) also ends up with no harness
// above it. Operator-scope reads need evidence of the user: a command typed
// at a terminal, or Orch.app. Without it the caller reads public memory
// only. See docs/internals/daemon.md "Reading memory by space".

// Operator evidence.
const (
	// operatorTerminal is a caller running in the foreground process group
	// of its controlling terminal, whose group leader is no harness.
	operatorTerminal = "terminal"
	// operatorApp is Orch.app, installed where install-app.sh puts it.
	operatorApp = "app"
)

// appExecutable is Orch.app's executable inside an installed bundle.
const appExecutable = "Orch.app/Contents/MacOS/Orch"

// operatorEvidence returns why process pid, which runs in no harness, is
// the user ("" when nothing shows it).
//
// Reason for the foreground check: a harness's tool commands inherit its
// controlling terminal and process group, and so does a process an agent
// leaves behind without starting a new session, so having a terminal alone
// proves nothing. A shell gives each command the user types its own
// foreground group; an agent's commands stay in the harness's group, whose
// leader is the harness.
func (p peerCredentials) operatorEvidence(pid int) string {
	if p.program != nil {
		if prog, err := p.program(pid); err == nil && p.isApp(prog.Path) {
			return operatorApp
		}
	}
	if p.terminal == nil {
		return ""
	}
	t, err := p.terminal(pid)
	if err != nil || !t.InForeground() {
		return ""
	}
	if t.Group != pid {
		if p.program == nil || !p.descendsFrom(pid, t.Group) {
			return ""
		}
		// Reason: a leader that cannot be read (it exited, or orchd may
		// not read it) might be a harness, so it shows nothing.
		leader, err := p.program(t.Group)
		if err != nil || harnessOf(leader) != "" {
			return ""
		}
	}
	return operatorTerminal
}

// descendsFrom reports whether pid's parents reach leader, or the process
// that started leader (a shell runs every command of a pipeline in the
// first one's group), without a break. Reason: a process left behind in a
// foreground group (its parent exited, so launchd adopted it) keeps the
// group, and a wrapper that runs a harness without exec leads the harness's
// group; only an unbroken chain shows the caller is a command the group's
// leader, or the shell that made the group, ran.
func (p peerCredentials) descendsFrom(pid, leader int) bool {
	starter, err := p.parent(leader)
	if err != nil || starter <= 1 {
		starter = leader
	}
	for range maxAncestry {
		ppid, err := p.parent(pid)
		switch {
		case err != nil || ppid <= 1 || ppid == pid:
			return false
		case ppid == leader || ppid == starter:
			return true
		}
		pid = ppid
	}
	return false
}

// isApp reports whether path is Orch.app's executable in /Applications or
// the user's own Applications folder. Accepted risk: a process that can
// replace the installed bundle could run its own code as Orch.app, but it
// could equally replace the app the user opens.
func (p peerCredentials) isApp(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	if path == filepath.Join("/Applications", appExecutable) {
		return true
	}
	return p.home != "" && path == filepath.Join(p.home, "Applications", appExecutable)
}

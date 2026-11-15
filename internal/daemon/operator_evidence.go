package daemon

import (
	"bytes"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// A caller in no agent harness, that orchd did not start, resolves to the
// operator by its process tree alone. Under a spaces policy that is not
// enough (D-71, settling RC-705 to RC-707): a process an agent detaches from
// its session also has no harness above it, and it can give itself a
// terminal (a pseudo-terminal of its own), so neither a process tree nor a
// terminal shows the user. Until the unlock confirmed in Orch.app exists
// (SP-077), only Orch.app counts, recognized by its code signature. Without
// that evidence the caller reads public memory only and is refused
// operator-only RPCs. With no policy applied nothing changes (INV-10). See
// docs/internals/daemon.md "Operator evidence".

// operatorApp is the evidence that a caller is Orch.app.
const operatorApp = "app"

const (
	// appIdentifier is Orch.app's bundle identifier, which its signature
	// carries.
	appIdentifier = "com.willesq.orch"
	// appRequirement is the code requirement Orch.app's running code and
	// its bundle on disk must meet.
	appRequirement = `identifier "` + appIdentifier + `"`
	// appExecutable is Orch.app's executable inside its bundle.
	appExecutable = "Contents/MacOS/Orch"
	// codeRuntime is the hardened runtime's code directory flag
	// (kSecCodeSignatureRuntime): the process ignores DYLD_* variables and
	// loads no library another signer made, so its code is its bundle's.
	codeRuntime = 0x10000
)

// codeSignature is what a code signature check learned about one program.
type codeSignature struct {
	// Path is the code's path: a bundle's folder, or an executable.
	Path string
	// Flags are the code directory flags.
	Flags uint32
	// CDHash is the code directory hash, which names the exact code signed.
	CDHash []byte
}

// appVerifier recognizes Orch.app by its code signature (D-71, RC-706).
// Each function is replaced in tests.
type appVerifier struct {
	// running checks the program at the other end of a connection, by the
	// socket's audit token, against a requirement.
	running func(c net.Conn, requirement string) (codeSignature, error)
	// static checks the code on disk at a path against a requirement.
	static func(path, requirement string) (codeSignature, error)
	// exists reports whether a path exists.
	exists func(path string) bool
	// dirs are the folders Orch.app is installed in: /Applications and the
	// user's own Applications folder.
	dirs []string
}

// runningSignature and staticSignature check real signatures; they are set
// only in builds that can (appsig_darwin.go: macOS with cgo), so every other
// build recognizes no Orch.app.
var (
	runningSignature func(c net.Conn, requirement string) (codeSignature, error)
	staticSignature  func(path, requirement string) (codeSignature, error)
)

// newAppVerifier checks real signatures, or none when this build cannot.
func newAppVerifier(home string) *appVerifier {
	if runningSignature == nil || staticSignature == nil {
		return nil
	}
	dirs := []string{"/Applications"}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return &appVerifier{running: runningSignature, static: staticSignature, exists: pathExists, dirs: dirs}
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// installedApp is where Orch.app is installed: /Applications/Orch.app or
// ~/Applications/Orch.app, whichever exists. Reason: with both present one
// may be a copy an agent planted, and orchd cannot tell which, so neither
// counts; install-app.sh removes the other copy when it installs.
func (v *appVerifier) installedApp() (string, bool) {
	var found []string
	for _, dir := range v.dirs {
		if p := filepath.Join(dir, "Orch.app"); v.exists(p) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// isApp reports whether the program at the other end of c is the installed
// Orch.app: its running code meets the app's requirement and has the
// hardened runtime, it runs from the installed bundle, and its code is that
// bundle's on disk (the same code directory hash), so neither a copy
// elsewhere, a program named Orch, nor the real app with an injected
// library or a debugger passes.
func (v *appVerifier) isApp(c net.Conn) bool {
	ok, _ := v.check(c)
	return ok
}

// check is isApp with, for a caller whose code is an Orch.app that is
// refused anyway, the reason; other callers get no reason.
func (v *appVerifier) check(c net.Conn) (bool, string) {
	if v == nil || c == nil {
		return false, ""
	}
	run, err := v.running(c, appRequirement)
	if err != nil {
		return false, ""
	}
	app, ok := v.installedApp()
	switch {
	case !ok:
		return false, "Orch.app is not installed in exactly one of " + strings.Join(v.dirs, " and ")
	case run.Flags&codeRuntime == 0 || len(run.CDHash) == 0:
		return false, "it was signed without the hardened runtime; reinstall it (make app-install)"
	}
	// Reason: the kernel reports the path with symlinks resolved
	// (/tmp is /private/tmp).
	if real, err := filepath.EvalSymlinks(app); err == nil {
		app = real
	}
	if p := filepath.Clean(run.Path); p != app && p != filepath.Join(app, appExecutable) {
		return false, "it runs from " + p + ", not " + app
	}
	disk, err := v.static(app, appRequirement)
	switch {
	case err != nil:
		return false, "the installed bundle's signature does not verify: " + err.Error()
	case disk.Flags&codeRuntime == 0:
		return false, "the installed bundle was signed without the hardened runtime"
	case !bytes.Equal(run.CDHash, disk.CDHash):
		return false, "its code is not the installed bundle's (the bundle changed after it started)"
	}
	return true, ""
}

// policyApplied reports whether operator-only RPCs and operator-scope reads
// need evidence of the user: whenever a spaces policy with spaces is
// applied, or the applied one does not verify (D-71). With none, the
// operator is recognized by process tree alone, as before spaces (INV-10).
func policyApplied(e *spaces.Engine) func() bool {
	return func() bool {
		if e == nil {
			return false
		}
		r, err := e.Current()
		return err != nil || r.Configured()
	}
}

// appCheck is one connection's Orch.app check, run at most once.
type appCheck struct {
	v    *appVerifier
	c    net.Conn
	pid  int
	once sync.Once
	ok   bool
}

// isApp reports whether the connection's caller is Orch.app, checking the
// first time it is asked. A nil check is no app.
func (a *appCheck) isApp() bool {
	if a == nil {
		return false
	}
	a.once.Do(func() {
		var why string
		a.ok, why = a.v.check(a.c)
		if why != "" && appLogs.allow(time.Now()) {
			// Reason: an Orch.app that is refused (installed twice, rebuilt
			// without the hardened runtime, changed on disk) locks the
			// operator out, so the log says why. Any program can carry the
			// app's identifier, so the reason is quoted and rate-limited.
			log.Printf("orchd: process %d is an Orch.app but not the installed one: %q", a.pid, why)
		}
	})
	return a.ok
}

// logLimit lets one log line through per interval.
type logLimit struct {
	mu    sync.Mutex
	every time.Duration
	last  time.Time
}

func (l *logLimit) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		return false
	}
	l.last = now
	return true
}

// appLogs limits the refused-Orch.app log line.
var appLogs = &logLimit{every: 10 * time.Second}

//go:build darwin && cgo

package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// peerProgram is a program that connects to the socket named by its first
// argument and waits to be killed.
const peerProgram = `package main

import (
	"net"
	"os"
	"time"
)

func main() {
	c, err := net.Dial("unix", os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	defer c.Close()
	time.Sleep(time.Minute)
}
`

const appInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>%s</string>
<key>CFBundleExecutable</key><string>Orch</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>
`

// buildPeer compiles peerProgram, its code made distinct by variant.
func buildPeer(t *testing.T, dir, variant string) string {
	t.Helper()
	src := filepath.Join(dir, "peer"+variant+".go")
	prog := peerProgram + "\nvar variant = \"" + variant + "\"\n\nfunc init() { _ = variant }\n"
	if err := os.WriteFile(src, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "peer"+variant)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot build the peer program: %v: %s", err, out)
	}
	return bin
}

// makeApp builds a bundle at app from bin, with identifier id, signed
// ad-hoc with or without the hardened runtime.
func makeApp(t *testing.T, bin, app, id string, hardened bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, appExecutable), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(fmt.Sprintf(appInfoPlist, id)), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--force", "--sign", "-"}
	if hardened {
		args = append(args, "--options", "runtime")
	}
	if out, err := exec.Command("codesign", append(args, app)...).CombinedOutput(); err != nil {
		t.Skipf("codesign: %v: %s", err, out)
	}
}

// peerConn starts exe, which connects to a new socket, and returns orchd's
// end of that connection.
func peerConn(t *testing.T, dir, exe string) net.Conn {
	t.Helper()
	sock := filepath.Join(dir, "s")
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cmd := exec.Command(exe, sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_ = l.(*net.UnixListener).SetDeadline(time.Now().Add(20 * time.Second))
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestAppSignatureOfARealProcess checks Orch.app's recognition (D-71,
// RC-706) against real processes and real ad-hoc signatures: a bundle
// with Orch.app's identifier and the hardened runtime, run from the
// installed folder, is the app; the same bundle without the runtime, with
// another identifier, run from a copy elsewhere, or modified on disk after
// it started, is not.
func TestAppSignatureOfARealProcess(t *testing.T) {
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("codesign is not available")
	}
	// Reason: a socket path must fit in 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "sp025-sig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := buildPeer(t, dir, "a")
	installed := filepath.Join(dir, "Applications")
	v := &appVerifier{running: runningSignature, static: staticSignature, exists: pathExists, dirs: []string{installed}}
	app := filepath.Join(installed, "Orch.app")

	makeApp(t, bin, app, appIdentifier, true)
	if !v.isApp(peerConn(t, dir, filepath.Join(app, appExecutable))) {
		t.Fatal("the signed, hardened app in the installed folder is not recognized")
	}
	other := filepath.Join(dir, "Elsewhere", "Orch.app")
	makeApp(t, bin, other, appIdentifier, true)
	if v.isApp(peerConn(t, dir, filepath.Join(other, appExecutable))) {
		t.Fatal("a copy outside the installed folder is recognized")
	}
	makeApp(t, bin, app, appIdentifier, false)
	if v.isApp(peerConn(t, dir, filepath.Join(app, appExecutable))) {
		t.Fatal("the app without the hardened runtime is recognized")
	}
	makeApp(t, bin, app, "com.example.other", true)
	c := peerConn(t, dir, filepath.Join(app, appExecutable))
	if v.isApp(c) {
		t.Fatal("an app with another identifier is recognized")
	}
	if _, err := runningSignature(c, appRequirement); err == nil {
		t.Fatal("another identifier meets Orch.app's requirement")
	}
	// Re-signed on disk after it started: the running code is no longer
	// the bundle's.
	makeApp(t, bin, app, appIdentifier, true)
	c = peerConn(t, dir, filepath.Join(app, appExecutable))
	makeApp(t, buildPeer(t, dir, "b"), app, appIdentifier, true)
	if v.isApp(c) {
		t.Fatal("a process whose bundle changed on disk is recognized")
	}
	if _, err := staticSignature(filepath.Join(dir, "missing"), appRequirement); err == nil {
		t.Fatal("a missing bundle has a signature")
	}
	if _, err := runningSignature(nil, appRequirement); err == nil {
		t.Fatal("a connection without a socket has a signature")
	}
}

// Package qdrant provisions and manages a NATIVE qdrant engine — no Docker. It
// downloads the pinned qdrant release binary, runs it hardened (localhost-only,
// telemetry off, CORS off) against a storage directory, and health-checks it.
//
// This is what makes the "same data, different engine" cutover possible: point
// the native binary at the existing storage dir (e.g. ~/.claude/orchestrator/
// data/qdrant/) and it serves the existing collections. Only one engine may own
// a storage dir at a time, so the install cutover is stop-docker → start-native.
//
// The network download and process spawn are injected seams so the logic is
// testable without pulling a real binary or opening ports.
package qdrant

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Config controls the native engine.
type Config struct {
	Version  string // pinned release, e.g. v1.17.1
	BinDir   string // where the qdrant binary is cached
	Storage  string // storage directory (the data location)
	HTTPPort int
	GRPCPort int
	APIKey   string // optional; /readyz doesn't require it, CheckKey does
	// Output receives the engine's stdout and stderr; nil discards them.
	Output io.Writer
}

// downloadFunc fetches url into dest.
type downloadFunc func(ctx context.Context, url, dest string) error

// stopFunc terminates a running process.
type stopFunc func() error

// runFunc starts a binary in the given working dir with env and returns a stop handle.
type runFunc func(bin, dir string, env []string) (stopFunc, error)

// Provisioner manages one native qdrant engine.
type Provisioner struct {
	cfg      Config
	download downloadFunc
	run      runFunc
	stop     stopFunc
	httpc    *http.Client
	// pid is the engine this provisioner started; 0 when it did not start one
	// (none running, or an existing engine was reused).
	pid atomic.Int64
}

// New builds a provisioner with real download + exec, overridable in tests.
func New(cfg Config) *Provisioner {
	p := &Provisioner{
		cfg:      cfg,
		download: httpDownload,
		httpc:    &http.Client{Timeout: 3 * time.Second},
	}
	p.run = func(bin, dir string, env []string) (stopFunc, error) {
		return execRun(bin, dir, env, cfg.Output, &p.pid)
	}
	return p
}

// PID is the process ID of the engine this provisioner started, or 0.
func (p *Provisioner) PID() int { return int(p.pid.Load()) }

// BinaryPath is where the qdrant binary is cached.
func (p *Provisioner) BinaryPath() string { return filepath.Join(p.cfg.BinDir, "qdrant") }

// releaseURL is the GitHub download URL for the pinned version on this platform.
func releaseURL(version string) (string, error) {
	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://github.com/qdrant/qdrant/releases/download/%s/%s", version, asset), nil
}

// assetName maps GOOS/GOARCH to the qdrant release asset filename.
func assetName(goos, goarch string) (string, error) {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "qdrant-aarch64-apple-darwin.tar.gz", nil
	case goos == "darwin" && goarch == "amd64":
		return "qdrant-x86_64-apple-darwin.tar.gz", nil
	case goos == "linux" && goarch == "arm64":
		return "qdrant-aarch64-unknown-linux-musl.tar.gz", nil
	case goos == "linux" && goarch == "amd64":
		return "qdrant-x86_64-unknown-linux-musl.tar.gz", nil
	default:
		return "", fmt.Errorf("no qdrant release asset for %s/%s", goos, goarch)
	}
}

// EnsureBinary downloads + extracts the pinned qdrant binary if not already present.
func (p *Provisioner) EnsureBinary(ctx context.Context) error {
	if _, err := os.Stat(p.BinaryPath()); err == nil {
		return nil
	}
	if err := os.MkdirAll(p.cfg.BinDir, 0o755); err != nil {
		return err
	}
	url, err := releaseURL(p.cfg.Version)
	if err != nil {
		return err
	}
	tmp := filepath.Join(p.cfg.BinDir, "qdrant-download.tar.gz")
	if err := p.download(ctx, url, tmp); err != nil {
		return fmt.Errorf("download qdrant: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := extractBinary(tmp, "qdrant", p.BinaryPath()); err != nil {
		return fmt.Errorf("extract qdrant: %w", err)
	}
	return os.Chmod(p.BinaryPath(), 0o755)
}

// Start ensures the binary, launches it hardened against the storage dir, and
// waits until it reports ready. If an engine is already serving on the port
// (e.g. an orphan from a hard kill), it is reused rather than double-spawned,
// but only when it accepts orchd's API key: an engine that does not is someone
// else's, and reusing it would turn every request into a 401.
func (p *Provisioner) Start(ctx context.Context) error {
	if p.Ready(ctx) {
		return p.checkOwnEngine(ctx)
	}
	if err := p.EnsureBinary(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(p.cfg.Storage, 0o755); err != nil {
		return err
	}
	stop, err := p.run(p.BinaryPath(), p.cfg.Storage, p.env())
	if err != nil {
		return err
	}
	p.stop = stop
	// Checked again after spawning: another engine may have taken the port
	// between the readiness probe and ours binding to it.
	err = p.waitReady(ctx, 30*time.Second)
	if err == nil {
		err = p.checkOwnEngine(ctx)
	}
	if err != nil {
		_ = stop()
		p.stop = nil
		return err
	}
	return nil
}

// checkOwnEngine confirms the engine on the HTTP port accepts orchd's API key.
func (p *Provisioner) checkOwnEngine(ctx context.Context) error {
	base := fmt.Sprintf("http://127.0.0.1:%d", p.cfg.HTTPPort)
	err := CheckKey(ctx, base, p.cfg.APIKey)
	if errors.Is(err, ErrKeyRejected) {
		return fmt.Errorf("qdrant on 127.0.0.1:%d rejects orchd's API key, so another engine holds the port; "+
			"stop it or move orchd with rag.qdrant.http_port and rag.qdrant.grpc_port: %w", p.cfg.HTTPPort, err)
	}
	if err != nil {
		return fmt.Errorf("qdrant on 127.0.0.1:%d: %w", p.cfg.HTTPPort, err)
	}
	return nil
}

// Stop terminates the engine if running.
func (p *Provisioner) Stop() error {
	if p.stop == nil {
		return nil
	}
	err := p.stop()
	p.stop = nil
	return err
}

// env is the hardened qdrant configuration (localhost, telemetry off, CORS off).
func (p *Provisioner) env() []string {
	env := append(withoutQdrantSettings(os.Environ()),
		"QDRANT__SERVICE__HOST=127.0.0.1",
		// Cluster mode would open a peer-to-peer port beside the two pinned here.
		"QDRANT__CLUSTER__ENABLED=false",
		fmt.Sprintf("QDRANT__SERVICE__HTTP_PORT=%d", p.cfg.HTTPPort),
		fmt.Sprintf("QDRANT__SERVICE__GRPC_PORT=%d", p.cfg.GRPCPort),
		"QDRANT__STORAGE__STORAGE_PATH="+p.cfg.Storage,
		// Absolute snapshots path: qdrant's default is the relative "./snapshots",
		// which panics when the process cwd is read-only (e.g. "/" under launchd).
		"QDRANT__STORAGE__SNAPSHOTS_PATH="+filepath.Join(p.cfg.Storage, "snapshots"),
		"QDRANT__TELEMETRY_DISABLED=true",
		"QDRANT__SERVICE__ENABLE_CORS=false",
	)
	if p.cfg.APIKey != "" {
		env = append(env, "QDRANT__SERVICE__API_KEY="+p.cfg.APIKey)
	}
	return env
}

// withoutQdrantSettings drops qdrant settings orchd inherited, so only the
// hardened ones above configure the engine. An inherited QDRANT__ variable
// could otherwise set what env leaves alone, such as a second, read-only API
// key or cluster peers.
func withoutQdrantSettings(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "QDRANT__") {
			out = append(out, kv)
		}
	}
	return out
}

// Ready reports whether the engine answers its readiness probe.
func (p *Provisioner) Ready(ctx context.Context) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", p.cfg.HTTPPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// ErrKeyRejected means a qdrant engine answered but refused the API key, which
// usually means a different engine, with its own key, holds the port.
var ErrKeyRejected = errors.New("qdrant rejected the API key")

// CheckKey makes one keyed request (GET /collections) to the engine at baseURL.
// It returns nil when the engine accepts apiKey, or needs no key, and
// ErrKeyRejected on 401 or 403. /readyz needs no key, so only this check shows
// whether an engine that looks ready will serve orchd's requests. Redirects are
// not followed, so the key is never sent to another host.
func CheckKey(ctx context.Context, baseURL, apiKey string) error {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		// The URL is left out: a configured one can carry userinfo credentials.
		return errors.New("check qdrant key: invalid url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.JoinPath("collections").String(), nil)
	if err != nil {
		return fmt.Errorf("check qdrant key: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("api-key", apiKey)
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("check qdrant key: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("check qdrant key: %s: %w", resp.Status, ErrKeyRejected)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("check qdrant key: unexpected status %s", resp.Status)
	}
	return nil
}

func (p *Provisioner) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.Ready(ctx) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("qdrant did not become ready within %s", timeout)
}

// httpDownload fetches url into dest (the real download seam).
func httpDownload(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, resp.Body)
	return err
}

// execRun starts a binary with its output going to out (nil discards it),
// records its process ID in pid, and returns a stop handle that SIGTERMs and
// reaps it.
func execRun(bin, dir string, env []string, out io.Writer, pid *atomic.Int64) (stopFunc, error) {
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started := int64(cmd.Process.Pid)
	pid.Store(started)
	// Wait as soon as it starts, so a crash clears the PID at once: a dead PID
	// left behind could be reused by an unrelated process.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		pid.CompareAndSwap(started, 0)
		close(done)
	}()
	return func() error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
		return nil
	}, nil
}

// extractBinary pulls the entry named `want` out of a .tar.gz into dest.
func extractBinary(tarGzPath, want, dest string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%q not found in archive", want)
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) != want {
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		defer func() { _ = out.Close() }()
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // trusted release archive
			return err
		}
		return nil
	}
}

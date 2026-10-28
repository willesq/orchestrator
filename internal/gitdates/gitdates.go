// Package gitdates reads when a repository's markdown files last changed, so
// indexed docs carry the date of their content rather than of the sync that
// stored them.
package gitdates

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dates are a repository's commit times.
type Dates struct {
	// Head is the HEAD commit's time; zero when unknown.
	Head time.Time
	// Files maps a slash-separated path to the time of the last commit that
	// changed it.
	Files map[string]time.Time
}

// For is when the file at rel last changed: its own commit time when the
// history read had it, else the HEAD commit's (a shallow clone lists every
// file under its one commit), else zero.
func (d Dates) For(rel string) time.Time {
	if t, ok := d.Files[filepath.ToSlash(rel)]; ok {
		return t
	}
	return d.Head
}

// Bounds on one read: a large history is cut short, and the files it did not
// reach fall back to the HEAD commit's time.
var (
	maxLogBytes int64 = 16 << 20
	timeout           = 30 * time.Second
)

// safeArgs keep git from running anything a repository's own config names.
// A local repo's .git/config is whatever is on disk, so it is untrusted: no
// fsmonitor hook, no signature program, no pager, no hooks, and no
// transport at all, since a partial clone's lazy fetch of a missing object
// would run its remote's helper (an ext:: URL, core.sshCommand) inside orchd.
var safeArgs = []string{
	"-c", "core.fsmonitor=false", "-c", "log.showSignature=false", "-c", "core.quotePath=false",
	// protocol.<name>.allow overrides protocol.allow, so each is set too.
	"-c", "protocol.allow=never", "-c", "protocol.ext.allow=never", "-c", "protocol.file.allow=never",
	"-c", "protocol.ssh.allow=never", "-c", "protocol.git.allow=never", "-c", "protocol.http.allow=never",
	"-c", "protocol.https.allow=never",
	"-c", "core.sshCommand=false", "-c", "core.hooksPath=/dev/null",
	"-c", "core.alternateRefsCommand=", "-c", "diff.external=", "--no-pager",
}

// safeEnv backs safeArgs: no lazy fetching, no system config, no prompts.
var safeEnv = []string{"GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	"GIT_PROTOCOL_FROM_USER=0", "GIT_ASKPASS=false", "SSH_ASKPASS=false"}

// Read returns the commit times of the repository at dir. It never fails: a
// directory that is not a repository, or a git that is missing, gives zero
// Dates, and callers then date by when they store.
func Read(ctx context.Context, dir string) Dates {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := Dates{Files: map[string]time.Time{}}
	if out, err := run(ctx, dir, 1<<10, "log", "-1", "--format=%ct", "HEAD"); err == nil {
		d.Head = parseUnix(strings.TrimSpace(string(out)))
	}
	if d.Head.IsZero() {
		return d
	}
	out, _ := run(ctx, dir, maxLogBytes, "log", "--format=%x00%ct", "--name-only", "--no-renames", "--no-color", "--relative",
		"--", ":(icase)*.md")
	d.Files = parse(out)
	return d
}

// run runs git in dir and returns at most limit bytes of its output.
func run(ctx context.Context, dir string, limit int64, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{"-C", dir}, safeArgs...), args...)...)
	cmd.Env = append(os.Environ(), safeEnv...)
	// Its own process group, so stopping it stops anything it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, rerr := io.ReadAll(io.LimitReader(stdout, limit))
	// Output past the limit is not read; stop git rather than wait on it.
	_ = killGroup(cmd)
	_ = cmd.Wait()
	return out, rerr
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// parse reads `git log --format=%x00%ct --name-only` output, newest commit
// first: the first time a path appears is its last change.
func parse(out []byte) map[string]time.Time {
	files := map[string]time.Time{}
	var cur time.Time
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\x00") {
			cur = parseUnix(strings.TrimPrefix(line, "\x00"))
			continue
		}
		if line == "" || cur.IsZero() {
			continue
		}
		if _, ok := files[line]; !ok {
			files[line] = cur
		}
	}
	return files
}

// parseUnix reads Unix seconds; zero when malformed or before 2000. A commit
// dated in the future is dated now, so it cannot claim more recency than
// today's content.
func parseUnix(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 946684800 {
		return time.Time{}
	}
	t := time.Unix(n, 0).UTC()
	if now := time.Now().UTC(); t.After(now) {
		return now.Truncate(time.Second)
	}
	return t
}

package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// prWatchFile is the registry the PR Watch menu bar app reads, relative to home.
const prWatchFile = ".pr-watch.json"

var (
	ghPRCreateRe = regexp.MustCompile(`\bgh\s+pr\s+create\b`)
	prURLRe      = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+`)
)

// prWatchLock bounds how long registration waits for another writer, and when
// a leftover lock counts as abandoned by a crashed writer.
var (
	prWatchLockWait  = 3 * time.Second
	prWatchLockStale = time.Minute
)

// maybeRegisterPR adds the PR a `gh pr create` printed to ~/.pr-watch.json, so
// the PR Watch app picks it up. It only ever appends, and only to a file that
// already exists: the app owns the file, and a user without it has opted out.
func maybeRegisterPR(p Payload, command string) {
	if !ghPRCreateRe.MatchString(command) {
		return
	}
	url := prURLFromResponse(p.ToolResponse)
	if url == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	_ = registerPR(filepath.Join(home, prWatchFile), url, time.Now())
}

// prURLFromResponse finds the last PR URL in a Bash tool response: stdout,
// then stderr, where gh reports "a pull request ... already exists: <url>".
func prURLFromResponse(raw json.RawMessage) string {
	var text string
	var obj struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		text = obj.Stdout + "\n" + obj.Stderr
	} else {
		_ = json.Unmarshal(raw, &text)
	}
	all := prURLRe.FindAllString(text, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// errNotRegistry means the file is not PR Watch JSON and was left untouched.
var errNotRegistry = errors.New("not a PR Watch registry")

// registerPR appends {url, added_at} to the registry at path unless the URL is
// already there. The write is atomic (temp file and rename in the same
// directory) under a lock directory shared with other writers, and keeps the
// file's mode. A missing file or one that does not parse is left alone.
func registerPR(path, url string, now time.Time) error {
	// Reason: the lock is taken at the registry's own path, the lock other
	// writers use, while the write goes to the link's target: a dotfile
	// manager may link the registry into place, and writing the target keeps
	// the link instead of replacing it with a file.
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	unlock, err := lockDir(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	// A FIFO or device would block the read; the registry is a small file.
	if !info.Mode().IsRegular() || info.Size() > maxRegistry {
		return errNotRegistry
	}
	old, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	next, err := appendPR(old, url, now)
	if err != nil || next == nil {
		return err
	}
	return writeAtomic(target, next, info.Mode().Perm())
}

// maxRegistry bounds the registry read; PR Watch keeps a few KiB.
const maxRegistry = 4 << 20

// appendPR returns the registry with url appended, or nil when it is present.
// Existing entries and other top-level keys are kept as they are.
func appendPR(old []byte, url string, now time.Time) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(old, &root); err != nil || root == nil {
		return nil, errNotRegistry
	}
	var prs []json.RawMessage
	// "prs" must be an array: a missing key, null, or any other type is a
	// file this hook does not understand. An empty array decodes non-nil.
	if err := json.Unmarshal(root["prs"], &prs); err != nil || prs == nil {
		return nil, errNotRegistry
	}
	for _, e := range prs {
		var entry struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(e, &entry) == nil && entry.URL == url {
			return nil, nil
		}
	}
	entry, err := json.Marshal(map[string]string{"url": url, "added_at": now.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	prs = append(prs, entry)
	list, err := json.Marshal(prs)
	if err != nil {
		return nil, err
	}
	root["prs"] = list
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// lockDir takes a lock by creating a directory, the same lock other PR Watch
// writers use, waiting briefly and clearing one left by a crashed writer.
func lockDir(lock string) (func(), error) {
	deadline := time.Now().Add(prWatchLockWait)
	for {
		err := os.Mkdir(lock, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > prWatchLockStale {
			// Reason: rename is atomic, so when two writers find the same
			// stale lock only one moves it aside; the other then waits for
			// the winner's fresh lock instead of removing it.
			aside := fmt.Sprintf("%s.stale.%d.%d", lock, os.Getpid(), time.Now().UnixNano())
			if os.Rename(lock, aside) == nil {
				_ = os.Remove(aside)
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("pr-watch registry is locked by another writer")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// writeAtomic replaces path with data via a temp file in the same directory.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

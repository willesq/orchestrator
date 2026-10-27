package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxFileSize bounds how much of a managed file is read. Settings files are
// a few KiB; the cap keeps a stray huge file from stalling every poll.
const MaxFileSize = 4 << 20

// ReadFile reads a managed file, following symlinks. A missing file is nil
// with no error; anything other than a regular file is refused before it is
// opened, since a FIFO would block the open.
func ReadFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, MaxFileSize)
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// writeTarget is the file a write to path lands in: path itself, or the
// file a symlink there points at. A link whose target does not exist is
// refused rather than followed, so a write never creates directories
// wherever a link happens to point.
func writeTarget(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink to a missing file; refusing to write through it", path)
	}
	return path, nil
}

// writeAtomic replaces path's contents with next, refusing when the file no
// longer holds want (the bytes the change was computed from).
//
// A symlink is followed, as install's writes are, so a settings file a
// dotfile manager links in place keeps its link: the new bytes are written to
// a temporary file beside the link's target and renamed over the target,
// which keeps the target's mode. A reader therefore sees the old file or the
// new one, never a partial write. A new file is created owner-only, since
// settings files can hold env secrets.
//
// Accepted risk: the last check and the rename are two steps, so a writer
// that lands in between (microseconds) is overwritten; a backup of the file
// as it was before orch's first change is kept.
func writeAtomic(path string, want, next []byte) error {
	target, err := writeTarget(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	if target == path {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".orch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once renamed
	_, werr := tmp.Write(next)
	if err := errors.Join(werr, tmp.Chmod(mode), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	// Reason: the change was computed from want. A tool that rewrote the file
	// since (Claude Code saving a permission, an editor) would be clobbered,
	// so check again as late as possible and refuse instead. The link is
	// resolved again too: one retargeted meanwhile would make the check read
	// one file while the rename replaced another.
	if again, err := writeTarget(path); err != nil || again != target {
		return fmt.Errorf("%w: %s now points somewhere else; nothing was written — try again", ErrConflict, path)
	}
	current, err := ReadFile(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, want) {
		return fmt.Errorf("%w: %s changed on disk while orch was writing it; nothing was written — try again", ErrConflict, path)
	}
	return os.Rename(tmpName, target)
}

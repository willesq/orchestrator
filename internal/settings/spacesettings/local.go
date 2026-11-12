package spacesettings

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/willesq/orchestrator/internal/settings"
)

// The checkout's settings file. Every access goes through file descriptors
// opened without following links. Reason: a checkout is written by the
// session working in it, which could swap .claude or the file for a link to
// another folder and so make orchd write there.
const (
	claudeDir = ".claude"
	localFile = "settings.local.json"
	// LocalPath is the file's path relative to a checkout.
	LocalPath = claudeDir + "/" + localFile
)

// ErrConflict is returned when the file changed while orch was writing it.
var ErrConflict = errors.New("settings file changed while orch was writing it")

// ErrUnusable is returned for a .claude that is a link or not a folder, or a
// settings file that is a link, not a regular file, or too large: orch
// neither reads nor writes through it.
var ErrUnusable = errors.New("cannot be used as it is")

// openDir opens name in dir (or the absolute path name when dir is -1) as
// a directory, refusing a link.
func openDir(dir int, name string) (int, error) {
	if dir < 0 {
		return unix.Open(name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	return unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// claudeDirFD opens checkout/.claude, creating it when create is set. It
// returns -1 and no error when the folder is absent and create is not set.
func claudeDirFD(checkout string, create bool) (int, error) {
	top, err := openDir(-1, checkout)
	if err != nil {
		return -1, fmt.Errorf("open checkout %s: %w", checkout, err)
	}
	defer unix.Close(top)
	fd, err := openDir(top, claudeDir)
	if errors.Is(err, unix.ENOENT) && create {
		if err := unix.Mkdirat(top, claudeDir, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, fmt.Errorf("create %s/%s: %w", checkout, claudeDir, err)
		}
		fd, err = openDir(top, claudeDir)
	}
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, nil
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, fmt.Errorf("%s/%s is a link or not a folder: %w", checkout, claudeDir, ErrUnusable)
	case err != nil:
		return -1, fmt.Errorf("open %s/%s: %w", checkout, claudeDir, err)
	}
	return fd, nil
}

// readAt reads the settings file in the open .claude folder dir. A missing
// file is nil, false.
func readAt(dir int) ([]byte, bool, error) {
	fd, err := unix.Openat(dir, localFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, false, nil
	case errors.Is(err, unix.ELOOP):
		return nil, false, fmt.Errorf("%s is a link: %w", LocalPath, ErrUnusable)
	case err != nil:
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), localFile)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file: %w", LocalPath, ErrUnusable)
	}
	b, err := io.ReadAll(io.LimitReader(f, settings.MaxFileSize+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > settings.MaxFileSize {
		return nil, false, fmt.Errorf("%s is larger than %d bytes: %w", LocalPath, settings.MaxFileSize, ErrUnusable)
	}
	return b, true, nil
}

// ReadLocal reads checkout's .claude/settings.local.json. A missing file is
// nil, false.
func ReadLocal(checkout string) ([]byte, bool, error) {
	dir, err := claudeDirFD(checkout, false)
	if err != nil || dir < 0 {
		return nil, false, err
	}
	defer unix.Close(dir)
	return readAt(dir)
}

// WriteLocal replaces checkout's settings file with next, refusing when it
// no longer holds old (absent when existed is false). The new content is
// written beside it and renamed over it, so a reader never sees half a
// file; a new file is owner-only, an existing one keeps its mode.
func WriteLocal(checkout string, old []byte, existed bool, next []byte) error {
	dir, err := claudeDirFD(checkout, true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	mode := uint32(0o600)
	var st unix.Stat_t
	if err := unix.Fstatat(dir, localFile, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		mode = uint32(st.Mode) & 0o777
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := "." + localFile + ".orch-" + hex.EncodeToString(suffix[:])
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w", LocalPath, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.Write(next)
	if err := errors.Join(werr, f.Chmod(os.FileMode(mode)), f.Sync(), f.Close()); err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
		return err
	}
	// Reason: the change was computed from old. Claude Code itself writes
	// this file (a permission the user allows), so check again as late as
	// possible and refuse rather than drop its write.
	if err := unchanged(dir, old, existed); err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
		return err
	}
	if err := unix.Renameat(dir, tmp, dir, localFile); err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
		return err
	}
	return nil
}

// RemoveLocal deletes checkout's settings file if it still holds old. The
// .claude folder stays: it may hold the user's own files.
func RemoveLocal(checkout string, old []byte) error {
	dir, err := claudeDirFD(checkout, false)
	if err != nil || dir < 0 {
		return err
	}
	if err := unchanged(dir, old, true); err != nil {
		unix.Close(dir)
		return err
	}
	err = unix.Unlinkat(dir, localFile, 0)
	unix.Close(dir)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// unchanged returns ErrConflict unless the file in dir holds old.
func unchanged(dir int, old []byte, existed bool) error {
	cur, ok, err := readAt(dir)
	if err != nil {
		return err
	}
	if ok != existed || !bytes.Equal(cur, old) {
		return fmt.Errorf("%w: %s; nothing was written, it is tried again at the next sync", ErrConflict, LocalPath)
	}
	return nil
}

// MoveAside renames an unusable .claude, or settings file in it, to
// <name>.orch-moved-<time> beside it, so orch can write its own. A rename
// moves a link itself, never what it points at. It returns the new path.
func MoveAside(checkout string) (string, error) {
	top, err := openDir(-1, checkout)
	if err != nil {
		return "", fmt.Errorf("open checkout %s: %w", checkout, err)
	}
	defer unix.Close(top)
	suffix := fmt.Sprintf(".orch-moved-%d", time.Now().UnixNano())
	dir, err := openDir(top, claudeDir)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		if err := unix.Renameat(top, claudeDir, top, claudeDir+suffix); err != nil {
			return "", err
		}
		return filepath.Join(checkout, claudeDir+suffix), nil
	case err != nil:
		return "", err
	}
	defer unix.Close(dir)
	if err := unix.Renameat(dir, localFile, dir, localFile+suffix); err != nil {
		return "", err
	}
	return filepath.Join(checkout, claudeDir, localFile+suffix), nil
}

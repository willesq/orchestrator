package install

import "golang.org/x/sys/unix"

// renameNoReplace renames from to to, failing when to exists.
func renameNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

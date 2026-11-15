//go:build !darwin && !linux

package install

import (
	"fmt"
	"os"
)

// renameNoReplace renames from to to, failing when to exists. Reason: no
// atomic no-replace rename here, so a file appearing between the check and
// the rename can still be replaced.
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s exists", to)
	}
	return os.Rename(from, to)
}

package session

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionFilesRejectUnsafeIDs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	for _, id := range []string{"", "../escape", "a/b", "x.y", "..", strings.Repeat("a", 129)} {
		t.Run(id, func(t *testing.T) {
			if ValidID(id) {
				t.Errorf("ValidID(%q) = true", id)
			}
			if err := WriteRecord(dir, &Record{SessionID: id}); !errors.Is(err, ErrInvalidSessionID) {
				t.Errorf("WriteRecord err = %v", err)
			}
			if err := WriteCheckpoint(dir, &Record{SessionID: id}); !errors.Is(err, ErrInvalidSessionID) {
				t.Errorf("WriteCheckpoint err = %v", err)
			}
			if err := DeleteCheckpoint(dir, id); !errors.Is(err, ErrInvalidSessionID) {
				t.Errorf("DeleteCheckpoint err = %v", err)
			}
		})
	}
	// Nothing was written anywhere under the root, inside the sessions dir or out.
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("unsafe IDs wrote files: %v", found)
	}
	for _, id := range []string{"sid", "a1b2-C3_d4", "3d6686b9-935c-4701-88ee-a8166010b20c"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false for a real-shaped ID", id)
		}
	}
}

package spaces

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Writing and describing a space's terms file for the app's write-only
// terms field (SP-053). INV-07 holds here too: a term is never returned,
// and every error names the space and a line number at most.

// TermsInfo describes one terms file without a term.
type TermsInfo struct {
	Exists bool
	// Count is how many terms the outbound check reads from it.
	Count int
	// Fingerprint is the first 12 hex digits of HMAC-SHA256 of the file,
	// keyed with key.
	Fingerprint string
	// Problem says why the file cannot be read or parsed.
	Problem string
}

// DescribeTerms reads space's terms file at path and describes it. A file
// that is not there is not a problem: no terms are kept from leaving yet.
func DescribeTerms(space, path string, key []byte) TermsInfo {
	data, err := readSmall(path, maxTermsFile+1)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return TermsInfo{}
	case err != nil:
		return TermsInfo{Exists: true, Problem: fmt.Sprintf("the terms file of space %q cannot be read", space)}
	}
	info := TermsInfo{Exists: true, Fingerprint: termsFingerprint(key, data)}
	list, err := ParseTerms(space, data)
	if err != nil {
		info.Problem = err.Error()
		return info
	}
	info.Count = list.Len()
	return info
}

// termsFingerprint is the first 12 hex digits of HMAC-SHA256 of data.
// Reason: a plain hash of a file holding one short term could be matched
// by hashing guesses; a key only orchd holds stops that.
func termsFingerprint(key, data []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// WriteTerms replaces space's terms file at path with data once it parses
// as a terms file, so the outbound check never reads a broken one. The
// folder is created (0700) when missing; the file is written to a
// temporary file beside it, mode 0600, and renamed over the old one, both
// through a handle on the folder so the folder cannot be swapped between
// the checks and the write.
//
// Reason (SP-053 security review): an agent runs as the user and could
// replace a folder on the way with a symbolic link into its own workspace,
// so that the operator's next terms land where it can read them. So no
// folder from home down to the file may be a link, the folder may not be
// writable by others, and the file itself must be a regular file or absent.
// home is the user's home; "" checks every folder of the path.
func WriteTerms(space, path, home string, data []byte, key []byte) (TermsInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return TermsInfo{}, fmt.Errorf("the terms file of space %q is not an absolute path", space)
	}
	list, err := ParseTerms(space, data)
	if err != nil {
		return TermsInfo{}, err
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	if err := checkTermsFolders(space, dir, home); err != nil {
		return TermsInfo{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return TermsInfo{}, fmt.Errorf("the folder of space %q's terms file cannot be created", space)
	}
	// Folders MkdirAll made are checked as the ones that were there.
	if err := checkTermsFolders(space, dir, home); err != nil {
		return TermsInfo{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return TermsInfo{}, fmt.Errorf("the folder of space %q's terms file cannot be opened", space)
	}
	defer root.Close()
	if fi, err := root.Lstat(base); err == nil && !fi.Mode().IsRegular() {
		return TermsInfo{}, fmt.Errorf("the terms file of space %q is not a regular file", space)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return TermsInfo{}, fmt.Errorf("the terms file of space %q cannot be checked", space)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return TermsInfo{}, fmt.Errorf("the terms file of space %q cannot be written", space)
	}
	tmp := ".terms-" + hex.EncodeToString(rnd[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return TermsInfo{}, fmt.Errorf("the terms file of space %q cannot be written", space)
	}
	defer root.Remove(tmp) // fails harmlessly once renamed
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = root.Rename(tmp, base)
	}
	if werr != nil {
		return TermsInfo{}, fmt.Errorf("the terms file of space %q cannot be written", space)
	}
	return TermsInfo{Exists: true, Count: list.Len(), Fingerprint: termsFingerprint(key, data)}, nil
}

// checkTermsFolders refuses a terms folder reached through a symbolic link
// below home (or anywhere, with no home), or one others may write to.
// Folders that do not exist yet are skipped.
func checkTermsFolders(space, dir, home string) error {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode().IsDir() && fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("the folder of space %q's terms file may be written by other users", space)
	}
	home = filepath.Clean(home)
	for d := dir; ; d = filepath.Dir(d) {
		if home != "." && home != "" && !strings.HasPrefix(d, home+string(filepath.Separator)) {
			// Reason: folders above home belong to the system, which an
			// agent running as the user cannot replace.
			return nil
		}
		fi, err := os.Lstat(d)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return fmt.Errorf("the folder of space %q's terms file cannot be checked", space)
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("the folder of space %q's terms file is reached through a symbolic link", space)
		case !fi.IsDir():
			return fmt.Errorf("the folder of space %q's terms file is not a folder", space)
		}
		if parent := filepath.Dir(d); parent == d {
			return nil
		}
	}
}

// TermsFile is the expanded terms file path of space under res, "" when it
// names none.
func (r *Resolved) TermsFile(space, home string) string {
	s, ok := r.Space(space)
	if !ok || s.TermsFile == "" {
		return ""
	}
	return Env{Home: home}.expand(s.TermsFile)
}

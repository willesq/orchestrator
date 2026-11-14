package spacesettings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// Placer returns the space whose rules a checkout gets: placed is false
// when it gets none (a conflict between its root and its remote,
// quarantine, or a space only its remote names).
type Placer func(ctx context.Context, checkout string) (space string, placed bool, err error)

// Syncer keeps the per-checkout settings files. orchd runs one.
type Syncer struct {
	Layout Layout
	// StateFile records what orch put in each file (0600).
	StateFile string
	// BackupDir keeps each file as it was before orch first changed it.
	BackupDir string
	Place     Placer
	Git       Git

	// mu makes syncs run one at a time.
	mu sync.Mutex
}

// Report says what one sync did.
type Report struct {
	// Checkouts are the checkouts found under the roots.
	Checkouts int
	// Written and Cleared are the checkouts whose file orch wrote with its
	// entries, or took its entries out of.
	Written []string
	Cleared []string
	// Ignored are the checkouts where orch added the file to info/exclude.
	Ignored []string
	// Problems are checkouts orch could not bring up to date, and why.
	Problems []string
	// Truncated reports that discovery stopped at its folder cap.
	Truncated bool
}

// fileState is what the state file holds for one checkout.
type fileState struct {
	Space string `json:"space,omitempty"`
	Owned Owned  `json:"owned"`
	// BackedUp reports that the file as it was before orch's first change
	// is in the backup folder (or that there was no file to back up).
	BackedUp bool `json:"backed_up,omitempty"`
	// Ignored reports that git was last seen ignoring the file.
	Ignored bool `json:"ignored,omitempty"`
}

type state struct {
	Checkouts map[string]fileState `json:"checkouts"`
}

// Sync brings every checkout under res's roots up to date as choice says:
// orch's sandbox rules where they apply (Choice.Sandboxes), elsewhere only
// a space's own gh profile. With the switch off or no policy, it takes
// orch's entries out of every file it wrote and leaves the rest of each
// file as it is.
func (s *Syncer) Sync(ctx context.Context, res *spaces.Resolved, choice Choice) Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rep Report
	st, raw, err := s.load()
	if err != nil {
		rep.Problems = append(rep.Problems, "read "+s.StateFile+": "+err.Error())
		return rep
	}
	want := map[string]Rules{}
	if !choice.SwitchedOff && res.Configured() {
		var roots []string
		for _, r := range res.BoundRoots() {
			roots = append(roots, r.Root)
		}
		var found []string
		found, rep.Truncated = Discover(roots)
		rep.Checkouts = len(found)
		for _, c := range found {
			space, placed, err := s.Place(ctx, c)
			switch {
			case err != nil:
				rep.Problems = append(rep.Problems, c+": place: "+err.Error())
				want[c] = s.kept(res, c, st.Checkouts[c])
			case placed:
				want[c] = For(res, space, s.Layout)
			default:
				want[c] = ForUnplaced(res, c, s.Layout)
			}
		}
		// Reason: fail closed. A checkout orch wrote that this sync did not
		// find or place (its .git renamed by the session in it, or the walk
		// cut short) keeps its rules while it stays under a root; only the
		// switch, the policy or the folder itself going takes them out.
		for c, fs := range st.Checkouts {
			if _, ok := want[c]; !ok && underRoot(res, c) && isDir(c) {
				want[c] = s.kept(res, c, fs)
			}
		}
		// Reason: fail closed. A space whose temp folder orch could not
		// make its own (under /tmp another user can make it first) gets
		// no TMPDIR and no write access to it, rather than one that user
		// could read or link elsewhere.
		bad := map[string]bool{}
		for _, n := range res.Names() {
			if !choice.Sandboxes(For(res, n, s.Layout)) {
				// Its sessions get no TMPDIR from orch's rules.
				continue
			}
			if err := s.Layout.EnsureTmpDir(n); err != nil {
				bad[n] = true
				rep.Problems = append(rep.Problems, "create the TMPDIR of space "+n+": "+err.Error()+"; its sessions get none")
			} else if d := s.Layout.TmpDir(n); s.Layout.Tmp != "" && ClaudeSandboxTmp(d) == SharedClaudeTmp() {
				rep.Problems = append(rep.Problems, "the TMPDIR of space "+n+" ("+d+") is too long for Claude Code; its sessions use the shared "+SharedClaudeTmp())
			}
		}
		for c, r := range want {
			if bad[r.Space] {
				r.TmpDir = ""
				want[c] = r
			}
		}
	}
	paths := make([]string, 0, len(want)+len(st.Checkouts))
	for c := range want {
		paths = append(paths, c)
	}
	for c := range st.Checkouts {
		if _, ok := want[c]; !ok {
			paths = append(paths, c)
		}
	}
	sort.Strings(paths)
	for _, c := range paths {
		if ctx.Err() != nil {
			break
		}
		r, wanted := want[c]
		fs := st.Checkouts[c]
		var frag Fragment
		if wanted {
			frag = choice.CheckoutFor(r, c)
		}
		next, outcome, err := s.syncOne(ctx, c, frag, fs)
		switch {
		case err != nil:
			rep.Problems = append(rep.Problems, c+": "+err.Error())
		case outcome == outcomeWritten:
			rep.Written = append(rep.Written, c)
		case outcome == outcomeCleared:
			rep.Cleared = append(rep.Cleared, c)
		}
		if next.added {
			rep.Ignored = append(rep.Ignored, c)
		}
		if next.note != "" {
			rep.Problems = append(rep.Problems, c+": "+next.note)
		}
		next.st.Space = r.Space
		if next.st.Owned.Empty() && !wanted {
			delete(st.Checkouts, c)
		} else {
			st.Checkouts[c] = next.st
		}
	}
	if err := s.save(st, raw); err != nil {
		rep.Problems = append(rep.Problems, "write "+s.StateFile+": "+err.Error())
	}
	return rep
}

// kept is the rules a checkout keeps when this sync could not place it: its
// last space's, or a checkout with no space's when that space is gone.
func (s *Syncer) kept(res *spaces.Resolved, checkout string, fs fileState) Rules {
	if _, ok := res.Space(fs.Space); ok && fs.Space != "" {
		return For(res, fs.Space, s.Layout)
	}
	return ForUnplaced(res, checkout, s.Layout)
}

// underRoot reports whether path is in or under a bound root.
func underRoot(res *spaces.Resolved, path string) bool {
	for _, r := range res.BoundRoots() {
		if spaces.RootHolds(r.Root, path) {
			return true
		}
	}
	return false
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

type outcome int

const (
	outcomeNone outcome = iota
	outcomeWritten
	outcomeCleared
)

type synced struct {
	st fileState
	// added reports that orch added the file to info/exclude.
	added bool
	// note is a problem the sync worked around, for the report.
	note string
}

// syncOne brings one checkout's file to frag. On an error the state stays
// as it was, so a later sync tries again with what orch really owns.
func (s *Syncer) syncOne(ctx context.Context, checkout string, frag Fragment, fs fileState) (synced, outcome, error) {
	out := synced{st: fs}
	if _, err := os.Lstat(checkout); errors.Is(err, os.ErrNotExist) {
		// The checkout is gone, and its file with it.
		return synced{}, outcomeNone, nil
	}
	old, exists, err := ReadLocal(checkout)
	if errors.Is(err, ErrUnusable) && !frag.Empty() {
		// Reason: fail closed, as for a file orch cannot merge into: a
		// link or an odd file there would leave the checkout without its
		// rules, and a session started in it would load whatever it holds.
		// A tracked entry is the repository's and is never moved.
		if tracked, terr := s.Git.TrackedEntry(ctx, checkout); terr != nil || tracked {
			if tracked {
				return out, outcomeNone, ErrTracked
			}
			return out, outcomeNone, fmt.Errorf("%w; check whether git tracks it: %v", err, terr)
		}
		where, merr := MoveAside(checkout)
		if merr != nil {
			return out, outcomeNone, fmt.Errorf("%w; move it aside: %v", err, merr)
		}
		out.note = fmt.Sprintf("%v; moved to %s", err, where)
		old, exists, err = ReadLocal(checkout)
	}
	if err != nil {
		return out, outcomeNone, err
	}
	m, err := Merge(old, exists, frag, fs.Owned)
	if errors.Is(err, ErrInvalid) && !frag.Empty() {
		// Reason: fail closed. A file orch cannot merge into would leave
		// the checkout without its rules, so it is kept as a backup and
		// replaced by one holding only orch's rules.
		where, berr := s.backupInvalid(checkout, old)
		if berr != nil {
			return out, outcomeNone, fmt.Errorf("%s: %w; back it up: %v", LocalPath, err, berr)
		}
		out.note = fmt.Sprintf("%s: %v; moved to %s and rewritten with orch's rules", LocalPath, err, where)
		m, err = Merge(nil, false, frag, Owned{})
		m.Changed = true
		fs.BackedUp, out.st.BackedUp = true, true
	}
	if err != nil {
		return out, outcomeNone, fmt.Errorf("%s: %w", LocalPath, err)
	}
	// Reason: ignore the file before it holds anything of orch's, so it is
	// never left where 'git add .' would pick it up. A file already in sync
	// and checked is not checked again on every sync.
	if !frag.Empty() && (m.Changed || !fs.Ignored) {
		added, err := s.Git.EnsureIgnored(ctx, checkout)
		out.added = added
		if err != nil {
			out.st.Ignored = false
			return out, outcomeNone, err
		}
		out.st.Ignored = true
	}
	if !m.Changed {
		out.st.Owned = m.Owned
		return out, outcomeNone, nil
	}
	if !fs.BackedUp {
		if exists {
			if err := s.backup(checkout, old); err != nil {
				return out, outcomeNone, fmt.Errorf("back up %s: %w", LocalPath, err)
			}
		}
		out.st.BackedUp = true
	}
	if m.Remove {
		err = RemoveLocal(checkout, old)
	} else {
		err = WriteLocal(checkout, old, exists, m.Data)
	}
	if err != nil {
		out.st.Owned = fs.Owned
		return out, outcomeNone, err
	}
	out.st.Owned = m.Owned
	if frag.Empty() {
		out.st.Ignored = false
		return out, outcomeCleared, nil
	}
	return out, outcomeWritten, nil
}

// backup keeps the file as it was before orch's first change, once.
func (s *Syncer) backup(checkout string, content []byte) error {
	sum := sha256.Sum256([]byte(checkout))
	dir := filepath.Join(s.BackupDir, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The checkout's path, so a backup can be found again.
	if err := writeNew(filepath.Join(dir, "checkout"), []byte(checkout+"\n")); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	err := writeNew(filepath.Join(dir, localFile), content)
	if errors.Is(err, os.ErrExist) {
		// An earlier first change's backup stays: it is the original.
		return nil
	}
	return err
}

// backupInvalid keeps a settings file orch could not merge into, beside the
// checkout's first-change backup, and returns where.
func (s *Syncer) backupInvalid(checkout string, content []byte) (string, error) {
	sum := sha256.Sum256([]byte(checkout))
	dir := filepath.Join(s.BackupDir, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeNew(filepath.Join(dir, "checkout"), []byte(checkout+"\n")); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("invalid-%d.json", time.Now().UnixNano()))
	return path, writeNew(path, content)
}

// writeNew creates path owner-only, refusing one that exists or is a link.
func writeNew(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(content)
	return errors.Join(werr, f.Close())
}

// load reads the state file and returns it with its bytes; a missing file
// is an empty state.
func (s *Syncer) load() (state, []byte, error) {
	st := state{Checkouts: map[string]fileState{}}
	b, err := os.ReadFile(s.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil, nil
	}
	if err != nil {
		return st, nil, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, nil, err
	}
	if st.Checkouts == nil {
		st.Checkouts = map[string]fileState{}
	}
	return st, b, nil
}

// save writes the state file when it changed. With no file and nothing to
// record it writes none, so orchd without spaces leaves no file behind.
func (s *Syncer) save(st state, old []byte) error {
	if old == nil && len(st.Checkouts) == 0 {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if bytes.Equal(b, old) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.StateFile), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.StateFile), ".checkouts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(b)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.StateFile)
}

// EnsurePrivateDir creates dir owner-only, and refuses one that is a link,
// not a folder or another user's, and makes one others may enter
// owner-only. Reason: Claude Code refuses a CLAUDE_CODE_TMPDIR another user
// could reach, a space's temp files are its own, and under the shared /tmp
// another user can make the folder first.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user (uid %d)", dir, st.Uid)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

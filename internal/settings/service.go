package settings

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Service is the reconciler: it imports what providers find in their files
// into the store, and applies each change made through orch to the files.
//
// Every method runs under one lock, so changes are serialized across files
// and a poll never interleaves with a write.
type Service struct {
	store *Store
	reg   *Registry
	// backup saves a file's bytes before orch first changes it; nil skips.
	backup func(path string, content []byte) error

	mu sync.Mutex
	// seen is a hash of each provider's files at the last import, so a poll
	// that finds them unchanged does nothing.
	seen map[string][32]byte
	// backedUp records the files backed up by this process.
	backedUp map[string]bool
	// state is each provider's last read: when, and why it failed.
	state map[string]ProviderState
	// revision increases whenever a row changes, so a client can tell a
	// list apart from the one it already shows.
	revision int64
	now      func() time.Time
}

// ProviderState is what the last look at a provider's files found.
type ProviderState struct {
	ObservedAt time.Time
	Error      string
	Files      []string
}

// NewService builds a reconciler. backup, when set, receives each file's
// bytes before the first change this process makes to it.
func NewService(store *Store, reg *Registry, backup func(path string, content []byte) error) *Service {
	return &Service{
		store: store, reg: reg, backup: backup,
		seen: map[string][32]byte{}, backedUp: map[string]bool{}, state: map[string]ProviderState{},
		revision: 1, now: time.Now,
	}
}

// Registry returns the providers the service manages.
func (s *Service) Registry() *Registry { return s.reg }

// Sync imports every provider's files whose bytes changed since the last
// look. It is the poll orchd runs, and runs before every list and change.
func (s *Service) Sync(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.reg.Providers() {
		files, err := s.syncLocked(ctx, p)
		if err != nil {
			continue
		}
		if _, ok := p.(Authoritative); !ok {
			continue
		}
		problems, err := s.enforceLocked(ctx, p, files, nil)
		s.setState(p, files, errors.Join(problems, err))
	}
}

// enforceLocked rewrites an authoritative provider's files to match the
// stored rows, with override (when set) in place of its stored row, and
// re-imports what it wrote. It returns the files Enforce left alone
// (problems) apart from a failure to write (err). Other providers are left
// alone: for them the file wins.
func (s *Service) enforceLocked(ctx context.Context, p Provider, files map[string][]byte, override *Row) (problems, err error) {
	a, ok := p.(Authoritative)
	if !ok {
		return nil, nil
	}
	rows, err := s.store.List(ctx, p.Name(), "")
	if err != nil {
		return nil, err
	}
	state := map[string]bool{}
	for _, r := range rows {
		if r.RemovedAt.IsZero() {
			state[r.Kind+"\x00"+r.Identity] = r.Enabled
		}
	}
	if override != nil {
		state[override.Kind+"\x00"+override.Identity] = override.Enabled
	}
	changes, problems := a.Enforce(files, func(kind, identity string) (bool, bool) {
		on, known := state[kind+"\x00"+identity]
		return on, known
	})
	if changes.Empty() {
		return problems, nil
	}
	var logRow Row
	if override != nil {
		logRow = *override
	}
	if err := s.writeFiles(p, files, changes.Write, logRow, logRow.Enabled); err != nil {
		return problems, err
	}
	if err := s.deleteFiles(p, files, changes); err != nil {
		return problems, err
	}
	return problems, s.reimportLocked(ctx, p)
}

// deleteFiles removes the files and then the emptied directories an
// authoritative provider asked for, under the same rules as a write: only
// paths it read, each backed up before this process first changes it.
func (s *Service) deleteFiles(p Provider, files map[string][]byte, c Changes) error {
	for _, path := range c.Delete {
		old, known := files[path]
		if !known {
			return fmt.Errorf("settings provider %s tried to delete %s, which it did not read", p.Name(), path)
		}
		if old == nil {
			continue
		}
		if s.backup != nil && len(old) > 0 && !s.backedUp[path] {
			if err := s.backup(path, old); err != nil {
				return fmt.Errorf("back up %s: %w", path, err)
			}
			s.backedUp[path] = true
		}
		if err := removeIfUnchanged(path, old); err != nil {
			return err
		}
		log.Printf("settings: %s: removed %s", p.Name(), path)
	}
	for _, dir := range c.Prune {
		if !holdsReadFile(dir, files) {
			return fmt.Errorf("settings provider %s tried to remove %s, which holds no file it read", p.Name(), dir)
		}
		if removeEmptyDir(dir) {
			log.Printf("settings: %s: removed empty %s", p.Name(), dir)
		}
	}
	return nil
}

// holdsReadFile reports whether dir is an ancestor of a file in files.
func holdsReadFile(dir string, files map[string][]byte) bool {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	for path := range files {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// syncLocked reads a provider's files and imports them if they changed,
// returning the bytes read.
func (s *Service) syncLocked(ctx context.Context, p Provider) (map[string][]byte, error) {
	files, err := p.Read()
	if err != nil {
		s.setState(p, nil, err)
		return nil, err
	}
	h := hashFiles(files)
	if prev, ok := s.seen[p.Name()]; ok && prev == h {
		return files, nil
	}
	if err := s.importLocked(ctx, p, files); err != nil {
		s.setState(p, files, err)
		return nil, err
	}
	s.seen[p.Name()] = h
	s.setState(p, files, nil)
	return files, nil
}

func (s *Service) setState(p Provider, files map[string][]byte, err error) {
	st := ProviderState{ObservedAt: s.now(), Files: sortedPaths(files)}
	if err != nil {
		st.Error = err.Error()
	}
	// Reason: logged on change only, since a broken file would otherwise
	// log on every poll.
	if prev := s.state[p.Name()]; prev.Error != st.Error {
		s.revision++
		if st.Error != "" {
			log.Printf("settings: %s: %s", p.Name(), st.Error)
		}
	}
	s.state[p.Name()] = st
}

// importLocked records what the files hold. The file wins for a change made
// outside orch: a new entry is imported (as orch's or found) with the state
// the file gives it, an entry turned back on in the file is on, and one
// removed from the file is marked removed rather than put back — except an
// enabled orch item, which stays enabled for orch install to restore.
func (s *Service) importLocked(ctx context.Context, p Provider, files map[string][]byte) error {
	observed, err := p.Observe(files)
	if err != nil {
		return err
	}
	if len(observed) > MaxItems {
		return fmt.Errorf("%d items is more than the %d orch manages per provider; nothing was imported", len(observed), MaxItems)
	}
	// One transaction, so a failure part way leaves the last import whole.
	return s.store.inTx(ctx, func(st *Store) error {
		changed, err := s.importRows(ctx, st, p, observed)
		if err == nil && changed {
			s.revision++
		}
		return err
	})
}

// MaxItems bounds the items imported from one provider, which keeps every
// import and listing a bounded amount of work.
const MaxItems = 5000

// importRows reconciles the stored rows with what was observed, reporting
// whether any row changed.
func (s *Service) importRows(ctx context.Context, st *Store, p Provider, observed []Item) (bool, error) {
	changed := false
	rows, err := st.List(ctx, p.Name(), "")
	if err != nil {
		return false, err
	}
	byKey := map[string]Row{}
	for _, r := range rows {
		byKey[r.Kind+"\x00"+r.Identity] = r
	}
	_, authoritative := p.(Authoritative)
	seen := map[string]bool{}
	for _, it := range observed {
		key := it.Kind + "\x00" + it.Identity
		seen[key] = true
		r, ok := byKey[key]
		next := r
		if !ok {
			next = Row{Provider: p.Name(), Kind: it.Kind, Identity: it.Identity, Origin: it.Origin}
		}
		next.Payload, next.Enabled, next.Present, next.RemovedAt = it.Payload, it.Enabled, true, time.Time{}
		if authoritative && ok && r.RemovedAt.IsZero() {
			// Reason: orch.db is the truth for these items; what the file
			// shows is drift for Enforce to undo, not a choice to import.
			next.Enabled = r.Enabled
		}
		if ok && sameRow(r, next) {
			continue
		}
		if err := st.Put(ctx, next); err != nil {
			return false, err
		}
		changed = true
	}
	for _, r := range rows {
		if seen[r.Kind+"\x00"+r.Identity] || !r.Present {
			continue
		}
		spec, _ := Kind(p, r.Kind)
		next := r
		next.Present = false
		if spec.StateInFile || r.Origin != OriginOrch {
			if !r.Enabled && !spec.StateInFile {
				// Turned off through orch and so already out of the file.
				continue
			}
			next.RemovedAt = s.now()
			if !spec.StateInFile {
				next.Enabled = false
			}
		}
		if err := st.Put(ctx, next); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func sameRow(a, b Row) bool {
	return a.Enabled == b.Enabled && a.Present == b.Present && a.RemovedAt.Equal(b.RemovedAt) &&
		a.Origin == b.Origin && string(a.Payload) == string(b.Payload)
}

// Listing is a snapshot of managed items.
type Listing struct {
	Rows      []Row
	Providers map[string]ProviderState
	Revision  int64
}

// List syncs, then returns the rows of one provider and kind (empty for all).
func (s *Service) List(ctx context.Context, provider, kind string) (Listing, error) {
	if provider != "" && s.reg.Get(provider) == nil {
		return Listing{}, fmt.Errorf("%w: settings provider %q", ErrNotFound, provider)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.reg.Providers() {
		if provider == "" || p.Name() == provider {
			_, _ = s.syncLocked(ctx, p)
		}
	}
	rows, err := s.store.List(ctx, provider, kind)
	if err != nil {
		return Listing{}, err
	}
	states := map[string]ProviderState{}
	for k, v := range s.state {
		states[k] = v
	}
	return Listing{Rows: rows, Providers: states, Revision: s.revision}, nil
}

// SetEnabled turns an item on or off in its file and records the choice.
func (s *Service) SetEnabled(ctx context.Context, provider, kind, identity string, enabled bool) (Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, spec, files, err := s.prepare(ctx, provider, kind)
	if err != nil {
		return Row{}, err
	}
	r, err := s.store.Get(ctx, provider, kind, identity)
	if err != nil {
		return Row{}, fmt.Errorf("%w: %s %s %q", err, provider, kind, identity)
	}
	if spec.StateInFile && !r.Present {
		return Row{}, fmt.Errorf("%w: %s %q is no longer in its file, so it cannot be turned on or off", ErrNotAllowed, kind, identity)
	}
	if r.Enabled == enabled && r.RemovedAt.IsZero() && (spec.StateInFile || r.Present == enabled) {
		return r, nil
	}
	r.Enabled, r.RemovedAt = enabled, time.Time{}
	if !spec.StateInFile {
		r.Present = enabled
	}
	if err := s.applyLocked(ctx, p, files, r); err != nil {
		return Row{}, err
	}
	return s.store.Get(ctx, provider, kind, identity)
}

// Add validates a new item, writes it to its file and records it as yours.
func (s *Service) Add(ctx context.Context, provider, kind string, fields map[string]string) (Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, spec, files, err := s.prepare(ctx, provider, kind)
	if err != nil {
		return Row{}, err
	}
	if !spec.Addable {
		return Row{}, fmt.Errorf("%w: %s items cannot be added here", ErrNotAllowed, kind)
	}
	it, err := p.Validate(kind, fields)
	if err != nil {
		return Row{}, err
	}
	if existing, err := s.store.Get(ctx, provider, kind, it.Identity); err == nil {
		if existing.Status() == StatusActive {
			return Row{}, fmt.Errorf("%w: that %s is already in place", ErrAlreadyExists, kind)
		}
		return Row{}, fmt.Errorf("%w: that %s is already listed (%s) — turn it on instead", ErrAlreadyExists, kind, existing.Status())
	} else if !errors.Is(err, ErrNotFound) {
		return Row{}, err
	}
	r := Row{Provider: provider, Kind: kind, Identity: it.Identity, Enabled: true, Present: true,
		Origin: OriginYou, Payload: it.Payload}
	if err := s.applyLocked(ctx, p, files, r); err != nil {
		return Row{}, err
	}
	return s.store.Get(ctx, provider, kind, it.Identity)
}

// Remove deletes an item from its file and forgets it. Orch items can only
// be turned off, since the next orch install would put them back. An item
// already gone from its file (status removed) of any kind or origin is only
// forgotten, so rows for vanished entries do not pile up.
func (s *Service) Remove(ctx context.Context, provider, kind, identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, spec, files, err := s.prepare(ctx, provider, kind)
	if err != nil {
		return err
	}
	r, err := s.store.Get(ctx, provider, kind, identity)
	if err != nil {
		return fmt.Errorf("%w: %s %s %q", err, provider, kind, identity)
	}
	if r.Status() == StatusRemoved {
		if err := s.store.Delete(ctx, provider, kind, identity); err != nil {
			return err
		}
		s.revision++
		return nil
	}
	if !spec.Removable {
		return fmt.Errorf("%w: %s items cannot be removed here, only turned off", ErrNotAllowed, kind)
	}
	if r.Origin == OriginOrch {
		return fmt.Errorf("%w: orch install adds this %s, so it can be turned off but not removed", ErrNotAllowed, kind)
	}
	if r.Present {
		if err := s.writeLocked(p, files, r, false); err != nil {
			return err
		}
	}
	if err := s.store.Delete(ctx, provider, kind, identity); err != nil {
		return err
	}
	s.revision++
	return s.reimportLocked(ctx, p)
}

// prepare resolves a provider and kind and brings the store up to date with
// the files, so a change is computed from what is on disk now.
func (s *Service) prepare(ctx context.Context, provider, kind string) (Provider, KindSpec, map[string][]byte, error) {
	p := s.reg.Get(provider)
	if p == nil {
		return nil, KindSpec{}, nil, fmt.Errorf("%w: settings provider %q", ErrNotFound, provider)
	}
	spec, ok := Kind(p, kind)
	if !ok {
		return nil, KindSpec{}, nil, fmt.Errorf("%w: %s has no kind %q", ErrNotFound, provider, kind)
	}
	files, err := s.syncLocked(ctx, p)
	if err != nil {
		return nil, KindSpec{}, nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	return p, spec, files, nil
}

// applyLocked writes a row's state to its file, then records the row and
// re-imports the files as written.
//
// Reason: the row is recorded before the re-import, so the import sees an
// item turned off through orch as intended rather than as removed outside it.
func (s *Service) applyLocked(ctx context.Context, p Provider, files map[string][]byte, r Row) error {
	if _, ok := p.(Authoritative); ok {
		// Reason: a file Enforce must leave alone (problems) does not block
		// the choice; the files it could write are written, the row is the
		// truth, and the problem stays visible until fixed. A failed write
		// records nothing.
		problems, err := s.enforceLocked(ctx, p, files, &r)
		if err != nil {
			return err
		}
		if err := s.store.Put(ctx, r); err != nil {
			return err
		}
		s.revision++
		err = s.reimportLocked(ctx, p)
		s.setState(p, files, problems)
		return err
	}
	if err := s.writeLocked(p, files, r, r.Enabled); err != nil {
		return err
	}
	if err := s.store.Put(ctx, r); err != nil {
		return err
	}
	s.revision++
	return s.reimportLocked(ctx, p)
}

// writeLocked computes and writes the files that change for r.
func (s *Service) writeLocked(p Provider, files map[string][]byte, r Row, enabled bool) error {
	changed, err := p.Apply(files, Item{Kind: r.Kind, Identity: r.Identity, Payload: r.Payload, Origin: r.Origin}, enabled)
	if err != nil {
		return err
	}
	return s.writeFiles(p, files, changed, r, enabled)
}

// writeFiles writes a provider's changed files: only files it read, each
// backed up before this process first changes it, each replaced atomically.
func (s *Service) writeFiles(p Provider, files, changed map[string][]byte, r Row, enabled bool) error {
	for _, path := range sortedPaths(changed) {
		old, known := files[path]
		if !known {
			// Reason: a provider writes only what it read, which keeps every
			// write inside the files it declared.
			return fmt.Errorf("settings provider %s tried to write %s, which it did not read", p.Name(), path)
		}
		if s.backup != nil && len(old) > 0 && !s.backedUp[path] {
			if err := s.backup(path, old); err != nil {
				return fmt.Errorf("back up %s: %w", path, err)
			}
			s.backedUp[path] = true
		}
		if err := writeAtomic(path, old, changed[path]); err != nil {
			return err
		}
		// Reason: an identity can hold a whole permission rule, which may
		// embed a secret, and orchd's log is readable by queued tasks; a
		// short hash still ties log lines to one item.
		if r.Identity == "" {
			log.Printf("settings: %s: wrote %s (restored stored state)", p.Name(), path)
			continue
		}
		sum := sha256.Sum256([]byte(r.Identity))
		log.Printf("settings: %s: wrote %s (%s %s %x on=%v)", p.Name(), path, r.Origin, r.Kind, sum[:4], enabled)
	}
	return nil
}

// reimportLocked imports the files as they are now, after a write.
func (s *Service) reimportLocked(ctx context.Context, p Provider) error {
	delete(s.seen, p.Name())
	_, err := s.syncLocked(ctx, p)
	return err
}

// hashFiles hashes a file map in path order.
func hashFiles(files map[string][]byte) [32]byte {
	h := sha256.New()
	for _, p := range sortedPaths(files) {
		b := files[p]
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		if b == nil {
			h.Write([]byte{1})
		}
		h.Write(b)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

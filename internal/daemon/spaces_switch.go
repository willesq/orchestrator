package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The memory spaces switch (spaces.enabled, off by default). Off, the spaces
// engine reports no policy to every reader, so orchd behaves as before spaces
// (INV-10's path), and orchd merges every space's collection back into the
// base. On, orchd first splits the base back into the spaces' collections
// by each chunk's stored space and only then enforces: until the split ends
// it behaves as off, as it did a moment before, so turning spaces on never
// shows more than off did. A policy, its revisions and every record are
// kept either way. See docs/internals/daemon.md "Spaces switch".

// Switch states, as SpacesStatus.state reports them.
const (
	spacesStateOn        = "on"
	spacesStateOff       = "off"
	spacesStateTurningOn = "turning_on"
)

// storeMover is the part of the RAG service the switch moves the store with.
type storeMover interface {
	MergeSpaces(ctx context.Context, progress func(rag.MoveReport)) (rag.MoveReport, error)
	SplitSpaces(ctx context.Context, progress func(rag.MoveReport)) (rag.MoveReport, error)
}

// moveMarker is the file recording a split the switch still owes. Reason:
// orchd may stop mid-split, and enforcing before the split ends would let a
// Shared read of the base reach chunks of other spaces. The marker is
// written before spaces.enabled is set, so a store that says on with a split
// pending always has it.
type moveMarker struct {
	Direction string    `json:"direction"`
	At        time.Time `json:"at"`
}

// moveRetry is how long the switch waits before running a move again after
// it failed, or while a migration or relabel holds the lock.
var moveRetry = time.Minute

// spacesSwitch applies the switch and runs its store moves, one at a time.
type spacesSwitch struct {
	engine *spaces.Engine
	mover  storeMover
	// markerPath is the pending-split marker; empty keeps none (tests).
	markerPath string
	// applied is told whenever enforcement starts or stops, so the guard
	// snapshot and the checkouts' settings follow.
	applied func()
	// enforce runs once enforcement starts after a split: the relabel of
	// content written while spaces were off. It runs again after moveRetry
	// until it succeeds.
	enforce func(ctx context.Context) error

	// ctx is orchd's run context, which every move ends with.
	ctx context.Context

	mu     sync.Mutex
	want   bool // spaces.enabled as last applied
	cancel context.CancelFunc
	done   chan struct{}
	move   *orchdv1.SpacesStoreMove
}

func newSpacesSwitch(engine *spaces.Engine, mover storeMover, markerPath string) *spacesSwitch {
	return &spacesSwitch{engine: engine, mover: mover, markerPath: markerPath}
}

// defaultMoveMarker is where orchd keeps the marker, beside the migrations'
// state.
func defaultMoveMarker(dataDir string) string {
	return filepath.Join(dataDir, "migrations", "spaces-split.json")
}

// prepare applies enabled at startup, before anything reads the engine: off,
// or on but owing a split, the engine enforces nothing.
func (w *spacesSwitch) prepare(enabled bool) {
	w.mu.Lock()
	w.want = enabled
	w.mu.Unlock()
	w.engine.SetOff(!enabled || w.splitPending())
}

// start logs the state prepare set and runs the move it owes until ctx, orchd's
// run context, ends: a merge while off (one listing when there is nothing to
// merge), the pending split while turning on.
func (w *spacesSwitch) start(ctx context.Context) {
	w.ctx = ctx
	w.mu.Lock()
	want := w.want
	w.mu.Unlock()
	switch {
	case !want:
		log.Printf("orchd: memory spaces are off (spaces.enabled); orch works as before spaces")
		// Reason, fail closed: never merge without the record that a split
		// is owed; the next start tries again.
		if err := w.writeMarker(); err != nil {
			log.Printf("orchd: spaces: record the pending split: %v; the store is not merged", err)
			return
		}
		w.run(rag.MoveMerge)
	case w.engine.Off():
		log.Printf("orchd: memory spaces are turning on: the store is split back first, then enforced")
		w.run(rag.MoveSplit)
	default:
		log.Printf("orchd: memory spaces are on")
	}
}

// change applies a new value of spaces.enabled, which store saves. Turning
// on records the split it owes before saving, so a stop between the two
// never enforces a merged store.
func (w *spacesSwitch) change(enabled bool, by string, store func() error) error {
	// Reason: a move still running could finish between the marker and the
	// new value and act on the old one; it starts again below.
	w.stop()
	w.mu.Lock()
	was := w.want
	w.mu.Unlock()
	if enabled && was && !w.engine.Off() {
		// Already on and enforcing: the store is split.
		return store()
	}
	// Reason: the marker says a split is owed. It is written before "on" is
	// stored and before a merge moves anything, so a store that may hold
	// other spaces' chunks in its base is never enforced against.
	if err := w.writeMarker(); err != nil {
		return fmt.Errorf("record the pending split: %w", err)
	}
	if err := store(); err != nil {
		return err
	}
	w.mu.Lock()
	w.want = enabled
	w.mu.Unlock()
	if enabled {
		log.Printf("orchd: memory spaces switched on by %s (was %s); splitting the store back, then enforcing", by, onOff(was))
		w.run(rag.MoveSplit)
		return nil
	}
	log.Printf("orchd: memory spaces switched off by %s (was %s); nothing is enforced, the policy and its records are kept, "+
		"every space's memory moves back into one store and quarantined content is readable again", by, onOff(was))
	w.engine.SetOff(true)
	w.notify()
	w.run(rag.MoveMerge)
	return nil
}

func onOff(on bool) string {
	if on {
		return spacesStateOn
	}
	return spacesStateOff
}

// run stops any move running and starts one in direction. A split that
// ends turns enforcement on; a move that fails runs again after moveRetry.
// Without a run context (start not called) it runs nothing.
func (w *spacesSwitch) run(direction string) {
	parent := w.ctx
	if parent == nil {
		return
	}
	w.stop()
	w.mu.Lock()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	w.cancel, w.done = cancel, done
	w.mu.Unlock()
	go func() {
		defer close(done)
		w.loop(ctx, direction)
	}()
}

// stop cancels the running move, if any, and waits for it to end.
func (w *spacesSwitch) stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel, w.done = nil, nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// loop runs direction's move until it succeeds or ctx ends.
func (w *spacesSwitch) loop(ctx context.Context, direction string) {
	for {
		err := w.once(ctx, direction)
		if err == nil {
			if direction == rag.MoveSplit {
				w.enforceNow(ctx)
			}
			// A merged store keeps its marker: a split is owed before
			// anything is enforced again.
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("orchd: spaces store %s: %v; trying again in %s", direction, err, moveRetry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(moveRetry):
		}
	}
}

// errSplitIncomplete keeps enforcement off while the base still holds
// chunks the split could not move.
var errSplitIncomplete = errors.New("some chunks could not be moved back into their spaces; enforcement waits until they are (see orchd's log)")

// errMoveBusy is a move that waits for a migration, re-index or relabel.
var errMoveBusy = errors.New("a hybrid migration, backup drop, keyword re-index or relabel is running")

// once runs one move under the migration lock.
func (w *spacesSwitch) once(ctx context.Context, direction string) error {
	if w.mover == nil {
		return nil
	}
	if !migrating.TryLock() {
		return errMoveBusy
	}
	defer migrating.Unlock()
	started := time.Now()
	progress := func(r rag.MoveReport) { w.setMove(moveProto(r, started, true, "")) }
	var rep rag.MoveReport
	var err error
	if direction == rag.MoveMerge {
		rep, err = w.mover.MergeSpaces(ctx, progress)
	} else {
		rep, err = w.mover.SplitSpaces(ctx, progress)
	}
	if err == nil && direction == rag.MoveSplit && rep.Failed > 0 {
		// Reason, fail closed: a chunk left in the base may belong to
		// another space, and a Shared read of the base would reach it.
		err = errSplitIncomplete
	}
	if err != nil {
		w.setMove(moveProto(rep, started, false, err.Error()))
		return err
	}
	w.setMove(nil)
	if rep.Sources > 0 || rep.Failed > 0 || rep.Dropped > 0 {
		log.Printf("orchd: spaces store %s done: %d source(s), %d chunk(s) moved, %d left in place, %d collection(s) removed",
			direction, rep.Sources, rep.Chunks, rep.Failed, rep.Dropped)
	}
	return nil
}

// enforceNow turns enforcement on once a split ended, unless spaces were
// switched off meanwhile.
func (w *spacesSwitch) enforceNow(ctx context.Context) {
	w.mu.Lock()
	want := w.want
	if want {
		w.engine.SetOff(false)
	}
	w.mu.Unlock()
	if !want {
		return
	}
	w.removeMarker()
	log.Printf("orchd: memory spaces are on and enforced")
	w.notify()
	for w.enforce != nil {
		err := w.enforce(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Printf("orchd: spaces: relabelling content written while spaces were off: %v; trying again in %s", err, moveRetry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(moveRetry):
		}
	}
}

func (w *spacesSwitch) notify() {
	if w.applied != nil {
		w.applied()
	}
}

func (w *spacesSwitch) setMove(m *orchdv1.SpacesStoreMove) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.move = m
}

func moveProto(r rag.MoveReport, started time.Time, running bool, err string) *orchdv1.SpacesStoreMove {
	return &orchdv1.SpacesStoreMove{Direction: r.Direction, Running: running, Collections: int32(r.Collections),
		CollectionsDone: int32(r.Done), Sources: int32(r.Sources), Chunks: int32(r.Chunks), Failed: int32(r.Failed),
		StartedAt: started.Unix(), Error: err}
}

// state is the switch for SpacesStatus, and the move while one runs or
// after one failed.
func (w *spacesSwitch) state() (string, *orchdv1.SpacesStoreMove) {
	if w == nil {
		return "", nil
	}
	w.mu.Lock()
	want, move := w.want, w.move
	w.mu.Unlock()
	switch {
	case !want:
		return spacesStateOff, move
	case w.engine.Off():
		return spacesStateTurningOn, move
	}
	return spacesStateOn, move
}

// off reports whether spaces are off or not yet enforced.
func (w *spacesSwitch) off() bool { return w != nil && w.engine.Off() }

// wantsOff reports whether spaces are switched off (not turning on).
func (w *spacesSwitch) wantsOff() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.want
}

func (w *spacesSwitch) splitPending() bool {
	if w.markerPath == "" {
		return false
	}
	_, err := os.Stat(w.markerPath)
	return err == nil
}

func (w *spacesSwitch) writeMarker() error {
	if w.markerPath == "" {
		return nil
	}
	b, err := json.Marshal(moveMarker{Direction: rag.MoveSplit, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.markerPath), 0o700); err != nil {
		return err
	}
	tmp := w.markerPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.markerPath)
}

func (w *spacesSwitch) removeMarker() {
	if w.markerPath == "" {
		return
	}
	if err := os.Remove(w.markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("orchd: spaces: remove the pending-split marker: %v", err)
	}
}

// wait blocks until the running move, if any, ends; for tests.
func (w *spacesSwitch) wait() {
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
}

// relabelAfterSplit brings content written while spaces were off up to the
// applied policy once enforcement starts: it was stored unplaced (labels_v
// 0), so it is stale and unreadable in any space until relabelled. Nothing
// else is placed again: a chunk whose labels_v is current keeps its space.
func (s *server) relabelAfterSplit(ctx context.Context) error {
	if _, rev, err := s.spaces.CurrentRevision(); err != nil || rev == 0 {
		return nil
	}
	rep, err := s.relabel(ctx, true)
	if err != nil {
		return err
	}
	if rep.Changed > 0 || rep.Failed > 0 {
		log.Printf("orchd: spaces: relabelled %d chunk(s) written while spaces were off (%d moved, %d left stale)", rep.Changed, rep.Moved, rep.Failed)
	}
	return nil
}

// spacesSwitchAtStart is spaces.enabled as orchd starts with it. A store
// with no stored value that applied a policy reads as on, and the value is
// stored, so the app and orch status show what orchd enforces. Reason, fail
// closed: a store that cannot be read keeps enforcing.
func spacesSwitchAtStart(ctx context.Context, store *sql.DB) bool {
	on, stored, err := config.SpacesEnabledIn(ctx, store)
	if err != nil {
		log.Printf("orchd: spaces: reading spaces.enabled: %v; memory spaces stay on", err)
		return true
	}
	if on && !stored {
		log.Printf("orchd: memory spaces stay on: a spaces policy was applied and spaces.enabled was not stored " +
			"(an upgrade from before the switch, or the value was removed outside orch)")
		if err := config.ApplyStore(ctx, store, map[string]any{config.KeySpacesEnabled: true}, true, "orchd:keep-on"); err != nil {
			log.Printf("orchd: spaces: storing spaces.enabled: %v", err)
		}
	}
	return on
}

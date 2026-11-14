package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The files the enforcing tool guard in orch-hook reads (spaces SP-041):
// orchd writes the applied policy with the guard's and the terms layer's
// switches into one snapshot, and each session's pin into a pin file, so
// the hook decides a call without waiting on orchd. See
// docs/internals/hooks.md "Enforcing guard".

// guardPinSyncEvery is how often pin files of sessions whose harness
// process exited are removed, and missing ones of live sessions written.
const guardPinSyncEvery = 10 * time.Minute

// guardWriter writes the guard's files.
type guardWriter struct {
	dir  string
	home string
	// configDir and dataDir are orch's directories and binDir holds its
	// binaries, all protected from agent writes (SP-042).
	configDir, dataDir, binDir string
	// mu orders snapshot writes and guards sw and terms, the guard's and
	// the terms layer's switches as last given.
	mu    sync.Mutex
	sw    spaces.Switch
	terms spaces.Switch
}

func newGuardWriter(dir, home, configDir, dataDir, binDir string) *guardWriter {
	return &guardWriter{dir: dir, home: home, configDir: configDir, dataDir: dataDir, binDir: binDir,
		sw:    spaces.Switch{Layer: spaces.LayerGuard, State: spaces.LayerGuard.Default()},
		terms: spaces.Switch{Layer: spaces.LayerTerms, State: spaces.LayerTerms.Default()}}
}

// defaultGuardWriter writes the guard's files where orch-hook reads them.
func defaultGuardWriter() *guardWriter {
	return newGuardWriter(paths.GuardDir(), userHome(), paths.ConfigDir(), paths.DataDir(), executableDir())
}

// executableDir is the directory of the running binary, where orch installs
// orch, orchd and orch-hook side by side; "" when it cannot be found.
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// protection is what the guard protects under res (SP-042,
// spaces.GuardProtection). The snapshot and a replay both take it from
// here, so the hook and a replay judge the same paths (RC-110).
func (g *guardWriter) protection(res *spaces.Resolved) spaces.Protection {
	return spaces.GuardProtection(g.home, g.configDir, g.dataDir, g.dir, g.binDir, res)
}

// guardProtection is the protection the guard applies under res, for a
// replay: the guard writer's, or where orch-hook reads its files when
// orchd writes none.
func (s *server) guardProtection(res *spaces.Resolved) spaces.Protection {
	g := s.guard
	if g == nil {
		g = defaultGuardWriter()
	}
	return g.protection(res)
}

// setSwitches takes every layer's switch after a change and rewrites the
// snapshot with the guard's and the terms layer's (RC-116). It runs under
// the switches' lock, so it must not call back into them.
func (s *server) setSwitches(e spaces.Enforcement) {
	g := s.guard
	if g == nil {
		return
	}
	g.take(e)
	s.writeGuardSnapshot()
}

// take keeps the guard's and the terms layer's switches of e, those it
// carries.
func (g *guardWriter) take(e spaces.Enforcement) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sw, ok := e[spaces.LayerGuard]; ok {
		g.sw = sw
	}
	if sw, ok := e[spaces.LayerTerms]; ok {
		g.terms = sw
	}
}

// writeGuardSnapshot writes the snapshot from the policy orchd enforces and
// the guard's switch. Without a spaces engine it removes the snapshot, so
// the hook falls back to orch.db and refuses while a policy is applied.
func (s *server) writeGuardSnapshot() {
	g := s.guard
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	path := spaces.GuardSnapshotPath(g.dir)
	if s.spaces == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("orchd: guard: remove the policy snapshot: %v", err)
		}
		return
	}
	snap := spaces.GuardSnapshot{
		Home: g.home, WrittenAt: time.Now().UTC(),
		Guard: spaces.GuardSwitch{State: g.sw.State, Until: g.sw.Until, Previous: g.sw.Previous},
		Terms: spaces.GuardSwitch{State: g.terms.State, Until: g.terms.Until, Previous: g.terms.Previous},
	}
	res, rev, err := s.spaces.CurrentRevision()
	switch {
	case err != nil:
		snap.Broken, res = true, nil
	case res != nil:
		p := res.Policy
		snap.Policy, snap.Revision = &p, rev
	}
	snap.Protection = g.protection(res)
	if err := writeSnapshotFile(g.dir, snap); err != nil {
		// Reason: a stale snapshot would keep a looser policy or switch in
		// force; without one the hook refuses while orch.db has a policy.
		log.Printf("orchd: guard: write the policy snapshot: %v; removing it, so the guard refuses until it is written", err)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("orchd: guard: remove the stale policy snapshot: %v", err)
		}
	}
}

// writeSnapshotFile writes the snapshot; tests replace it to fail.
var writeSnapshotFile = spaces.WriteGuardSnapshot

// writeGuardPin writes p's pin file.
func (s *server) writeGuardPin(p sessionPin) {
	if s.guard == nil {
		return
	}
	if err := spaces.WriteGuardPin(s.guard.dir, guardPinOf(p)); err != nil {
		log.Printf("orchd: guard: write the pin file of %s: %v", printableID(p.SessionID), err)
	}
}

func guardPinOf(p sessionPin) spaces.GuardPinFile {
	return spaces.GuardPinFile{SessionID: p.SessionID, Space: p.Space, Revision: p.Revision, Harness: p.Harness}
}

// syncGuardPins writes the pin file of every session whose harness process
// has not exited and removes every other pin file, so the directory holds
// the running sessions only.
func (s *server) syncGuardPins(ctx context.Context) {
	if s.guard == nil || s.pins == nil {
		return
	}
	pins, err := s.pins.all(ctx)
	if err != nil {
		log.Printf("orchd: guard: list pins: %v", err)
		return
	}
	keep := map[string]bool{}
	for _, p := range pins {
		if !p.ExitedAt.IsZero() {
			continue
		}
		keep[p.SessionID+".json"] = true
		s.writeGuardPin(p)
	}
	dir := filepath.Dir(spaces.GuardPinPath(s.guard.dir, "x"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if keep[e.Name()] || !ok {
			continue
		}
		// Reason: a session pinned after the list was read has its pin in
		// orch.db before its file is written; it must keep the file.
		if p, found, err := s.pins.get(ctx, id); err != nil || (found && p.ExitedAt.IsZero()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("orchd: guard: remove a pin file: %v", err)
		}
	}
}

// startGuardFiles writes the snapshot and the pin files at startup, takes
// the guard's switch from the switches, and keeps the pin files in step
// with the sessions until ctx ends.
func (s *server) startGuardFiles(ctx context.Context) {
	if s.guard == nil {
		return
	}
	if e := s.enforcement; e != nil {
		// Reason: read and hooked under one lock, so no change falls
		// between them.
		e.mu.Lock()
		s.guard.take(e.cur)
		e.onChange = s.setSwitches
		e.mu.Unlock()
	}
	s.writeGuardSnapshot()
	go func() {
		t := time.NewTicker(guardPinSyncEvery)
		defer t.Stop()
		for {
			s.syncGuardPins(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

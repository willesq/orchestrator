package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Unit is one piece of what orch puts on a machine — a harness's settings
// file, the shared policy, the launchd agent, the git hooks — with its
// install and its uninstall defined together, in the same place.
//
// The pairing rule: every install action has its reverse beside it. Each
// Edit carries Compute (install) and Reverse (uninstall); Steps (install
// actions beyond file writes) come with UndoSteps. `orch install` and
// `orch uninstall` walk the same registered list of units, install in order
// and uninstall in reverse, and ValidateUnits refuses a unit that breaks the
// rule, so a new install action cannot ship without its uninstall.
//
// Some things orch owns are put in place by orchd or another command rather
// than by install (the data directory, per-checkout settings, git hooks).
// Such a unit has no install action, names what installs it in InstalledBy,
// and still carries its uninstall.
type Unit struct {
	Name string
	// Shared units serve every harness (the shared policy, the shell alias,
	// the launchd agent). Uninstalling a single harness leaves them, since
	// another harness may still load them.
	Shared bool
	Edits  []Edit
	// Steps plans the install's actions from the current state; UndoSteps
	// plans the uninstall's. Each step must be idempotent.
	Steps     func() []Step
	UndoSteps func() []Step
	// Dirs are directories only this unit's files live in. An uninstall
	// removes each one it leaves empty, deepest first.
	Dirs []string
	// InstalledBy names what puts this unit in place, for a unit whose
	// install is not one of orch install's own actions.
	InstalledBy string
	// Manual lists what an install leaves to the user (Homebrew, a login);
	// Leftovers what an uninstall leaves (an app it cannot quit).
	Manual    func() []string
	Leftovers func() []string
}

// Undo is the result of an edit's Reverse: the file's content once orch's
// part is out of it, or Remove when nothing else was in it.
type Undo struct {
	Data    []byte
	Remove  bool
	Summary string
}

// keepUndo leaves the file as it is.
func keepUndo(cur []byte, summary string) Undo { return Undo{Data: cur, Summary: summary} }

// removeUndo deletes the file.
func removeUndo(summary string) Undo { return Undo{Remove: true, Summary: summary} }

// ValidateUnits checks the pairing rule for every unit: each edit has both
// directions, install steps have undo steps, and a unit with no install
// action says what installs it and still has an uninstall.
func ValidateUnits(units []Unit) error {
	var errs []error
	for _, u := range units {
		if u.Name == "" {
			errs = append(errs, errors.New("a unit has no name"))
		}
		for _, e := range u.Edits {
			if e.Compute == nil || e.Reverse == nil {
				errs = append(errs, fmt.Errorf("unit %s: edit of %s lacks its install or its uninstall", u.Name, e.Path))
			}
		}
		if u.Steps != nil && u.UndoSteps == nil {
			errs = append(errs, fmt.Errorf("unit %s: has install steps but no undo steps", u.Name))
		}
		installs := len(u.Edits) > 0 || u.Steps != nil || u.Manual != nil
		if !installs && u.InstalledBy == "" {
			errs = append(errs, fmt.Errorf("unit %s: installs nothing and does not say what installs it", u.Name))
		}
		uninstalls := len(u.Edits) > 0 || u.UndoSteps != nil || len(u.Dirs) > 0 || u.Leftovers != nil
		if !uninstalls {
			errs = append(errs, fmt.Errorf("unit %s: has no uninstall", u.Name))
		}
	}
	return errors.Join(errs...)
}

// dedupe drops repeated units (shared units appear in every harness) and
// repeated edits of one path, keeping the first.
func dedupe(units []Unit) []Unit {
	seenUnit, seenPath := map[string]bool{}, map[string]bool{}
	out := make([]Unit, 0, len(units))
	for _, u := range units {
		if u.Shared && seenUnit[u.Name] {
			continue
		}
		seenUnit[u.Name] = true
		edits := make([]Edit, 0, len(u.Edits))
		for _, e := range u.Edits {
			if !seenPath[e.Path] {
				seenPath[e.Path] = true
				edits = append(edits, e)
			}
		}
		u.Edits = edits
		out = append(out, u)
	}
	return out
}

// InstallPlan is the plan that installs units in order: every edit, then
// every step. A unit that breaks the pairing rule fails the plan.
func InstallPlan(target string, units []Unit) Plan {
	units = dedupe(units)
	p := Plan{Target: target, err: ValidateUnits(units)}
	var steps []func() []Step
	for _, u := range units {
		p.Edits = append(p.Edits, u.Edits...)
		if u.Steps != nil {
			steps = append(steps, u.Steps)
		}
	}
	if len(steps) > 0 {
		p.Steps = joinSteps(steps)
	}
	p.Notes = joinNotes(units, func(u Unit) func() []string { return u.Manual })
	return p
}

// UninstallPlan is the plan that uninstalls units in reverse: every undo
// step, then every edit's Reverse, then the empty directories. Shared units
// are left out when sharedToo is false (one harness of several).
func UninstallPlan(target string, units []Unit, sharedToo bool) Plan {
	units = dedupe(units)
	p := Plan{Target: target, Uninstall: true, err: ValidateUnits(units)}
	var steps []func() []Step
	var kept []Unit
	for i := len(units) - 1; i >= 0; i-- {
		u := units[i]
		if u.Shared && !sharedToo {
			continue
		}
		kept = append(kept, u)
		for j := len(u.Edits) - 1; j >= 0; j-- {
			e := u.Edits[j]
			e.undo, e.Unlink = true, nil
			p.Edits = append(p.Edits, e)
		}
		if u.UndoSteps != nil {
			steps = append(steps, u.UndoSteps)
		}
		p.Prune = append(p.Prune, u.Dirs...)
	}
	if len(steps) > 0 {
		p.Steps = joinSteps(steps)
	}
	p.Prune = deepestFirst(p.Prune)
	p.Notes = joinNotes(kept, func(u Unit) func() []string { return u.Leftovers })
	return p
}

func joinSteps(fs []func() []Step) func() []Step {
	return func() []Step {
		var out []Step
		for _, f := range fs {
			out = append(out, f()...)
		}
		return out
	}
}

func joinNotes(units []Unit, pick func(Unit) func() []string) func() []string {
	var fs []func() []string
	for _, u := range units {
		if f := pick(u); f != nil {
			fs = append(fs, f)
		}
	}
	if len(fs) == 0 {
		return nil
	}
	return func() []string {
		var out []string
		for _, f := range fs {
			out = append(out, f()...)
		}
		return out
	}
}

// deepestFirst orders directories so a child is removed before its parent,
// dropping repeats.
func deepestFirst(dirs []string) []string {
	out := slices.Clone(dirs)
	for i := range out {
		out[i] = filepath.Clean(out[i])
	}
	slices.SortStableFunc(out, func(a, b string) int {
		if da, db := strings.Count(a, string(filepath.Separator)), strings.Count(b, string(filepath.Separator)); da != db {
			return db - da
		}
		return strings.Compare(a, b)
	})
	return slices.Compact(out)
}

// evaluateUndo measures an edit's Reverse against the file on disk.
//
// Reason: a symlink is never deleted. Install writes through a link a
// dotfile manager put there, so the file behind it is the user's; when only
// orch's part was in it, the link stays and the file behind it is emptied.
func (e Edit) evaluateUndo(old []byte) (evaluated, error) {
	if e.Reverse == nil {
		return evaluated{}, fmt.Errorf("%s: edit has no uninstall", e.Path)
	}
	u, err := e.Reverse(old)
	if err != nil {
		return evaluated{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	info, lerr := os.Lstat(e.Path)
	exists := lerr == nil
	if u.Remove {
		switch {
		case !exists:
			return evaluated{old: old, next: old, summary: u.Summary}, nil
		case info.Mode()&os.ModeSymlink != 0:
			next := []byte{}
			return evaluated{old: old, next: next, changed: len(old) > 0,
				summary: u.Summary + " — the symlink stays (the file behind it is yours); emptied instead"}, nil
		case !info.Mode().IsRegular():
			return evaluated{}, fmt.Errorf("%s: not a regular file — refusing to remove it", e.Path)
		}
		return evaluated{old: old, summary: u.Summary, changed: true, remove: true}, nil
	}
	ev := evaluated{old: old, next: u.Data, summary: u.Summary, changed: !bytes.Equal(old, u.Data)}
	if ev.changed && e.Normalize != nil {
		if norm, nerr := e.Normalize(old); nerr == nil && bytes.Equal(norm, u.Data) {
			ev.changed = false
		}
	}
	return ev, nil
}

// printUninstall renders an uninstall plan: steps first, as they run.
func (p Plan) printUninstall(w io.Writer, evs []evaluated) {
	fmt.Fprintf(w, "DRY RUN — uninstall %q. Nothing will be changed.\n", p.Target)
	steps := p.steps()
	for _, s := range steps {
		fmt.Fprintf(w, "\n=== RUN ===\n%s\n", s.Summary)
	}
	changed := 0
	for i, e := range p.Edits {
		ev := evs[i]
		if !ev.changed {
			continue
		}
		changed++
		action := "MODIFY"
		if ev.remove {
			action = "REMOVE"
		}
		fmt.Fprintf(w, "\n=== %s %s ===\n%s\n", action, e.Path, ev.summary)
	}
	for _, ev := range evs {
		// A file left in place because it is not orch's is worth naming
		// even though nothing changes.
		if !ev.changed && strings.Contains(ev.summary, "left in place") {
			fmt.Fprintf(w, "\n=== KEEP ===\n%s\n", ev.summary)
		}
	}
	empty := p.emptyDirsAfter(evs)
	for _, d := range empty {
		fmt.Fprintf(w, "\n=== REMOVE %s ===\nempty directory only orch used\n", d)
	}
	if changed == 0 && len(steps) == 0 && len(empty) == 0 {
		fmt.Fprintf(w, "\nNothing of orch's left to remove for %q.\n", p.Target)
	} else {
		fmt.Fprintf(w, "\n%d file(s) would change and %d step(s) would run. Re-run with --apply to do it (originals backed up first).\n", changed, len(steps))
	}
	p.printNotes(w)
}

// emptyDirsAfter lists the prune directories the plan would leave empty,
// counting the files it removes.
func (p Plan) emptyDirsAfter(evs []evaluated) []string {
	gone := map[string]bool{}
	for i, e := range p.Edits {
		if evs[i].remove {
			gone[filepath.Clean(e.Path)] = true
		}
	}
	var out []string
	for _, d := range p.Prune {
		if !isRealDir(d) {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		left := 0
		for _, en := range entries {
			child := filepath.Join(d, en.Name())
			if !gone[child] {
				left++
			}
		}
		if left == 0 {
			out = append(out, d)
			gone[d] = true
		}
	}
	return out
}

// applyUninstall runs the undo steps, then each edit's Reverse, backing up
// every file before it changes or goes, then removes the empty directories.
func (p Plan) applyUninstall(ctx context.Context, w io.Writer, backupDir string, evs []evaluated) error {
	ran, err := p.runSteps(ctx, w)
	if err != nil {
		return err
	}
	// Reason: the steps can change files the edits then read (claude
	// plugin uninstall edits settings.json), so the edits are measured
	// again from what the steps left.
	if ran > 0 {
		if evs, err = p.evaluateAll(); err != nil {
			return err
		}
	}
	dest := filepath.Join(backupDir, time.Now().UTC().Format("20060102-150405"))
	changed := 0
	for i, e := range p.Edits {
		ev := evs[i]
		if !ev.changed {
			continue
		}
		current, err := os.ReadFile(e.Path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if !bytes.Equal(current, ev.old) {
			return fmt.Errorf("%s: changed on disk since the plan was computed — re-run to apply against current state", e.Path)
		}
		if len(ev.old) > 0 {
			if err := backup(dest, e.Path, ev.old); err != nil {
				return fmt.Errorf("backup %s: %w", e.Path, err)
			}
		}
		changed++
		if ev.remove {
			if err := removeRegular(e.Path); err != nil {
				return err
			}
			fmt.Fprintf(w, "removed %s\n  %s\n", e.Path, ev.summary)
			continue
		}
		if err := os.WriteFile(e.Path, ev.next, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(w, "wrote %s\n", e.Path)
		for _, line := range strings.Split(ev.summary, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	pruned := 0
	for _, d := range p.Prune {
		if isRealDir(d) && os.Remove(d) == nil {
			pruned++
			fmt.Fprintf(w, "removed empty directory %s\n", d)
		}
	}
	if changed > 0 {
		fmt.Fprintf(w, "%d file(s) changed or removed; backups in %s\n", changed, dest)
	}
	if changed == 0 && ran == 0 && pruned == 0 {
		fmt.Fprintf(w, "nothing of orch's left to remove for %q — nothing changed.\n", p.Target)
	}
	p.printNotes(w)
	return nil
}

// removeRegular deletes path only while it is a regular file. Reason: the
// check is made again at the moment of removal, so a file swapped for a
// link or a directory since the plan is refused rather than followed;
// os.Remove itself never follows a final symlink.
func removeRegular(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: no longer a regular file — refusing to remove it", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return nil
}

// isRealDir reports whether p is a directory and not a link to one.
func isRealDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

// printNotes lists what the plan leaves for the user to do by hand.
func (p Plan) printNotes(w io.Writer) {
	if p.Notes == nil {
		return
	}
	notes := p.Notes()
	if len(notes) == 0 {
		return
	}
	title := "Left for you to do by hand:"
	if !p.Uninstall {
		title = "Manual steps orch does not do for you:"
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, n := range notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

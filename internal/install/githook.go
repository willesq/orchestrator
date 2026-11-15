package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Git hooks for outbound terms (spaces plan SP-045). `orch guard git-hook
// --install` puts a commit-msg and a pre-push hook in a repository's hooks
// directory (core.hooksPath when set) that run `orch guard git-hook run`.
// Like the harness install it is dry-run by default, idempotent, and backs
// up every file it changes. A hook already there is chained, not replaced:
// it moves to <name>.orch-prev, and orch's hook runs it after its own check
// passes, with the same arguments and input. Uninstall puts it back.

// GitHookNames are the hooks orch installs, in order.
var GitHookNames = []string{"commit-msg", "pre-push"}

// gitHookMarker marks a hook file as orch's.
const gitHookMarker = "# orch-git-hook: managed by `orch guard git-hook` (spaces SP-045)"

// GitHookPrevSuffix names the hook orch's hook chains to.
const GitHookPrevSuffix = ".orch-prev"

// IsOrchGitHook reports whether a hook file's content is orch's.
func IsOrchGitHook(content []byte) bool {
	return bytes.Contains(content, []byte(gitHookMarker))
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// GitHookScript is orch's hook named name, which runs orchBin.
func GitHookScript(name, orchBin string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n%s\n", gitHookMarker)
	b.WriteString("# It checks what git is about to send for other spaces' outbound terms and\n")
	b.WriteString("# runs the hook it replaced (" + name + GitHookPrevSuffix + "), if any.\n")
	b.WriteString("# Remove it with `orch guard git-hook --uninstall --apply`.\n")
	fmt.Fprintf(&b, "orch=%s\n", shQuote(orchBin))
	// Reason: orch may have moved since the install; one found on PATH
	// still checks. With none the commit or push is refused (fails
	// closed), and the message says how to remove the hook.
	b.WriteString(`if [ ! -x "$orch" ]; then
	orch=$(command -v orch 2>/dev/null) || orch=
fi
if [ -z "$orch" ]; then
	echo "orch git hook: orch was not found, so this ` + name + ` is refused. Reinstall orch, or remove the hook: mv \"$0` + GitHookPrevSuffix + `\" \"$0\" (or rm \"$0\" when there is none)." >&2
	exit 1
fi
`)
	fmt.Fprintf(&b, "prev=\"$0%s\"\n", GitHookPrevSuffix)
	if name == "pre-push" {
		// Reason: pre-push reads the refs it pushes on its input, which
		// both orch and the chained hook must see.
		b.WriteString(`input=$(cat)
if [ -n "$input" ]; then printf '%s\n' "$input"; fi | "$orch" guard git-hook run pre-push -- "$@" || exit 1
if [ -x "$prev" ]; then
	if [ -n "$input" ]; then printf '%s\n' "$input"; fi | "$prev" "$@"
	exit $?
fi
exit 0
`)
		return []byte(b.String())
	}
	// Reason: the chained commit-msg hook runs first, since it may add to
	// the message (a Change-Id, a ticket prefix); orch then checks the
	// message as it will be committed.
	b.WriteString(`if [ -x "$prev" ]; then
	"$prev" "$@" || exit $?
fi
`)
	fmt.Fprintf(&b, "exec \"$orch\" guard git-hook run %s -- \"$@\"\n", name)
	return []byte(b.String())
}

// gitHookAction is one change the plan makes to one hook.
type gitHookAction struct {
	name string
	path string
	// old is the hook's content when planned (nil: none); oldMode its mode.
	old     []byte
	oldMode os.FileMode
	// next is the content to write; nil removes orch's hook.
	next []byte
	// chain moves the existing hook to <path>.orch-prev before writing;
	// restore moves <path>.orch-prev back after removing.
	chain, restore bool
	summary        string
	changed        bool
}

// GitHookPlan is the set of hook changes for one hooks directory.
type GitHookPlan struct {
	Dir       string
	Uninstall bool
	actions   []gitHookAction
}

// maxHookFile bounds what is read of a hook. Reason: a hook is a short
// script; anything larger is not one orch should move or back up.
const maxHookFile = 1 << 20

// lstatHook reads the hook at path: its content (nil when there is none;
// for a symlink, the link's target path, never what it points at) and
// mode. Anything but a regular file or a symlink is an error.
func lstatHook(path string) ([]byte, os.FileMode, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		// Reason: a link is chained as it is (moved, never followed), so
		// its target is neither read nor changed.
		target, err := os.Readlink(path)
		return []byte(target), fi.Mode(), true, err
	case !fi.Mode().IsRegular():
		return nil, 0, false, fmt.Errorf("%s is not a regular file or a symlink", path)
	case fi.Size() > maxHookFile:
		return nil, 0, false, fmt.Errorf("%s is over %d bytes", path, maxHookFile)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxHookFile+1))
	if err == nil && len(b) > maxHookFile {
		err = fmt.Errorf("%s is over %d bytes", path, maxHookFile)
	}
	return b, fi.Mode(), true, err
}

// PlanGitHooks plans installing (or, with uninstall, removing) orch's hooks
// in hooksDir, writing nothing. orchBin is the orch the hooks run.
func PlanGitHooks(hooksDir, orchBin string, uninstall bool) (GitHookPlan, error) {
	p := GitHookPlan{Dir: hooksDir, Uninstall: uninstall}
	if !filepath.IsAbs(hooksDir) {
		return p, fmt.Errorf("hooks directory %q is not absolute", hooksDir)
	}
	if !uninstall && !filepath.IsAbs(orchBin) {
		return p, fmt.Errorf("orch binary %q is not absolute", orchBin)
	}
	for _, name := range GitHookNames {
		path := filepath.Join(hooksDir, name)
		old, mode, exists, err := lstatHook(path)
		if err != nil {
			return p, err
		}
		_, _, prevExists, err := lstatHook(path + GitHookPrevSuffix)
		if err != nil {
			return p, err
		}
		a := gitHookAction{name: name, path: path, old: old, oldMode: mode}
		ours := exists && mode&os.ModeSymlink == 0 && IsOrchGitHook(old)
		if uninstall {
			switch {
			case ours:
				a.changed, a.restore = true, prevExists
				a.summary = "remove orch's " + name + " hook"
				if prevExists {
					a.summary += " and put back the hook it chained (" + name + GitHookPrevSuffix + ")"
				}
			case exists:
				a.summary = name + " is not orch's; left alone"
			default:
				a.summary = "no " + name + " hook"
			}
			p.actions = append(p.actions, a)
			continue
		}
		a.next = GitHookScript(name, orchBin)
		switch {
		case ours && bytes.Equal(old, a.next) && mode.Perm()&0o111 != 0:
			a.summary = "orch's " + name + " hook is installed"
		case ours:
			a.changed, a.summary = true, "update orch's "+name+" hook"
		case exists && prevExists:
			// Reason: moving this hook would overwrite the one already
			// chained; neither may be lost.
			return p, fmt.Errorf("%s exists and so does %s%s; move one of them away first", path, name, GitHookPrevSuffix)
		case exists:
			a.changed, a.chain = true, true
			a.summary = "add orch's " + name + " hook; the existing hook moves to " + name + GitHookPrevSuffix + " and still runs"
		case prevExists:
			// Reason: a chained hook left behind (orch's hook removed by
			// hand) starts running again; the plan says so.
			a.changed = true
			a.summary = "add orch's " + name + " hook; it will also run the " + name + GitHookPrevSuffix + " already there"
		default:
			a.changed, a.summary = true, "add orch's "+name+" hook"
		}
		p.actions = append(p.actions, a)
	}
	return p, nil
}

// Changes counts the hooks the plan would change.
func (p GitHookPlan) Changes() int {
	n := 0
	for _, a := range p.actions {
		if a.changed {
			n++
		}
	}
	return n
}

// Print shows the plan, writing nothing.
func (p GitHookPlan) Print(w io.Writer) {
	verb := "install"
	if p.Uninstall {
		verb = "uninstall"
	}
	fmt.Fprintf(w, "DRY RUN — %s orch's git hooks in %s. No files will be written.\n", verb, p.Dir)
	for _, a := range p.actions {
		state := "UNCHANGED"
		if a.changed {
			state = "CHANGE"
		}
		fmt.Fprintf(w, "  %-9s %s: %s\n", state, a.name, a.summary)
	}
	if n := p.Changes(); n == 0 {
		fmt.Fprintln(w, "Nothing to do.")
	} else {
		fmt.Fprintf(w, "%d hook(s) would change. Re-run with --apply to do it (originals backed up first).\n", n)
	}
}

// Apply makes the plan's changes, backing up each hook it changes into a
// timestamped directory under backupDir. A hook changed since the plan was
// made is not touched.
func (p GitHookPlan) Apply(w io.Writer, backupDir string) error {
	dest := filepath.Join(backupDir, time.Now().UTC().Format("20060102-150405"))
	changed := 0
	for _, a := range p.actions {
		if !a.changed {
			fmt.Fprintf(w, "unchanged %s\n", a.path)
			continue
		}
		cur, mode, _, err := lstatHook(a.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(cur, a.old) || mode != a.oldMode {
			return fmt.Errorf("%s: changed on disk since the plan was computed — re-run to apply against current state", a.path)
		}
		// Reason: a symlink is moved, never removed, so nothing is lost.
		if a.old != nil && a.oldMode&os.ModeSymlink == 0 {
			if err := backup(dest, a.path, a.old); err != nil {
				return fmt.Errorf("backup %s: %w", a.path, err)
			}
		}
		if err := a.apply(); err != nil {
			return err
		}
		changed++
		fmt.Fprintf(w, "%s: %s\n", a.path, a.summary)
	}
	if changed > 0 {
		fmt.Fprintf(w, "%d hook(s) changed; backups in %s\n", changed, dest)
	} else {
		fmt.Fprintln(w, "nothing changed.")
	}
	return nil
}

// apply makes one action's change.
func (a gitHookAction) apply() error {
	prev := a.path + GitHookPrevSuffix
	if a.next == nil {
		if err := os.Remove(a.path); err != nil {
			return err
		}
		if a.restore {
			return renameNoReplace(prev, a.path)
		}
		return nil
	}
	if a.chain {
		// Reason: a link is renamed, not copied, so the hook it points at
		// stays where it is and keeps working; a chained hook that appeared
		// since the plan is never replaced.
		if err := renameNoReplace(a.path, prev); err != nil {
			return fmt.Errorf("move %s to %s: %w", a.path, prev, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	// Reason: written beside the hook and renamed over it, so git never
	// runs a half-written hook.
	tmp, err := os.CreateTemp(filepath.Dir(a.path), "."+a.name+".orch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(a.next); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.path)
}

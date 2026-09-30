package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeBin writes a stand-in binary and stamps a deterministic mtime, so a
// rewrite is distinguishable even when two writes land in the same filesystem
// timestamp tick.
func writeBin(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestInspect(t *testing.T) {
	dir := t.TempDir()
	mod := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	real := filepath.Join(dir, "cellar", "0.4.1", "orchd")
	writeBin(t, real, "binary", mod)
	link := filepath.Join(dir, "opt", "orchd")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	writeBin(t, empty, "", mod)

	tests := []struct {
		name     string
		path     string
		wantPath string
		wantErr  string
	}{
		{name: "a regular binary", path: real, wantPath: real},
		{name: "a symlink resolves to its target", path: link, wantPath: real},
		{name: "a missing path errors", path: filepath.Join(dir, "gone"), wantErr: "no such file"},
		{name: "a directory is not a binary", path: dir, wantErr: "not a regular file"},
		{name: "an empty file is a half-written binary", path: empty, wantErr: "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := Inspect(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Inspect(%s) error = %v, want one containing %q", tt.path, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Inspect(%s): %v", tt.path, err)
			}
			// EvalSymlinks also resolves the temp dir itself (/var -> /private/var
			// on macOS), so compare against the resolved want.
			want, _ := filepath.EvalSymlinks(tt.wantPath)
			if b.Path != want {
				t.Errorf("Path = %q, want %q", b.Path, want)
			}
			if b.Size != int64(len("binary")) || !b.ModTime.Equal(mod) {
				t.Errorf("build = %+v, want size 6 and mtime %s", b, mod)
			}
		})
	}
}

// TestWatcherChanged covers the two shapes an upgrade takes — a relinked symlink
// (Homebrew) and an overwritten binary at the same path (a source build) — plus
// the unreadable-path case an upgrade passes through between the two.
func TestWatcherChanged(t *testing.T) {
	mod := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	newer := mod.Add(time.Hour)

	tests := []struct {
		name        string
		upgrade     func(t *testing.T, dir, link string)
		wantChanged bool
	}{
		{
			name:        "nothing touched the binary",
			upgrade:     func(*testing.T, string, string) {},
			wantChanged: false,
		},
		{
			name: "the launch path is relinked to a new version",
			upgrade: func(t *testing.T, dir, link string) {
				next := filepath.Join(dir, "cellar", "0.4.3", "orchd")
				writeBin(t, next, "binary", mod) // same size and mtime, new path
				if err := os.Remove(link); err != nil {
					t.Fatalf("unlink: %v", err)
				}
				if err := os.Symlink(next, link); err != nil {
					t.Fatalf("relink: %v", err)
				}
			},
			wantChanged: true,
		},
		{
			name: "the binary is rebuilt in place",
			upgrade: func(t *testing.T, dir, _ string) {
				writeBin(t, filepath.Join(dir, "cellar", "0.4.1", "orchd"), "binary", newer)
			},
			wantChanged: true,
		},
		{
			name: "an upgrade that has unlinked but not relinked is not a change",
			upgrade: func(t *testing.T, _, link string) {
				if err := os.Remove(link); err != nil {
					t.Fatalf("unlink: %v", err)
				}
			},
			wantChanged: false,
		},
		{
			name: "a half-written binary is not a change",
			upgrade: func(t *testing.T, dir, _ string) {
				writeBin(t, filepath.Join(dir, "cellar", "0.4.1", "orchd"), "", newer)
			},
			wantChanged: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			real := filepath.Join(dir, "cellar", "0.4.1", "orchd")
			writeBin(t, real, "binary", mod)
			link := filepath.Join(dir, "orchd")
			if err := os.Symlink(real, link); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			w, err := NewWatcher(link)
			if err != nil {
				t.Fatalf("NewWatcher: %v", err)
			}
			if w.Path() != link {
				t.Errorf("Path = %q, want the launch path %q", w.Path(), link)
			}
			if _, changed := w.Changed(); changed {
				t.Fatal("Changed reported drift before anything was upgraded")
			}

			tt.upgrade(t, dir, link)
			b, changed := w.Changed()
			if changed != tt.wantChanged {
				t.Fatalf("Changed = %v (%s), want %v", changed, b, tt.wantChanged)
			}
			if !changed {
				return
			}
			// Adopting the new build silences the repeat report, which is what
			// keeps an unsupervised daemon from logging the same drift forever.
			w.Adopt(b)
			if _, again := w.Changed(); again {
				t.Error("Changed still reported drift after the build was adopted")
			}
		})
	}
}

func TestNewWatcherRejectsAnUnreadablePath(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "a launch path that does not exist yields no watcher"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewWatcher(filepath.Join(t.TempDir(), "gone")); err == nil {
				t.Fatal("NewWatcher succeeded on a missing path")
			}
		})
	}
}

func TestBuildString(t *testing.T) {
	tests := []struct {
		name  string
		build Build
		want  []string
	}{
		{
			name: "names the resolved path, which carries the version",
			build: Build{
				Path:    "/opt/homebrew/Cellar/orch/0.4.3/bin/orchd",
				Size:    42,
				ModTime: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
			},
			want: []string{"/opt/homebrew/Cellar/orch/0.4.3/bin/orchd", "42 bytes", "2026-09-30T12:00:00Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.build.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("String() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

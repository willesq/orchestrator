package settings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
)

// authProvider owns one file listing the enabled items, one per line. Items
// a, b and c always exist and default on; a file containing BROKEN cannot be
// rewritten.
type authProvider struct{ path string }

func (*authProvider) Name() string  { return "auth" }
func (*authProvider) Title() string { return "Auth" }
func (*authProvider) Kinds() []KindSpec {
	return []KindSpec{{Name: "m", Title: "Modules", StateInFile: true}}
}
func (p *authProvider) Read() (map[string][]byte, error) {
	b, err := ReadFile(p.path)
	return map[string][]byte{p.path: b}, err
}
func (*authProvider) Observe(map[string][]byte) ([]Item, error) {
	var out []Item
	for _, id := range []string{"a", "b", "c"} {
		out = append(out, Item{Kind: "m", Identity: id, Enabled: true, Origin: OriginOrch, Payload: json.RawMessage(`{}`)})
	}
	return out, nil
}
func (p *authProvider) Enforce(files map[string][]byte, enabled func(kind, identity string) (bool, bool)) (Changes, error) {
	if strings.Contains(string(files[p.path]), "BROKEN") {
		return Changes{}, errors.New("broken file")
	}
	var on []string
	for _, id := range []string{"a", "b", "c"} {
		if v, known := enabled("m", id); !known || v {
			on = append(on, id)
		}
	}
	sort.Strings(on)
	want := strings.Join(on, "\n") + "\n"
	if string(files[p.path]) == want {
		return Changes{}, nil
	}
	return Changes{Write: map[string][]byte{p.path: []byte(want)}}, nil
}
func (*authProvider) Apply(map[string][]byte, Item, bool) (map[string][]byte, error) {
	return nil, errors.New("Apply must not run for an authoritative provider")
}
func (*authProvider) Validate(string, map[string]string) (Item, error)  { return Item{}, ErrInvalid }
func (*authProvider) Describe(string, json.RawMessage) (string, string) { return "", "" }

func TestAuthoritativeProviderStoreWins(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := &authProvider{path: filepath.Join(t.TempDir(), "auth.txt")}
	reg, _ := NewRegistry(p)
	var backups []string
	svc := NewService(NewStore(conn), reg, func(path string, _ []byte) error {
		backups = append(backups, path)
		return nil
	})
	ctx := context.Background()
	read := func() string {
		b, _ := os.ReadFile(p.path)
		return string(b)
	}

	svc.Sync(ctx)
	if read() != "a\nb\nc\n" {
		t.Fatalf("first sync should write the defaults, got %q", read())
	}
	r, err := svc.SetEnabled(ctx, "auth", "m", "b", false)
	if err != nil || r.Enabled || r.Status() != StatusDisabled {
		t.Fatalf("SetEnabled = %+v, %v", r, err)
	}
	if read() != "a\nc\n" {
		t.Errorf("toggle off not written: %q", read())
	}
	if len(backups) != 1 {
		t.Errorf("backups = %v, want one before the first change", backups)
	}

	// A hand edit is reverted, and does not change the stored choice.
	if err := os.WriteFile(p.path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Sync(ctx)
	if read() != "a\nc\n" {
		t.Errorf("hand edit not reverted: %q", read())
	}
	if got, _ := svc.store.Get(ctx, "auth", "m", "b"); got.Enabled {
		t.Error("import took the file's state over the stored choice")
	}

	// A file Enforce must leave alone is the provider's error, but does not
	// block the choice: it is recorded, and the file converges once fixed.
	if err := os.WriteFile(p.path, []byte("BROKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Sync(ctx)
	l, err := svc.List(ctx, "auth", "")
	if err != nil {
		t.Fatal(err)
	}
	if st := l.Providers["auth"]; !strings.Contains(st.Error, "broken file") {
		t.Errorf("provider state = %+v", st)
	}
	if _, err := svc.SetEnabled(ctx, "auth", "m", "b", true); err != nil {
		t.Fatalf("a problem file blocked the toggle: %v", err)
	}
	if got, _ := svc.store.Get(ctx, "auth", "m", "b"); !got.Enabled {
		t.Error("the choice was not recorded")
	}
	if read() != "BROKEN\n" {
		t.Errorf("a file Enforce refused was written: %q", read())
	}
	if err := os.WriteFile(p.path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Sync(ctx)
	if l, _ := svc.List(ctx, "auth", ""); l.Providers["auth"].Error != "" {
		t.Errorf("error not cleared once fixed: %q", l.Providers["auth"].Error)
	}
	if read() != "a\nb\nc\n" {
		t.Errorf("toggle on not written: %q", read())
	}
}

func TestDeleteFilesGuards(t *testing.T) {
	dir := t.TempDir()
	read := filepath.Join(dir, "skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(read), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(read, []byte("orch's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var backups []string
	s := NewService(nil, nil, func(path string, _ []byte) error { backups = append(backups, path); return nil })
	p := &authProvider{path: read}
	files := map[string][]byte{read: []byte("orch's\n")}

	if err := s.deleteFiles(p, files, Changes{Delete: []string{filepath.Join(dir, "other")}}); err == nil {
		t.Error("deleted a path the provider never read")
	}
	if err := s.deleteFiles(p, files, Changes{Prune: []string{t.TempDir()}}); err == nil {
		t.Error("pruned a directory holding no file the provider read")
	}
	// Changed underneath since it was read: refused, file kept.
	if err := os.WriteFile(read, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteFiles(p, files, Changes{Delete: []string{read}}); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting a changed file = %v, want a conflict", err)
	}
	if _, err := os.Stat(read); err != nil {
		t.Fatal("a changed file was removed")
	}
	// A symlink is never deleted through.
	target := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(target, []byte("orch's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "skill", "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteFiles(p, map[string][]byte{link: []byte("orch's\n")}, Changes{Delete: []string{link}}); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting through a symlink = %v, want a conflict", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "orch's\n" {
		t.Error("the link's target changed")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	// The happy path: backed up, removed, emptied directory pruned; a nil
	// (missing) entry is skipped.
	if err := os.WriteFile(read, []byte("orch's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "skill", "gone.md")
	files[missing] = nil
	if err := s.deleteFiles(p, files, Changes{Delete: []string{missing, read}, Prune: []string{filepath.Dir(read)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(read)); !os.IsNotExist(err) {
		t.Errorf("emptied directory kept: %v", err)
	}
	// Each file is backed up once, before orch's first attempt to change it,
	// so the refused attempts above already saved both.
	if len(backups) != 2 || backups[0] != read || backups[1] != link {
		t.Errorf("backups = %v", backups)
	}
}

func TestRemoveEmptyDir(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	full := filepath.Join(dir, "full")
	target := filepath.Join(dir, "target")
	for _, d := range []string{empty, full, target} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !removeEmptyDir(empty) {
		t.Error("an empty directory was kept")
	}
	if removeEmptyDir(full) || removeEmptyDir(filepath.Join(dir, "missing")) {
		t.Error("a non-empty or missing directory was reported removed")
	}
	if removeEmptyDir(link) {
		t.Error("a symlink to a directory was removed")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("the symlink is gone")
	}
}

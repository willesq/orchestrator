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
func (p *authProvider) Enforce(files map[string][]byte, enabled func(kind, identity string) (bool, bool)) (map[string][]byte, error) {
	if strings.Contains(string(files[p.path]), "BROKEN") {
		return nil, errors.New("broken file")
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
		return nil, nil
	}
	return map[string][]byte{p.path: []byte(want)}, nil
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

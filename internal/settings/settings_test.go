package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
)

// fakeProvider keeps items in one JSON file: {"<kind>/<id>": {"on", "orch", "v"}}.
// Kind "hook" leaves the file when off; kind "plugin" records off in it.
type fakeProvider struct {
	path string
	// writeElsewhere makes Apply return a path it did not read.
	writeElsewhere bool
}

type fakeEntry struct {
	On   bool   `json:"on"`
	Orch bool   `json:"orch,omitempty"`
	V    string `json:"v"`
}

func (*fakeProvider) Name() string  { return "fake" }
func (*fakeProvider) Title() string { return "Fake" }
func (*fakeProvider) Kinds() []KindSpec {
	return []KindSpec{
		{Name: "hook", Title: "Hooks", Addable: true, Removable: true,
			Fields: []FieldSpec{{Name: "v", Label: "Value", Type: "string", Required: true}}},
		{Name: "plugin", Title: "Plugins", StateInFile: true},
	}
}

func (p *fakeProvider) Read() (map[string][]byte, error) {
	b, err := ReadFile(p.path)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{p.path: b}, nil
}

func (p *fakeProvider) parse(b []byte) (map[string]fakeEntry, error) {
	m := map[string]fakeEntry{}
	if len(b) == 0 {
		return m, nil
	}
	return m, json.Unmarshal(b, &m)
}

func (p *fakeProvider) Observe(files map[string][]byte) ([]Item, error) {
	m, err := p.parse(files[p.path])
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Item
	for _, k := range keys {
		kind, id, _ := strings.Cut(k, "/")
		e := m[k]
		origin := OriginFound
		if e.Orch {
			origin = OriginOrch
		}
		on := e.On || kind == "hook"
		out = append(out, Item{Kind: kind, Identity: id, Enabled: on, Origin: origin,
			Payload: json.RawMessage(`{"v":"` + e.V + `"}`)})
	}
	return out, nil
}

func (p *fakeProvider) Apply(files map[string][]byte, it Item, enabled bool) (map[string][]byte, error) {
	m, err := p.parse(files[p.path])
	if err != nil {
		return nil, err
	}
	var pl struct{ V string }
	_ = json.Unmarshal(it.Payload, &pl)
	key := it.Kind + "/" + it.Identity
	switch {
	case it.Kind == "plugin":
		e := m[key]
		e.On = enabled
		m[key] = e
	case enabled:
		m[key] = fakeEntry{On: true, Orch: it.Origin == OriginOrch, V: pl.V}
	default:
		delete(m, key)
	}
	b, _ := json.Marshal(m)
	path := p.path
	if p.writeElsewhere {
		path += ".other"
	}
	return map[string][]byte{path: b}, nil
}

func (*fakeProvider) Validate(kind string, f map[string]string) (Item, error) {
	if f["v"] == "" {
		return Item{}, Invalidf("v is required")
	}
	return Item{Kind: kind, Identity: "new-" + f["v"], Payload: json.RawMessage(`{"v":"` + f["v"] + `"}`), Origin: OriginYou}, nil
}

func (*fakeProvider) Describe(kind string, payload json.RawMessage) (string, string) {
	return kind, string(payload)
}

type harness struct {
	t       *testing.T
	svc     *Service
	p       *fakeProvider
	db      *sql.DB
	backups []string
}

func newHarness(t *testing.T, initial string) *harness {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	h := &harness{t: t, db: conn, p: &fakeProvider{path: filepath.Join(t.TempDir(), "fake.json")}}
	if initial != "" {
		h.write(initial)
	}
	reg, err := NewRegistry(h.p)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = NewService(NewStore(conn), reg, func(path string, _ []byte) error {
		h.backups = append(h.backups, path)
		return nil
	})
	return h
}

func (h *harness) write(s string) {
	h.t.Helper()
	if err := os.WriteFile(h.p.path, []byte(s), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) file() map[string]fakeEntry {
	h.t.Helper()
	b, err := os.ReadFile(h.p.path)
	if err != nil {
		h.t.Fatal(err)
	}
	m, err := h.p.parse(b)
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

func (h *harness) rows() map[string]Row {
	h.t.Helper()
	l, err := h.svc.List(context.Background(), "", "")
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]Row{}
	for _, r := range l.Rows {
		out[r.Kind+"/"+r.Identity] = r
	}
	return out
}

const seed = `{"hook/a":{"on":true,"v":"one"},"hook/o":{"on":true,"orch":true,"v":"orch"},"plugin/p":{"on":true,"v":"pl"}}`

func TestSyncImportsAndSkipsUnchanged(t *testing.T) {
	h := newHarness(t, seed)
	ctx := context.Background()
	h.svc.Sync(ctx)
	l, err := h.svc.List(ctx, "fake", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rows) != 3 {
		t.Fatalf("rows = %+v", l.Rows)
	}
	got := h.rows()
	if got["hook/a"].Origin != OriginFound || got["hook/o"].Origin != OriginOrch || got["plugin/p"].Status() != StatusActive {
		t.Errorf("rows = %+v", got)
	}
	if st := l.Providers["fake"]; st.Error != "" || len(st.Files) != 1 || st.ObservedAt.IsZero() {
		t.Errorf("provider state = %+v", st)
	}
	rev := l.Revision
	h.svc.Sync(ctx)
	if l2, _ := h.svc.List(ctx, "", "hook"); l2.Revision != rev || len(l2.Rows) != 2 {
		t.Errorf("unchanged files changed the listing: rev %d -> %d, rows %d", rev, l2.Revision, len(l2.Rows))
	}
	if _, err := h.svc.List(ctx, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown provider: %v", err)
	}
}

func TestToggleRemovesAndRestoresExactly(t *testing.T) {
	h := newHarness(t, seed)
	ctx := context.Background()
	h.svc.Sync(ctx)
	r, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status() != StatusDisabled || r.Enabled {
		t.Errorf("row = %+v", r)
	}
	if _, ok := h.file()["hook/a"]; ok {
		t.Error("disabled hook still in the file")
	}
	// A second toggle to the same state writes nothing.
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false); err != nil {
		t.Fatal(err)
	}
	r, err = h.svc.SetEnabled(ctx, "fake", "hook", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status() != StatusActive || h.file()["hook/a"].V != "one" {
		t.Errorf("not restored: row %+v file %+v", r, h.file())
	}
	if len(h.backups) != 1 {
		t.Errorf("backups = %v, want one before the first change only", h.backups)
	}
	// A plugin is recorded off in the file.
	if r, err = h.svc.SetEnabled(ctx, "fake", "plugin", "p", false); err != nil || r.Status() != StatusDisabled {
		t.Fatalf("plugin off: %+v %v", r, err)
	}
	if e := h.file()["plugin/p"]; e.On {
		t.Errorf("plugin still on in the file: %+v", e)
	}
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "zzz", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown item: %v", err)
	}
	if _, err := h.svc.SetEnabled(ctx, "fake", "nokind", "a", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := h.svc.SetEnabled(ctx, "nope", "hook", "a", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown provider: %v", err)
	}
}

func TestExternalEdits(t *testing.T) {
	h := newHarness(t, seed)
	ctx := context.Background()
	h.svc.Sync(ctx)
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false); err != nil {
		t.Fatal(err)
	}
	// Outside orch: the disabled hook comes back, the orch hook and the
	// plugin vanish, and a new hook appears.
	h.write(`{"hook/a":{"on":true,"v":"one"},"hook/n":{"on":true,"v":"new"}}`)
	got := h.rows()
	if got["hook/a"].Status() != StatusActive {
		t.Errorf("re-added hook = %s, want active (the file wins)", got["hook/a"].Status())
	}
	if got["hook/n"].Status() != StatusActive || got["hook/n"].Origin != OriginFound {
		t.Errorf("new hook = %+v", got["hook/n"])
	}
	if got["hook/o"].Status() != StatusMissing || !got["hook/o"].Enabled {
		t.Errorf("orch hook = %s, want missing and still enabled", got["hook/o"].Status())
	}
	if got["plugin/p"].Status() != StatusRemoved {
		t.Errorf("plugin = %s, want removed", got["plugin/p"].Status())
	}
	// A found hook removed outside orch is marked removed, not put back.
	h.write(`{"hook/a":{"on":true,"v":"one"}}`)
	got = h.rows()
	if got["hook/n"].Status() != StatusRemoved || got["hook/n"].Enabled {
		t.Errorf("removed hook = %+v", got["hook/n"])
	}
	if _, ok := h.file()["hook/n"]; ok {
		t.Error("a removed hook was put back")
	}
	// Turning a removed hook on restores it; a removed plugin cannot be toggled.
	if r, err := h.svc.SetEnabled(ctx, "fake", "hook", "n", true); err != nil || r.Status() != StatusActive {
		t.Fatalf("restore: %+v %v", r, err)
	}
	if h.file()["hook/n"].V != "new" {
		t.Errorf("file = %+v", h.file())
	}
	if _, err := h.svc.SetEnabled(ctx, "fake", "plugin", "p", true); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("removed plugin: %v", err)
	}
	// A vanished plugin can be forgotten, though plugins are not removable.
	if err := h.svc.Remove(ctx, "fake", "plugin", "p"); err != nil {
		t.Errorf("forget a removed plugin: %v", err)
	}
	if _, ok := h.rows()["plugin/p"]; ok {
		t.Error("removed plugin still listed")
	}
	// The missing orch hook can be restored from orch too.
	if r, err := h.svc.SetEnabled(ctx, "fake", "hook", "o", true); err != nil || r.Status() != StatusActive {
		t.Errorf("restore orch hook: %+v %v", r, err)
	}
}

func TestAddAndRemove(t *testing.T) {
	h := newHarness(t, seed)
	ctx := context.Background()
	r, err := h.svc.Add(ctx, "fake", "hook", map[string]string{"v": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Origin != OriginYou || r.Status() != StatusActive || h.file()["hook/new-x"].V != "x" {
		t.Errorf("added %+v, file %+v", r, h.file())
	}
	if _, err := h.svc.Add(ctx, "fake", "hook", map[string]string{"v": "x"}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "new-x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Add(ctx, "fake", "hook", map[string]string{"v": "x"}); !errors.Is(err, ErrAlreadyExists) ||
		!strings.Contains(err.Error(), "turn it on") {
		t.Errorf("duplicate of a disabled item: %v", err)
	}
	if _, err := h.svc.Add(ctx, "fake", "hook", map[string]string{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid: %v", err)
	}
	if _, err := h.svc.Add(ctx, "fake", "plugin", map[string]string{"v": "x"}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("not addable: %v", err)
	}
	if err := h.svc.Remove(ctx, "fake", "hook", "o"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("orch item removed: %v", err)
	}
	if err := h.svc.Remove(ctx, "fake", "plugin", "p"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("plugin removed: %v", err)
	}
	if err := h.svc.Remove(ctx, "fake", "hook", "zzz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	// Removing a disabled item only forgets it; removing a present one also
	// takes it out of the file.
	if err := h.svc.Remove(ctx, "fake", "hook", "new-x"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Remove(ctx, "fake", "hook", "a"); err != nil {
		t.Fatal(err)
	}
	got := h.rows()
	if _, ok := got["hook/new-x"]; ok {
		t.Error("removed item still listed")
	}
	if _, ok := got["hook/a"]; ok {
		t.Error("removed found item still listed")
	}
	if _, ok := h.file()["hook/a"]; ok {
		t.Error("removed item still in the file")
	}
	if err := h.svc.Remove(ctx, "fake", "nokind", "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown kind: %v", err)
	}
}

func TestUnreadableAndStrayWrites(t *testing.T) {
	h := newHarness(t, `{not json`)
	ctx := context.Background()
	l, err := h.svc.List(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if l.Providers["fake"].Error == "" {
		t.Error("parse error not reported")
	}
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false); !errors.Is(err, ErrUnreadable) {
		t.Errorf("change against an unreadable file: %v", err)
	}
	if _, err := h.svc.Add(ctx, "fake", "hook", map[string]string{"v": "x"}); !errors.Is(err, ErrUnreadable) {
		t.Errorf("add against an unreadable file: %v", err)
	}
	if err := h.svc.Remove(ctx, "fake", "hook", "a"); !errors.Is(err, ErrUnreadable) {
		t.Errorf("remove against an unreadable file: %v", err)
	}
	h.write(seed)
	h.p.writeElsewhere = true
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false); err == nil || !strings.Contains(err.Error(), "did not read") {
		t.Errorf("stray write: %v", err)
	}
	if _, err := os.Stat(h.p.path + ".other"); !os.IsNotExist(err) {
		t.Error("a stray path was written")
	}
	// A provider whose file is a directory reports a read error.
	h.p.writeElsewhere = false
	if err := os.Remove(h.p.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.p.path, 0o755); err != nil {
		t.Fatal(err)
	}
	if l, _ := h.svc.List(ctx, "", ""); !strings.Contains(l.Providers["fake"].Error, "not a regular file") {
		t.Errorf("read error = %q", l.Providers["fake"].Error)
	}
}

func TestTooManyItems(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i <= MaxItems; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"hook/h%d":{"on":true,"v":"x"}`, i)
	}
	b.WriteString("}")
	h := newHarness(t, b.String())
	l, err := h.svc.List(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(l.Providers["fake"].Error, "more than") || len(l.Rows) != 0 {
		t.Errorf("error %q, %d rows", l.Providers["fake"].Error, len(l.Rows))
	}
}

func TestBackupFailureStopsTheWrite(t *testing.T) {
	h := newHarness(t, seed)
	h.svc.backup = func(string, []byte) error { return errors.New("disk full") }
	if _, err := h.svc.SetEnabled(context.Background(), "fake", "hook", "a", false); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := h.file()["hook/a"]; !ok {
		t.Error("file changed without a backup")
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(link, []byte("old"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Errorf("target = %q", b)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the original 0600", fi.Mode().Perm())
	}
	// A file changed since the bytes were read is not overwritten.
	if err := writeAtomic(link, []byte("stale"), []byte("newer")); !errors.Is(err, ErrConflict) {
		t.Errorf("stale write: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Errorf("stale write changed the file: %q", b)
	}
	// A missing file is created, with its directory.
	fresh := filepath.Join(dir, "sub", "fresh.json")
	if err := writeAtomic(fresh, nil, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("fresh file: %v %v", fi, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".orch-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestWriteAtomicFailures(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	_ = os.Symlink(b, a)
	_ = os.Symlink(a, b)
	if err := writeAtomic(a, nil, []byte("x")); err == nil {
		t.Error("wrote through a symlink loop")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := writeAtomic(filepath.Join(locked, "f.json"), nil, []byte("x")); err == nil {
		t.Error("wrote into a read-only directory")
	}
	// The file turning into something unreadable mid-write is refused.
	target := filepath.Join(dir, "gone")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, nil, []byte("x")); err == nil {
		t.Error("replaced a directory")
	}
	// A dangling link is refused: nothing is created where it points.
	dangling := filepath.Join(dir, "dangling")
	_ = os.Symlink(filepath.Join("nowhere", "deep", "f.json"), dangling)
	if err := writeAtomic(dangling, nil, []byte("x")); err == nil || !strings.Contains(err.Error(), "missing file") {
		t.Errorf("dangling link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nowhere")); !os.IsNotExist(err) {
		t.Error("a directory was created where the link points")
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	if b, err := ReadFile(filepath.Join(dir, "missing")); b != nil || err != nil {
		t.Errorf("missing = %v %v", b, err)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, nil, 0o600)
	if b, err := ReadFile(empty); b == nil || len(b) != 0 || err != nil {
		t.Errorf("empty = %v %v", b, err)
	}
	big := filepath.Join(dir, "big")
	_ = os.WriteFile(big, bytes.Repeat([]byte("x"), MaxFileSize+1), 0o600)
	if _, err := ReadFile(big); err == nil {
		t.Error("oversized file read")
	}
	if _, err := ReadFile(dir); err == nil {
		t.Error("directory read")
	}
}

func TestRegistryAndDedupe(t *testing.T) {
	p := &fakeProvider{}
	if _, err := NewRegistry(p, p); err == nil {
		t.Error("duplicate provider registered")
	}
	r, _ := NewRegistry(p)
	if r.Get("fake") != p || r.Get("x") != nil || len(r.Providers()) != 1 {
		t.Error("registry lookups")
	}
	if _, ok := Kind(p, "plugin"); !ok {
		t.Error("kind lookup")
	}
	if _, ok := Kind(p, "x"); ok {
		t.Error("unknown kind found")
	}
	got := Dedupe([]Item{{Kind: "k", Identity: "a"}, {Kind: "k", Identity: "a"}, {Kind: "j", Identity: "a"}})
	if got[0].Identity != "a" || got[1].Identity != "a#2" || got[2].Identity != "a" {
		t.Errorf("dedupe = %+v", got)
	}
}

func TestToggles(t *testing.T) {
	h := newHarness(t, seed)
	ctx := context.Background()
	h.svc.Sync(ctx)
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false); err != nil {
		t.Fatal(err)
	}
	h.write(`{"hook/a":{"on":true,"v":"one"}}`) // the plugin and the orch hook go away
	h.svc.Sync(ctx)
	// hook/a came back in the file, so turn it off again for the count.
	if _, err := h.svc.SetEnabled(ctx, "fake", "hook", "a", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("ORCH_DATA_DIR"), "orch.db")
	tg, err := LoadToggles(path)
	if err != nil {
		t.Fatal(err)
	}
	if !tg.Disabled("fake", "hook", "a") || tg.Disabled("fake", "plugin", "p") || tg.Count("fake") != 1 {
		t.Errorf("toggles = %+v (a removed item is not one the user turned off)", tg)
	}
	if tg, err := LoadToggles(filepath.Join(t.TempDir(), "none.db")); err != nil || tg.Count("fake") != 0 {
		t.Errorf("missing store: %+v %v", tg, err)
	}
	bare := filepath.Join(t.TempDir(), "bare.db")
	conn, err := sql.Open("sqlite", "file:"+bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE meta (k TEXT)`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if tg, err := LoadToggles(bare); err != nil || tg.Count("fake") != 0 {
		t.Errorf("store without the table: %+v %v", tg, err)
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	_ = os.WriteFile(corrupt, []byte("this is not a database file at all, not even close......"), 0o600)
	if _, err := LoadToggles(corrupt); err == nil {
		t.Error("a corrupt store read as no toggles")
	}
	var zero Toggles
	if zero.Disabled("a", "b", "c") || zero.Count("a") != 0 {
		t.Error("zero toggles")
	}
}

func TestRowStatus(t *testing.T) {
	tests := []struct {
		r    Row
		want Status
	}{
		{Row{Enabled: true, Present: true}, StatusActive},
		{Row{Enabled: false}, StatusDisabled},
		{Row{Enabled: true}, StatusMissing},
		{Row{RemovedAt: fromUnix(5)}, StatusRemoved},
	}
	for _, tt := range tests {
		if got := tt.r.Status(); got != tt.want {
			t.Errorf("%+v: %s, want %s", tt.r, got, tt.want)
		}
	}
	if unix(fromUnix(0)) != 0 || unix(fromUnix(9)) != 9 {
		t.Error("unix round trip")
	}
}

package spaces

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTermsMatching is SP-044.1.a1: the parser and matcher, table-driven:
// literal and regex terms, case, word boundaries, comments, and the
// invalid files that are rejected.
func TestTermsMatching(t *testing.T) {
	file := strings.Join([]string{
		"# project code names",
		"",
		"Falcon",
		"  Project Zephyr  ",
		"c++ core",
		"re:INV-[0-9]{4}",
		"re:(alpha|beta)-team",
		"Überwald",
		"acme.internal",
	}, "\n") + "\r\n"
	l, err := ParseTerms("globex", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if l.Len() != 7 {
		t.Fatalf("%d terms, want 7", l.Len())
	}
	cases := []struct {
		text string
		line int // 0: no match
	}{
		{"we ship Falcon next week", 3},
		{"FALCON", 3},
		{"falcon.", 3},
		{"(falcon)", 3},
		{"falcon_x is a variable", 0},
		{"falconry is a sport", 0},
		{"refalcon", 0},
		{"see project   zephyr", 0},
		{"see Project Zephyr docs", 4},
		{"project zephyr", 4},
		{"our c++ core library", 5},
		{"c++ core", 5},
		{"ticket INV-1234 is open", 6},
		{"inv-1234", 6},
		{"INV-123", 0},
		{"INV-12345", 0},
		{"the alpha-team", 7},
		{"BETA-TEAM rocks", 7},
		{"alphabeta-team", 0},
		{"in Überwald today", 8},
		{"überwald", 8},
		{"Überwaldx", 0},
		{"host acme.internal resolves", 9},
		{"acmexinternal", 0},
		{"", 0},
		{"nothing to see here", 0},
	}
	for _, c := range cases {
		hit, ok := l.Match(c.text)
		switch {
		case c.line == 0 && ok:
			t.Errorf("%q matched line %d", c.text, hit.Line)
		case c.line != 0 && (!ok || hit.Line != c.line || hit.Space != "globex" || len(hit.Hash) != 12):
			t.Errorf("%q: hit %+v, %v; want line %d", c.text, hit, ok, c.line)
		}
	}
	if len(cases) < 25 {
		t.Errorf("%d cases, want at least 25", len(cases))
	}
}

// TestTermsRejected: invalid files are refused with the line, never the
// term, in the error.
func TestTermsRejected(t *testing.T) {
	cases := map[string]string{
		"invalid regex":         "ok\nre:secret(unclosed\n",
		"empty pattern":         "re:   \n",
		"matches empty text":    "re:x*\n",
		"line too long":         strings.Repeat("s", maxTermLen+1),
		"not UTF-8":             "caf\xe9",
		"file too large":        strings.Repeat("a\n", maxTermsFile),
		"too many terms":        strings.Repeat("t\n", maxTerms+1),
		"invalid regex (class)": "re:[z-a]secret",
	}
	for name, data := range cases {
		_, err := ParseTerms("acme", []byte(data))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: error %q names the term", name, err)
		}
	}
	if _, err := ParseTerms("acme", []byte("ok\nre:secret(\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %v does not name line 2", err)
	}
}

// TestTermsRedact: Redact replaces every occurrence, back to back, and
// keeps what surrounds them; a regex term with its own groups too.
func TestTermsRedact(t *testing.T) {
	l, err := ParseTerms("a", []byte("falcon\nre:(red|blue)-([0-9]+)\n"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseTerms("b", []byte("zephyr\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"falcon falcon,falcon":         "[term] [term],[term]",
		`{"cmd":"cd /x/Falcon/y"}`:     `{"cmd":"cd /x/[term]/y"}`,
		"blue-42 and red-7":            "[term] and [term]",
		"zephyr meets falcon":          "[term] meets [term]",
		"falconry and zephyrs are not": "falconry and zephyrs are not",
	}
	for in, want := range cases {
		if got := RedactTerms(in, []TermList{l, m}); got != want {
			t.Errorf("RedactTerms(%q) = %q, want %q", in, got, want)
		}
	}
	h, _ := l.Match("falcon")
	if s := h.String(); strings.Contains(s, "falcon") || !strings.Contains(s, "line 1") {
		t.Errorf("hit string %q", s)
	}
}

// TestSpaceTerms reads each space's terms file; one that cannot be read is
// an error for that space alone.
func TestSpaceTerms(t *testing.T) {
	home := realTemp(t)
	writeFile(t, filepath.Join(home, "acme.txt"), "falcon\n")
	writeFile(t, filepath.Join(home, "bad.txt"), "re:(\n")
	res := mustValid(t, Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, TermsFile: "~/acme.txt"},
		{Name: "globex", Kind: Isolated, TermsFile: "~/missing.txt"},
		{Name: "initech", Kind: Isolated, TermsFile: "~/bad.txt"},
		{Name: "oss", Kind: Public},
	}}, Env{Home: home, InGitTree: func(string) bool { return false }})
	lists, errs := SpaceTerms(res, home)
	if len(lists) != 1 || lists[0].Space != "acme" || len(errs) != 2 || errs["globex"] == nil || errs["initech"] == nil {
		t.Errorf("lists %v, errs %v", lists, errs)
	}
	for space, err := range errs {
		if strings.Contains(err.Error(), "falcon") || strings.Contains(err.Error(), "re:(") {
			t.Errorf("%s: error %q names a term", space, err)
		}
	}
	if l, e := SpaceTerms(nil, home); l != nil || e != nil {
		t.Errorf("no policy: %v %v", l, e)
	}
	if err := os.Chmod(filepath.Join(home, "acme.txt"), 0); err == nil && os.Getuid() != 0 {
		if _, err := ReadTerms("acme", filepath.Join(home, "acme.txt")); err == nil {
			t.Error("unreadable terms file read")
		}
	}
}

// TestReadApplied reads the applied policy read-only, as the hooks do.
func TestReadApplied(t *testing.T) {
	ctx := context.Background()
	if r, err := ReadApplied(ctx, filepath.Join(t.TempDir(), "none.db"), testEnv()); r != nil || err != nil {
		t.Errorf("missing store: %v %v", r, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	writeFile(t, empty, "")
	if r, err := ReadApplied(ctx, empty, testEnv()); r != nil || err != nil {
		t.Errorf("store without tables: %v %v", r, err)
	}
	conn := openStore(t)
	dbPath := filepath.Join(os.Getenv("ORCH_DATA_DIR"), "orch.db")
	if r, err := ReadApplied(ctx, dbPath, testEnv()); r != nil || err != nil {
		t.Errorf("nothing applied: %v %v", r, err)
	}
	saveAndApply(t, openEngine(t, conn), validPolicy)
	if r, err := ReadApplied(ctx, dbPath, testEnv()); err != nil || !r.Configured() {
		t.Errorf("applied: %v %v", r, err)
	}
	garbage := filepath.Join(t.TempDir(), "garbage.db")
	writeFile(t, garbage, strings.Repeat("not a database ", 100))
	if _, err := ReadApplied(ctx, garbage, testEnv()); err == nil {
		t.Error("a damaged store read")
	}
}

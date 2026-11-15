package spaces

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureTerms are terms that must never appear in what orchd returns.
const fixtureTerms = "# job words\nProjectNightjar\nre:acme-[0-9]+\n\nhunter-two\n"

func TestWriteTermsWritesAndDescribesWithoutTerms(t *testing.T) {
	key := []byte("k")
	home := t.TempDir()
	path := filepath.Join(home, "new", "acme.terms")
	info, err := WriteTerms("acme", path, home, []byte(fixtureTerms), key)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Count != 3 || len(info.Fingerprint) != 12 || info.Problem != "" {
		t.Fatalf("info = %+v", info)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != fixtureTerms {
		t.Fatalf("file = %q, %v", got, err)
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %v; want no group or other access", p, perm)
		}
	}
	if d := DescribeTerms("acme", path, key); d != info {
		t.Fatalf("describe = %+v, want %+v", d, info)
	}
	// Another key gives another fingerprint; a changed file another too.
	if d := DescribeTerms("acme", path, []byte("other")); d.Fingerprint == info.Fingerprint {
		t.Error("the fingerprint does not depend on the key")
	}
	again, err := WriteTerms("acme", path, home, []byte("ProjectNightjar\n"), key)
	if err != nil || again.Count != 1 || again.Fingerprint == info.Fingerprint {
		t.Fatalf("rewrite = %+v, %v", again, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the folder holds %d entries; the temporary file was left behind", len(entries))
	}
}

func TestWriteTermsRefusesWithoutNamingATerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acme.terms")
	if err := os.WriteFile(path, []byte("Kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.terms")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	// A folder on the way replaced by a link into an agent's workspace.
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ path, data string }{
		"bad regexp":     {path, "fine\nre:Secret(Word\n"},
		"empty":          {path, "re:   \n"},
		"relative":       {"acme.terms", "x\n"},
		"unclean":        {dir + "/x/../acme.terms", "x\n"},
		"symlink":        {link, "x\n"},
		"not a file":     {dir, "x\n"},
		"not utf8":       {path, "\xff\xfe"},
		"matches none":   {path, "re:a*\n"},
		"linked folder":  {filepath.Join(dir, "linked", "deeper", "acme.terms"), "x\n"},
		"open folder":    {filepath.Join(open, "acme.terms"), "x\n"},
		"file as folder": {filepath.Join(dir, "plain", "acme.terms"), "x\n"},
	} {
		_, err := WriteTerms("acme", c.path, dir, []byte(c.data), []byte("k"))
		if err == nil {
			t.Errorf("%s: written", name)
			continue
		}
		if strings.Contains(err.Error(), "Secret") || strings.Contains(err.Error(), "Word") {
			t.Errorf("%s: the error names the term: %v", name, err)
		}
		if want := map[string]string{"linked folder": "symbolic link", "open folder": "other users", "file as folder": "not a folder"}[name]; want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it to say %q", name, err, want)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "Kept\n" {
		t.Fatalf("a refused write changed the file: %q", got)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("a write went through the linked folder: %v", entries)
	}
	// Without a home every folder is checked: the temporary folder's own
	// path may hold a link (macOS's /var), so only a clean one passes.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		if _, err := WriteTerms("acme", filepath.Join(real, "nohome.terms"), "", []byte("x\n"), []byte("k")); err != nil {
			t.Fatalf("no home, real path: %v", err)
		}
	}
}

func TestDescribeTermsReportsMissingAndBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	if d := DescribeTerms("acme", filepath.Join(dir, "none"), []byte("k")); d != (TermsInfo{}) {
		t.Fatalf("missing = %+v", d)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("ok\nre:Hidden(\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := DescribeTerms("acme", bad, []byte("k"))
	if !d.Exists || d.Problem == "" || !strings.Contains(d.Problem, "line 2") || strings.Contains(d.Problem, "Hidden") {
		t.Fatalf("broken = %+v", d)
	}
	if d := DescribeTerms("acme", dir, []byte("k")); !d.Exists || d.Problem == "" {
		t.Fatalf("folder = %+v", d)
	}
}

func TestResolvedTermsFile(t *testing.T) {
	res, err := Validate(Policy{Spaces: []Space{
		{Name: "acme", Kind: Isolated, TermsFile: "~/.config/orch/acme.terms"},
		{Name: "oss", Kind: Public},
	}}, Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.TermsFile("acme", "/home/u"); got != "/home/u/.config/orch/acme.terms" {
		t.Errorf("acme = %q", got)
	}
	if res.TermsFile("oss", "/home/u") != "" || res.TermsFile("nope", "/home/u") != "" {
		t.Error("a space without a terms file names one")
	}
}

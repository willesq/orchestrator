package github

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fileReaders are calls that read a path or what is under it.
var fileReaders = map[string]bool{
	"os.Open": true, "os.OpenFile": true, "os.ReadFile": true, "os.ReadDir": true, "os.Stat": true,
	"os.Lstat": true, "os.DirFS": true, "os.Readlink": true, "filepath.Walk": true, "filepath.WalkDir": true,
	"filepath.Glob": true, "ioutil.ReadFile": true, "ioutil.ReadDir": true, "fs.ReadFile": true, "fs.WalkDir": true,
}

// profileNames are identifiers and text that name a gh profile directory.
var profileNames = []string{"GHConfigDir", "ConfigDir", "ghDirs", "GH_CONFIG_DIR"}

// TestNoCodeReadsGHCredentials is SP-063.1.a4: no code outside tests runs
// 'gh auth token' or asks 'gh auth status' for a token, names gh's token
// files, or reads a path built from a gh profile directory. orch checks a
// profile only with 'gh auth status' and the login it reports.
func TestNoCodeReadsGHCredentials(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	var found []string
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files++
			rel, _ := filepath.Rel(root, path)
			for _, f := range credentialFindings(t, path) {
				found = append(found, rel+": "+f)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 100 {
		t.Fatalf("scanned only %d files; is the module root %s?", files, root)
	}
	for _, f := range found {
		t.Error(f)
	}
	t.Logf("scanned %d files, %d occurrences", files, len(found))
}

// credentialFindings parses one file and reports each forbidden occurrence.
func credentialFindings(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	add := func(n ast.Node, what string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+what)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			for _, bad := range []string{"auth token", "--show-token", "hosts.yml", "oauth_token"} {
				if strings.Contains(s, bad) {
					add(n, "string "+strconv.Quote(bad))
				}
			}
		case *ast.CallExpr:
			lits := stringArgs(n.Args)
			for i := 0; i+1 < len(lits); i++ {
				if lits[i] == "auth" && lits[i+1] == "token" {
					add(n, "runs gh auth token")
				}
			}
			if name := callName(n.Fun); fileReaders[name] {
				for _, a := range n.Args {
					if mentionsProfile(a) {
						add(n, name+" of a gh profile directory")
					}
				}
			}
		}
		return true
	})
	return out
}

// stringArgs is each argument's string value, or "" for one that is not a
// string literal.
func stringArgs(args []ast.Expr) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out[i], _ = strconv.Unquote(lit.Value)
		}
	}
	return out
}

func callName(fun ast.Expr) string {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

// mentionsProfile reports whether expression e uses a name or text that
// stands for a gh profile directory.
func mentionsProfile(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			for _, p := range profileNames {
				found = found || n.Name == p
			}
		case *ast.BasicLit:
			for _, p := range profileNames {
				found = found || strings.Contains(n.Value, p)
			}
		}
		return !found
	})
	return found
}

// TestCredentialFindingsCatchesViolations keeps the scan honest: each
// forbidden pattern in a sample file is reported.
func TestCredentialFindingsCatchesViolations(t *testing.T) {
	src := `package x
import ("os"; "os/exec"; "path/filepath")
func a(p struct{ ConfigDir string }) {
	exec.Command("gh", "auth", "token").Run()
	exec.Command("gh", "auth", "status", "--show-token").Run()
	os.ReadFile(filepath.Join(p.ConfigDir, "hosts.yml"))
	os.ReadDir(os.Getenv("GH_CONFIG_DIR"))
	os.ReadFile("/etc/hosts")
}
`
	path := filepath.Join(t.TempDir(), "x.go")
	if err := writeFile(path, src); err != nil {
		t.Fatal(err)
	}
	got := credentialFindings(t, path)
	// gh auth token, --show-token, hosts.yml, and the two profile reads;
	// the read of /etc/hosts is fine.
	if len(got) != 5 {
		t.Fatalf("findings = %q, want 5", got)
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

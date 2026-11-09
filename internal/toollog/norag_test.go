package toollog

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

const module = "github.com/willesq/orchestrator/"

// ragWrites are the calls that put content into the RAG store: every writer
// goes through rag.Service.IngestText, which stores through Upsert, and the
// Ingest RPC.
var ragWrites = map[string]bool{"IngestText": true, "Upsert": true, "Ingest": true}

// toollogImports are all this package may import besides the standard
// library: nothing that can reach the RAG store.
var toollogImports = map[string]bool{
	"modernc.org/sqlite":       true,
	module + "internal/redact": true,
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// mentionsLog reports whether name refers to the command log or the tool
// events it stores.
func mentionsLog(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "toollog") || strings.Contains(n, "toolevent") || strings.Contains(n, "toolrecord")
}

// TestCommandLogNeverReachesRAG is SP-014.1.a4 (INV-12): no code path
// ingests tool events into RAG. The log package imports nothing that can
// reach the store, and across the repository no function both touches the
// command log or tool events and calls a RAG write.
func TestCommandLogNeverReachesRAG(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	own, err := filepath.Glob(filepath.Join(root, "internal", "toollog", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range own {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.Split(p, "/")[0], ".") && !toollogImports[p] {
				t.Errorf("%s imports %s; the command log may import only the standard library, sqlite and redact",
					filepath.Base(path), p)
			}
		}
	}

	var writers, logUsers int
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.HasSuffix(path, ".pb.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var usesLog, writes bool
				var write string
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Ident:
						if mentionsLog(x.Name) {
							usesLog = true
						}
					case *ast.CallExpr:
						if sel, ok := x.Fun.(*ast.SelectorExpr); ok && ragWrites[sel.Sel.Name] {
							writes, write = true, sel.Sel.Name
						}
					}
					return true
				})
				if mentionsLog(fn.Name.Name) {
					usesLog = true
				}
				if writes {
					writers++
				}
				if usesLog {
					logUsers++
				}
				if usesLog && writes {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s: %s touches the command log and calls %s", rel, fn.Name.Name, write)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Guard against the test going vacuous after a rename.
	if writers < 6 || logUsers < 5 {
		t.Errorf("found %d RAG writers and %d command log users; the scan no longer sees the code it checks", writers, logUsers)
	}
	t.Logf("checked %d RAG-writing functions against %d command log functions", writers, logUsers)
}

package toollog

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// rootFiles hold the code that takes, stores or reads tool events.
var rootFiles = map[string]bool{
	"internal/toollog/record.go":      true,
	"internal/toollog/store.go":       true,
	"internal/toollog/writer.go":      true,
	"internal/toollog/read.go":        true,
	"internal/daemon/toollog.go":      true,
	"internal/daemon/toolevents.go":   true,
	"internal/daemon/toollog_read.go": true,
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
// reach the store; no function both touches the command log or tool events
// and calls a RAG write; and no function reachable by calls from the code
// that takes, stores or reads tool events writes to RAG. Calls are
// resolved with go/types; a call through an interface may reach any
// repository method of that name, and a call through a function value is
// not followed (the functions the log is handed are defined in its files).
func TestCommandLogNeverReachesRAG(t *testing.T) {
	g := loadCallGraph(t, repoRoot(t))

	for _, fn := range g.funcs {
		if !strings.HasPrefix(fn.file, "internal/toollog/") {
			continue
		}
		for _, p := range fn.imports {
			if strings.Contains(strings.Split(p, "/")[0], ".") && !toollogImports[p] {
				t.Errorf("%s imports %s; the command log may import only the standard library, sqlite and redact", fn.file, p)
			}
		}
	}

	var writers, logUsers int
	for _, fn := range g.funcs {
		if fn.write != "" {
			writers++
		}
		if fn.usesLog {
			logUsers++
		}
		if fn.usesLog && fn.write != "" {
			t.Errorf("%s: %s touches the command log and calls %s", fn.file, fn.id, fn.write)
		}
	}

	from := map[string]string{}
	var queue []string
	for id, fn := range g.funcs {
		if rootFiles[fn.file] {
			from[id] = ""
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	roots := len(queue)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		fn := g.funcs[id]
		if fn == nil {
			continue
		}
		if fn.write != "" {
			path := id
			for p := from[id]; p != ""; p = from[p] {
				path = p + " -> " + path
			}
			t.Errorf("tool events can reach %s: %s", fn.write, path)
			continue
		}
		for _, next := range g.callees(fn) {
			if _, seen := from[next]; !seen {
				from[next] = id
				queue = append(queue, next)
			}
		}
	}
	// Guard against the test going vacuous after a rename.
	if writers < 6 || logUsers < 5 || roots < 20 {
		t.Errorf("found %d RAG writers, %d command log users and %d root functions; the scan no longer sees the code it checks",
			writers, logUsers, roots)
	}
	t.Logf("checked %d RAG-writing functions against %d command log functions; %d functions reachable from %d in the log's files",
		writers, logUsers, len(from), roots)
}

// TestCallGraphFollowsHelpers: the graph sees a RAG write behind a chain of
// helpers and behind an interface, so the reachability check is not
// vacuous.
func TestCallGraphFollowsHelpers(t *testing.T) {
	g := loadCallGraph(t, repoRoot(t))
	reach := func(start string) bool {
		seen := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 {
			fn := g.funcs[queue[0]]
			queue = queue[1:]
			if fn == nil {
				continue
			}
			if fn.write != "" {
				return true
			}
			for _, n := range g.callees(fn) {
				if !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
		}
		return false
	}
	for _, id := range []string{
		// Writes through s.rag, an interface.
		"(*" + module + "internal/daemon.server).Ingest",
		// Writes through storeSessions -> storeSession -> IngestText.
		module + "internal/daemon.handoffUnit",
	} {
		if g.funcs[id] == nil {
			t.Errorf("%s not in the graph", id)
		} else if !reach(id) {
			t.Errorf("%s reaches no RAG write; the graph misses calls", id)
		}
	}
}

type graphFunc struct {
	id, file string
	imports  []string
	// calls are the repository functions called by ID; ifaceCalls the
	// names of methods called through an interface.
	calls      []string
	ifaceCalls []string
	usesLog    bool
	// write names the RAG write the function calls itself, if any.
	write string
}

type callGraph struct {
	funcs map[string]*graphFunc
	// methods are the IDs of every repository method by name.
	methods map[string][]string
}

func (g *callGraph) callees(fn *graphFunc) []string {
	out := append([]string(nil), fn.calls...)
	for _, name := range fn.ifaceCalls {
		out = append(out, g.methods[name]...)
	}
	return out
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
}

// loadCallGraph type-checks every repository package from source, with its
// dependencies read from the build's export data, and records each
// function's calls.
func loadCallGraph(t *testing.T, root string) *callGraph {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	var repo []listedPackage
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if strings.HasPrefix(p.ImportPath, module) && len(p.GoFiles) > 0 {
			repo = append(repo, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if e := exports[path]; e != "" {
			return os.Open(e)
		}
		return nil, errors.New("no export data for " + path)
	})
	g := &callGraph{funcs: map[string]*graphFunc{}, methods: map[string][]string{}}
	for _, p := range repo {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: imp}
		if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range files {
			rel, _ := filepath.Rel(root, fset.File(f.Pos()).Name())
			var imports []string
			for _, im := range f.Imports {
				s, _ := strconv.Unquote(im.Path.Value)
				imports = append(imports, s)
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				id := funcID(p.ImportPath, fd)
				fn := &graphFunc{id: id, file: filepath.ToSlash(rel), imports: imports, usesLog: mentionsLog(fd.Name.Name)}
				if fd.Recv != nil {
					g.methods[fd.Name.Name] = append(g.methods[fd.Name.Name], id)
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Ident:
						if mentionsLog(x.Name) {
							fn.usesLog = true
						}
					case *ast.CallExpr:
						recordCall(fn, x, info)
					}
					return true
				})
				g.funcs[id] = fn
			}
		}
	}
	return g
}

// recordCall adds the function call c names to fn.
func recordCall(fn *graphFunc, c *ast.CallExpr, info *types.Info) {
	var ident *ast.Ident
	switch f := c.Fun.(type) {
	case *ast.Ident:
		ident = f
	case *ast.SelectorExpr:
		ident = f.Sel
		if ragWrites[f.Sel.Name] {
			fn.write = f.Sel.Name
		}
	case *ast.IndexExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			ident = id
		}
	}
	if ident == nil {
		return
	}
	callee, ok := info.Uses[ident].(*types.Func)
	if !ok {
		return
	}
	sig, _ := callee.Type().(*types.Signature)
	switch {
	case sig != nil && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()):
		fn.ifaceCalls = append(fn.ifaceCalls, callee.Name())
	case callee.Pkg() != nil && strings.HasPrefix(callee.Pkg().Path(), module):
		fn.calls = append(fn.calls, callee.Origin().FullName())
	}
}

// funcID is the ID go/types gives the function fd declares
// (types.Func.FullName), computed from the syntax so it matches the IDs
// calls name whichever package checked them.
func funcID(pkg string, fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return pkg + "." + fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	ptr := false
	if s, ok := typ.(*ast.StarExpr); ok {
		ptr, typ = true, s.X
	}
	switch x := typ.(type) {
	case *ast.IndexExpr:
		typ = x.X
	case *ast.IndexListExpr:
		typ = x.X
	}
	name := pkg + "." + typ.(*ast.Ident).Name
	if ptr {
		name = "*" + name
	}
	return "(" + name + ")." + fd.Name.Name
}

// Package check verifies the orchestrator's runtime preconditions: the onnxruntime
// dylib, the embedding model files, orchd reachability, and gh auth. `orch check`
// prints one line per check and exits non-zero if any fail, so it is scriptable.
package check

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Result is one precondition outcome.
type Result struct {
	Name   string
	Detail string
	OK     bool
}

// Check produces one Result.
type Check func() Result

// RunAll runs every check and reports whether all passed.
func RunAll(checks []Check) ([]Result, bool) {
	allOK := true
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := c()
		results = append(results, r)
		if !r.OK {
			allOK = false
		}
	}
	return results, allOK
}

// Print writes results one per line.
func Print(w io.Writer, results []Result) {
	for _, r := range results {
		mark := "OK  "
		if !r.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "[%s] %s — %s\n", mark, r.Name, r.Detail)
	}
}

// FileCheck passes if path exists.
func FileCheck(name, path string) Check {
	return func() Result {
		if fileExists(path) {
			return Result{Name: name, Detail: path, OK: true}
		}
		return Result{Name: name, Detail: "missing: " + path, OK: false}
	}
}

// CommandCheck passes if the command exits zero.
func CommandCheck(name string, argv ...string) Check {
	return func() Result {
		if len(argv) == 0 {
			return Result{Name: name, Detail: "no command", OK: false}
		}
		if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
			return Result{Name: name, Detail: err.Error(), OK: false}
		}
		return Result{Name: name, Detail: "ok", OK: true}
	}
}

// SocketCheck passes if orchd answers a Ping.
func SocketCheck(name string) Check {
	return func() Result {
		client, cleanup, err := orchdclient.Dial()
		if err != nil {
			return Result{Name: name, Detail: err.Error(), OK: false}
		}
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := client.Ping(ctx, &orchdv1.PingRequest{}); err != nil {
			return Result{Name: name, Detail: "orchd not running", OK: false}
		}
		return Result{Name: name, Detail: "orchd reachable", OK: true}
	}
}

// Default assembles the standard precondition checks from config + paths.
func Default() []Check {
	v, _ := config.Load()
	s, _ := config.Unmarshal(v)

	modelDir := s.Embedder.ModelDir
	if modelDir == "" {
		modelDir = filepath.Join(paths.DataDir(), "models", "minilm")
	}
	m := model.New(model.Config{Dir: modelDir})

	onnxDir := s.Embedder.OnnxLibDir
	if onnxDir == "" {
		onnxDir = resolveOnnxLibDir()
	}

	return []Check{
		FileCheck("onnxruntime dylib", filepath.Join(onnxDir, "libonnxruntime.dylib")),
		func() Result {
			if m.Present() {
				return Result{Name: "embedding model", Detail: modelDir, OK: true}
			}
			return Result{Name: "embedding model", Detail: "missing (run: orch model fetch)", OK: false}
		},
		SocketCheck("orchd"),
		CommandCheck("gh auth", "gh", "auth", "status"),
	}
}

func resolveOnnxLibDir() string {
	if out, err := exec.Command("brew", "--prefix", "onnxruntime").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return filepath.Join(p, "lib")
		}
	}
	return "/opt/homebrew/opt/onnxruntime/lib"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

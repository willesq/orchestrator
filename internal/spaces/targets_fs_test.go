package spaces

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realTemp returns a temp dir with its own symlinks resolved, since macOS
// keeps temp dirs under the /var -> /private/var link.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestTargetsPathsCleaned(t *testing.T) {
	tool, in := bash("cat /a//b/./c/../d ./x/../y")
	got := extractTargets(tool, json.RawMessage(in), testCwd, testHome)
	for _, w := range []string{"/a/b/d", "/scratch/project/y"} {
		if !slices.Contains(got.Paths, w) {
			t.Errorf("paths %q missing %q", got.Paths, w)
		}
	}
}

func TestTargetsResolvesSymlinks(t *testing.T) {
	root := realTemp(t)
	own, other := filepath.Join(root, "own"), filepath.Join(root, "other", "job")
	mkdirs(t, own, other)
	symlink(t, other, filepath.Join(own, "link"))
	symlink(t, "../other", filepath.Join(own, "rel"))

	cases := []struct {
		name, cmd, want string
	}{
		{"absolute link", "cat " + own + "/link/secret", filepath.Join(other, "secret")},
		{"relative link", "cat rel/job/x", filepath.Join(root, "other", "job", "x")},
		// link/.. is the parent of the link's target, not own: cleaning
		// first would hide the other job's directory.
		{"dotdot after link", "cat link/../sibling", filepath.Join(root, "other", "sibling")},
		{"missing tail", "cat link/a/b/c", filepath.Join(other, "a", "b", "c")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, in := bash(c.cmd)
			got := extractTargets(tool, json.RawMessage(in), own, testHome)
			if !slices.Contains(got.Paths, c.want) {
				t.Errorf("paths %q missing %q", got.Paths, c.want)
			}
		})
	}

	t.Run("cd is logical, files physical", func(t *testing.T) {
		tool, in := bash("cd link/.. && cat f")
		got := extractTargets(tool, json.RawMessage(in), own, testHome)
		if !slices.Contains(got.CdDest, own) || !slices.Contains(got.Paths, filepath.Join(own, "f")) {
			t.Errorf("cd_dest %q paths %q", got.CdDest, got.Paths)
		}
	})
	t.Run("cd into link resolves", func(t *testing.T) {
		tool, in := bash("cd link")
		got := extractTargets(tool, json.RawMessage(in), own, testHome)
		if !slices.Contains(got.CdDest, other) {
			t.Errorf("cd_dest %q missing %q", got.CdDest, other)
		}
	})
	t.Run("read tool through link", func(t *testing.T) {
		got := extractTargets("Read", json.RawMessage(`{"file_path":"`+own+`/link/k"}`), own, testHome)
		if !slices.Contains(got.Paths, filepath.Join(other, "k")) {
			t.Errorf("paths %q", got.Paths)
		}
	})
	t.Run("symlink loop ends", func(t *testing.T) {
		symlink(t, "loop2", filepath.Join(own, "loop1"))
		symlink(t, "loop1", filepath.Join(own, "loop2"))
		tool, in := bash("cat loop1/x")
		got := extractTargets(tool, json.RawMessage(in), own, testHome)
		if len(got.Paths) == 0 {
			t.Error("no path for a symlink loop")
		}
	})
}

// makeWorktree lays out a main checkout and a linked worktree the way
// git worktree add does, without running git.
func makeWorktree(t *testing.T, root string) (main, wt string) {
	t.Helper()
	main = filepath.Join(root, "repo")
	wt = filepath.Join(root, "repo.worktrees", "feat", "x")
	gitdir := filepath.Join(main, ".git", "worktrees", "x")
	mkdirs(t, gitdir, filepath.Join(wt, "pkg"))
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	writeFile(t, filepath.Join(gitdir, "commondir"), "../..\n")
	return main, wt
}

func TestTargetsMapWorktreeToMainCheckout(t *testing.T) {
	root := realTemp(t)
	main, wt := makeWorktree(t, root)

	tool, in := bash("cat pkg/a.go && cd pkg")
	got := extractTargets(tool, json.RawMessage(in), wt, testHome)
	for _, w := range []string{filepath.Join(wt, "pkg", "a.go"), filepath.Join(main, "pkg", "a.go")} {
		if !slices.Contains(got.Paths, w) {
			t.Errorf("paths %q missing %q", got.Paths, w)
		}
	}
	if !slices.Contains(got.CdDest, filepath.Join(main, "pkg")) {
		t.Errorf("cd_dest %q missing the main checkout", got.CdDest)
	}

	got = extractTargets("Bash", json.RawMessage(`{"command":"git -C `+wt+` status"}`), root, testHome)
	if !slices.Contains(got.GitDirs, main) || !slices.Contains(got.GitDirs, wt) {
		t.Errorf("git_dirs %q, want %q and %q", got.GitDirs, wt, main)
	}
}

func TestTargetsWorktreeRelativeGitdir(t *testing.T) {
	root := realTemp(t)
	main := filepath.Join(root, "repo")
	wt := filepath.Join(main, ".claude", "worktrees", "w")
	gitdir := filepath.Join(main, ".git", "worktrees", "w")
	mkdirs(t, gitdir, wt)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: ../../../.git/worktrees/w\n")
	writeFile(t, filepath.Join(gitdir, "commondir"), "../..\n")

	got := extractTargets("Read", json.RawMessage(`{"file_path":"`+wt+`/f"}`), root, testHome)
	if !slices.Contains(got.Paths, filepath.Join(main, "f")) {
		t.Errorf("paths %q missing main checkout path", got.Paths)
	}
}

func TestTargetsNotAWorktree(t *testing.T) {
	root := realTemp(t)
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(root, "sub")
	bare := filepath.Join(root, "bare.git")
	odd := filepath.Join(root, "odd")
	mkdirs(t, filepath.Join(repo, ".git"), sub, filepath.Join(bare, "worktrees", "b"), odd, filepath.Join(root, "b"))
	// A submodule's .git file names a git dir without a commondir.
	mkdirs(t, filepath.Join(root, "modgit"))
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: ../modgit\n")
	// A worktree of a bare repo has no main checkout.
	writeFile(t, filepath.Join(root, "b", ".git"), "gitdir: "+filepath.Join(bare, "worktrees", "b")+"\n")
	writeFile(t, filepath.Join(bare, "worktrees", "b", "commondir"), "../..\n")
	writeFile(t, filepath.Join(odd, ".git"), "not a gitdir line\n")

	for _, p := range []string{repo + "/f", sub + "/f", root + "/b/f", odd + "/f"} {
		got := extractTargets("Read", json.RawMessage(`{"file_path":"`+p+`"}`), root, testHome)
		if !slices.Equal(got.Paths, []string{p}) {
			t.Errorf("%s: paths %q, want only itself", p, got.Paths)
		}
	}
}

type recordedEvent struct {
	Scenario string
	Tool     string
	Input    json.RawMessage
	Cwd      string
}

func recordedToolEvents(t *testing.T) []recordedEvent {
	t.Helper()
	var out []recordedEvent
	for _, f := range []string{"claude-code.jsonl", "codex.jsonl"} {
		fh, err := os.Open(filepath.Join("..", "hook", "testdata", "payloads", f))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var line struct {
				Scenario string `json:"scenario"`
				Payload  struct {
					Event     string          `json:"hook_event_name"`
					Tool      string          `json:"tool_name"`
					ToolInput json.RawMessage `json:"tool_input"`
					Cwd       string          `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(sc.Bytes(), &line) != nil || line.Payload.Event != "PreToolUse" {
				continue
			}
			out = append(out, recordedEvent{line.Scenario, line.Payload.Tool, line.Payload.ToolInput, line.Payload.Cwd})
		}
		fh.Close()
	}
	if len(out) == 0 {
		t.Fatal("no recorded PreToolUse payloads")
	}
	return out
}

// FuzzTargets checks that no command string panics Targets, yields a
// relative target, or is pathologically slow. Parallel fuzz workers share
// the CPU, so the in-fuzz ceiling is loose; TestTargetsCorpusBudget
// measures the 2 ms per-input budget on the corpus a fuzz run leaves.
// Run it for the SP-013.2 measure with
// go test ./internal/spaces -run '^$' -fuzz FuzzTargets -fuzztime 60s.
func FuzzTargets(f *testing.F) {
	for _, c := range targetCases() {
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(c.input), &in) == nil && in.Command != "" {
			f.Add(in.Command)
		}
	}
	for _, s := range []string{"", "(", "$(", "${", "<<", "'", "\"", "`", "cd ~x/$(y)", "a=(1 2); ${a[@]}",
		"bash -c 'bash -c \"bash -c x\"'", "case $x in a) cd /;; esac", "f() { cd /a; }; f", "coproc cat /a"} {
		f.Add(s)
	}
	cwd, _ := filepath.EvalSymlinks(f.TempDir())
	f.Fuzz(func(t *testing.T, cmd string) {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		got := extractTargets("Bash", in, cwd, testHome)
		if len(cmd) < 4096 {
			best := time.Hour
			for i := 0; i < 5 && best > fuzzCeiling; i++ {
				start := time.Now()
				extractTargets("Bash", in, cwd, testHome)
				best = min(best, time.Since(start))
			}
			if best > fuzzCeiling {
				t.Fatalf("took %v for %q", best, cmd)
			}
		}
		for _, l := range [][]string{got.Paths, got.CdDest, got.GitDirs, got.Prefixes} {
			for _, p := range l {
				if !filepath.IsAbs(p) {
					t.Fatalf("relative target %q for %q", p, cmd)
				}
			}
		}
	})
}

const fuzzCeiling = 20 * time.Millisecond

// TestTargetsCorpusBudget replays every input in the fuzz corpus directory
// named by ORCH_FUZZ_CORPUS (the FuzzTargets directory under
// "go env GOCACHE"/fuzz) one at a time and checks each takes under 2 ms,
// the best of three runs. It is skipped when the variable is unset.
func TestTargetsCorpusBudget(t *testing.T) {
	dir := os.Getenv("ORCH_FUZZ_CORPUS")
	if dir == "" {
		t.Skip("ORCH_FUZZ_CORPUS not set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cwd := realTemp(t)
	var worst time.Duration
	var total time.Duration
	n := 0
	for _, ent := range entries {
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		_, body, ok := strings.Cut(string(data), "\nstring(")
		if !ok {
			continue
		}
		cmd, err := strconv.Unquote(strings.TrimSuffix(strings.TrimSpace(body), ")"))
		if err != nil {
			continue
		}
		in, _ := json.Marshal(map[string]string{"command": cmd})
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			extractTargets("Bash", in, cwd, testHome)
			best = min(best, time.Since(start))
		}
		total += best
		n++
		if best > worst {
			worst = best
		}
		if best > 2*time.Millisecond {
			t.Errorf("%s took %v", ent.Name(), best)
		}
	}
	if n == 0 {
		t.Fatal("no corpus inputs")
	}
	t.Logf("%d inputs: worst %v, mean %v", n, worst, total/time.Duration(n))
}

// BenchmarkTargets times every table case, for the SP-013.2 per-input
// budget of 2 ms.
func BenchmarkTargets(b *testing.B) {
	cases := targetCases()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := cases[i%len(cases)]
		extractTargets(c.tool, json.RawMessage(c.input), testCwd, testHome)
	}
}

func TestTargetsFIFOCommondirDoesNotHang(t *testing.T) {
	root := realTemp(t)
	d := filepath.Join(root, "d")
	mkdirs(t, filepath.Join(d, "g"))
	if err := syscall.Mkfifo(filepath.Join(d, "g", "commondir"), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	writeFile(t, filepath.Join(d, ".git"), "gitdir: g\n")
	done := make(chan ToolTargets, 1)
	go func() { done <- extractTargets("Read", json.RawMessage(`{"file_path":"`+d+`/f"}`), root, testHome) }()
	select {
	case got := <-done:
		if !slices.Equal(got.Paths, []string{filepath.Join(d, "f")}) {
			t.Errorf("paths %q", got.Paths)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Targets blocked on a FIFO commondir")
	}
}

func TestTargetsDeepPathIsBounded(t *testing.T) {
	tool, in := bash("cat /tmp/x/" + strings.Repeat("a/", 20000))
	start := time.Now()
	got := extractTargets(tool, json.RawMessage(in), testCwd, testHome)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("took %v", d)
	}
	if !got.Opaque {
		t.Error("deep path not opaque")
	}
}

func TestTargetsReadsScripts(t *testing.T) {
	root := realTemp(t)
	writeFile(t, filepath.Join(root, "run.sh"), "#!/bin/bash\ncd /other && cat secret\n")
	writeFile(t, filepath.Join(root, "env.sh"), "cd /sourced\n")
	writeFile(t, filepath.Join(root, "tool.py"), "#!/usr/bin/env python3\nopen('/py/secret')\n")
	writeFile(t, filepath.Join(root, "bin"), "\x7fELF\x00\x00binary")
	writeFile(t, filepath.Join(root, "big.sh"), strings.Repeat("# filler\n", maxScript/9+10))

	run := func(cmd string) ToolTargets {
		tool, in := bash(cmd)
		return extractTargets(tool, json.RawMessage(in), root, testHome)
	}
	if got := run("bash run.sh"); !slices.Contains(got.Paths, "/other/secret") || got.Opaque {
		t.Errorf("bash run.sh: %+v", got)
	}
	if got := run("./run.sh"); !slices.Contains(got.Paths, "/other/secret") {
		t.Errorf("./run.sh: paths %q", got.Paths)
	}
	if got := run("bash run.sh; cat f"); slices.Contains(got.Paths, "/other/f") {
		t.Error("a script's cd leaked into the caller")
	}
	if got := run("source env.sh && cat f"); !slices.Contains(got.Paths, "/sourced/f") {
		t.Errorf("source: paths %q", got.Paths)
	}
	if got := run("./tool.py"); !slices.Contains(got.Paths, "/py/secret") || got.Opaque {
		t.Errorf("python script: %+v", got)
	}
	if got := run("python3 tool.py"); !slices.Contains(got.Paths, "/py/secret") {
		t.Errorf("python3 tool.py: paths %q", got.Paths)
	}
	if got := run("./bin"); got.Opaque {
		t.Errorf("binary opaque: %q", got.OpaqueReasons)
	}
	if got := run("bash big.sh"); !got.Opaque {
		t.Error("oversized script not opaque")
	}
	if got := run("echo x > new.sh && bash new.sh"); !got.Opaque {
		t.Error("script written by the call not opaque")
	}
}

func TestPosixShebang(t *testing.T) {
	for text, want := range map[string]bool{
		"#!/bin/sh\n":                  true,
		"#!/usr/bin/env bash\n":        true,
		"#!/usr/bin/env -S zsh -e\n":   true,
		"#!\n":                         true,
		"#!/usr/bin/env python3\n":     false,
		"#!/usr/bin/env FOO=1 node\n":  false,
		"#!/usr/local/bin/fish\nfoo\n": false,
	} {
		if got := posixShebang(text); got != want {
			t.Errorf("posixShebang(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestReadSmallRejectsNonRegular(t *testing.T) {
	root := realTemp(t)
	if _, err := readSmall(root, 10); err == nil {
		t.Error("read a directory")
	}
	if _, err := readSmall(filepath.Join(root, "missing"), 10); err == nil {
		t.Error("read a missing file")
	}
}

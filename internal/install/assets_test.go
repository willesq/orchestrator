package install

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippedAssetsAreWellFormed(t *testing.T) {
	var docs []string
	for _, pattern := range []string{"assets/claude/agents/*.md", "assets/claude/skills/*/SKILL.md"} {
		m, err := fs.Glob(assets, pattern)
		if err != nil || len(m) == 0 {
			t.Fatalf("no assets match %s (err=%v)", pattern, err)
		}
		docs = append(docs, m...)
	}
	for _, src := range docs {
		t.Run(src, func(t *testing.T) {
			body, _ := assets.ReadFile(src)
			s := string(body)
			// Claude Code only reads frontmatter at the very start of the file.
			if !strings.HasPrefix(s, "---\n") {
				t.Fatalf("frontmatter must open the file")
			}
			end := strings.Index(s[4:], "\n---\n")
			if end < 0 {
				t.Fatalf("frontmatter is not closed")
			}
			want := strings.TrimSuffix(path.Base(src), ".md")
			if path.Base(src) == "SKILL.md" {
				want = path.Base(path.Dir(src))
			}
			if !strings.Contains(s[:end+4], "\nname: "+want+"\n") {
				t.Errorf("frontmatter name must be %q", want)
			}
			if !strings.Contains(s, managedMarker) {
				t.Errorf("missing %q marker", managedMarker)
			}
			if strings.Contains(s, ".claude/orchestrator") {
				t.Errorf("references the retired ~/.claude/orchestrator checkout")
			}
		})
	}
	script, err := assets.ReadFile("assets/bin/gittree")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(script, []byte("#!/usr/bin/env bash\n")) || !bytes.Contains(script, []byte(managedMarker)) {
		t.Errorf("gittree must start with a bash shebang and carry the marker")
	}
}

// assetHome is a throwaway home with the plan's paths precomputed.
type assetHome struct {
	home, legacy string
}

func newAssetHome(t *testing.T) assetHome {
	t.Helper()
	home := t.TempDir()
	return assetHome{home: home, legacy: legacyCheckout(home)}
}

func (h assetHome) agent(name string) string {
	return filepath.Join(h.home, ".claude", "agents", name+".md")
}
func (h assetHome) skillDir(name string) string {
	return filepath.Join(h.home, ".claude", "skills", name)
}

func (h assetHome) plan() Plan {
	return Plan{Target: "claude-code", Edits: append(claudeAssetEdits(h.home), toolEdits(h.home)...)}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isSymlink(t *testing.T, p string) bool {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()&os.ModeSymlink != 0
}

func shipped(t *testing.T, src string) string {
	t.Helper()
	b, err := assets.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAssetEditsInstall(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, h assetHome)
		check func(t *testing.T, h assetHome)
	}{
		{
			name:  "fresh home gets every asset",
			setup: func(*testing.T, assetHome) {},
			check: func(t *testing.T, h assetHome) {
				if got := readFile(t, h.agent("dev")); got != shipped(t, "assets/claude/agents/dev.md") {
					t.Errorf("dev agent not installed verbatim")
				}
				if got := readFile(t, filepath.Join(h.skillDir("orch-dispatch"), "SKILL.md")); got != shipped(t, "assets/claude/skills/orch-dispatch/SKILL.md") {
					t.Errorf("orch-dispatch skill not installed verbatim")
				}
			},
		},
		{
			name: "legacy file and directory links are replaced, their targets untouched",
			setup: func(t *testing.T, h assetHome) {
				writeFile(t, filepath.Join(h.legacy, "agents", "dev.md"), "legacy dev")
				writeFile(t, filepath.Join(h.legacy, "skills", "orch-dispatch", "SKILL.md"), "legacy skill")
				symlink(t, filepath.Join(h.legacy, "agents", "dev.md"), h.agent("dev"))
				symlink(t, filepath.Join(h.legacy, "skills", "orch-dispatch"), h.skillDir("orch-dispatch"))
			},
			check: func(t *testing.T, h assetHome) {
				if isSymlink(t, h.agent("dev")) || isSymlink(t, h.skillDir("orch-dispatch")) {
					t.Fatalf("legacy links were written through instead of replaced")
				}
				if got := readFile(t, h.agent("dev")); !strings.Contains(got, managedMarker) {
					t.Errorf("dev agent not replaced with the shipped copy")
				}
				if got := readFile(t, filepath.Join(h.legacy, "agents", "dev.md")); got != "legacy dev" {
					t.Errorf("legacy agent target modified: %q", got)
				}
				if got := readFile(t, filepath.Join(h.legacy, "skills", "orch-dispatch", "SKILL.md")); got != "legacy skill" {
					t.Errorf("legacy skill target modified: %q", got)
				}
			},
		},
		{
			name: "relative and dangling legacy links are replaced",
			setup: func(t *testing.T, h assetHome) {
				symlink(t, "../orchestrator/agents/ops.md", h.agent("ops"))
				symlink(t, filepath.Join(h.legacy, "skills", "orch-pr-status"), h.skillDir("orch-pr-status"))
			},
			check: func(t *testing.T, h assetHome) {
				if isSymlink(t, h.agent("ops")) || isSymlink(t, h.skillDir("orch-pr-status")) {
					t.Fatalf("dangling legacy links survived")
				}
				if _, err := os.Stat(h.legacy); !os.IsNotExist(err) {
					t.Errorf("install recreated the retired checkout (err=%v)", err)
				}
			},
		},
		{
			name: "stale managed copy is updated",
			setup: func(t *testing.T, h assetHome) {
				writeFile(t, h.agent("dev"), "---\nname: dev\n---\n<!-- "+managedMarker+" -->\nold\n")
			},
			check: func(t *testing.T, h assetHome) {
				if got := readFile(t, h.agent("dev")); got != shipped(t, "assets/claude/agents/dev.md") {
					t.Errorf("stale managed copy not refreshed")
				}
			},
		},
		{
			name: "gittree is executable even when a stale copy was not",
			setup: func(t *testing.T, h assetHome) {
				writeFile(t, gittreePath(h.home), "#!/bin/sh\n# "+managedMarker+"\n")
			},
			check: func(t *testing.T, h assetHome) {
				info, err := os.Stat(gittreePath(h.home))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o755 {
					t.Errorf("gittree mode = %v, want 0755", info.Mode().Perm())
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAssetHome(t)
			tt.setup(t, h)
			if err := h.plan().Apply(io.Discard, filepath.Join(h.home, "backups")); err != nil {
				t.Fatalf("apply: %v", err)
			}
			tt.check(t, h)
			pending, err := h.plan().Pending()
			if err != nil || pending != 0 {
				t.Errorf("re-plan after apply: pending=%d err=%v, want 0, nil", pending, err)
			}
		})
	}
}

func TestAssetEditsRefuseForeignFiles(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, h assetHome)
		wantErr string
	}{
		{
			name:    "user-owned agent without the marker",
			setup:   func(t *testing.T, h assetHome) { writeFile(t, h.agent("dev"), "my own dev agent") },
			wantErr: "lacks the",
		},
		{
			name: "symlink into a dotfiles repo",
			setup: func(t *testing.T, h assetHome) {
				target := filepath.Join(h.home, "dotfiles", "orch-rag-query")
				writeFile(t, filepath.Join(target, "SKILL.md"), "mine")
				symlink(t, target, h.skillDir("orch-rag-query"))
			},
			wantErr: "does not point into",
		},
		{
			name: "link to a sibling whose name only shares the legacy prefix",
			setup: func(t *testing.T, h assetHome) {
				target := h.legacy + "-fork/agents/dev.md"
				writeFile(t, target, "fork")
				symlink(t, target, h.agent("dev"))
			},
			wantErr: "does not point into",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAssetHome(t)
			tt.setup(t, h)
			err := h.plan().Apply(io.Discard, filepath.Join(h.home, "backups"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("apply err = %v, want one containing %q", err, tt.wantErr)
			}
			// All-or-nothing: no other asset was written either.
			if _, err := os.Stat(gittreePath(h.home)); !os.IsNotExist(err) {
				t.Errorf("plan wrote files despite refusing one (gittree stat err=%v)", err)
			}
		})
	}
}

func TestAdaptersShipAssets(t *testing.T) {
	home := t.TempDir()
	bins := Bins{Orch: "/b/orch", OrchHook: "/b/orch-hook", Orchd: "/b/orchd"}
	tests := []struct {
		adapter    Adapter
		wantAgents bool
	}{
		{ClaudeCode{}, true},
		{Codex{}, false},
		{Gemini{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.adapter.Name(), func(t *testing.T) {
			var gittree, agents bool
			for _, e := range tt.adapter.Edits(bins, home) {
				gittree = gittree || e.Path == gittreePath(home)
				agents = agents || strings.HasPrefix(e.Path, filepath.Join(home, ".claude", "agents"))
			}
			if !gittree {
				t.Errorf("gittree helper not installed")
			}
			if agents != tt.wantAgents {
				t.Errorf("installs Claude agents = %v, want %v", agents, tt.wantAgents)
			}
		})
	}
}

func TestAliasBlockIncludesGittree(t *testing.T) {
	home := t.TempDir()
	next, summary, err := aliasEdit("/opt/orch/orch", home).Compute(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `alias gittree="` + gittreePath(home) + `"`
	if !strings.Contains(string(next), want) || !strings.Contains(summary, "gittree") {
		t.Errorf("alias block missing %s:\n%s", want, next)
	}
}

func TestUnlinkSymlinksLeavesRegularFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	writeFile(t, regular, "keep")
	link := filepath.Join(dir, "link")
	symlink(t, regular, link)
	if err := unlinkSymlinks([]string{regular, link, filepath.Join(dir, "missing")}); err != nil {
		t.Fatalf("unlinkSymlinks: %v", err)
	}
	if got := readFile(t, regular); got != "keep" {
		t.Errorf("regular file disturbed: %q", got)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("symlink not removed (err=%v)", err)
	}
}

func TestGittreeScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	script := filepath.Join(t.TempDir(), "gittree")
	writeFile(t, script, shipped(t, "assets/bin/gittree"))
	repo := filepath.Join(t.TempDir(), "widget")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "init")

	tests := []struct {
		name    string
		branch  string
		wantErr bool
	}{
		{name: "leading dash rejected", branch: "-x", wantErr: true},
		{name: "dot-dot rejected", branch: "a..b", wantErr: true},
		{name: "refspec syntax rejected", branch: "+x:y", wantErr: true},
		{name: "nested branch creates worktree beside the repo", branch: "feat/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("bash", script, tt.branch)
			cmd.Dir = repo
			out, err := cmd.CombinedOutput()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected failure, got success:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("gittree %s: %v\n%s", tt.branch, err, out)
			}
			wt := filepath.Join(filepath.Dir(repo), "widget.worktrees", "feat", "x")
			if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
				t.Errorf("worktree not created at %s: %v", wt, err)
			}
		})
	}
}

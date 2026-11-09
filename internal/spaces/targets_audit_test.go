package spaces

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The cases below came from the security review of SP-013: command forms
// that reached another job's space without being reported. Each must
// either name the other space's directory or be flagged opaque.
const (
	auditCwd   = "/w/code/myco/proj"
	auditHome  = "/w"
	auditOther = "/w/code/otherco"
)

// reaches reports whether t names dir, a path under it, or a prefix that
// contains it.
func reaches(t ToolTargets, dir string) bool {
	under := func(p string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }
	for _, l := range [][]string{t.Paths, t.CdDest, t.GitDirs} {
		if slices.ContainsFunc(l, under) {
			return true
		}
	}
	return slices.ContainsFunc(t.Prefixes, func(p string) bool {
		p = strings.TrimSuffix(p, "/")
		return under(p) || strings.HasPrefix(dir+"/", p+"/")
	})
}

func shellArgv(argv ...string) string {
	b, _ := json.Marshal(map[string]any{"command": argv})
	return string(b)
}

func TestTargetsAuditBypassesCaught(t *testing.T) {
	cases := []struct{ name, tool, input string }{
		{"setsid bash -c", "Bash", `setsid bash -c 'cat /w/code/otherco/s'`},
		{"watch", "Bash", `watch -n1 'cat /w/code/otherco/s'`},
		{"su -c", "Bash", `su me -c 'cat /w/code/otherco/s'`},
		{"find -exec sh -c", "Bash", `find . -exec sh -c 'cat /w/code/otherco/s' \;`},
		{"tcsh -c", "Bash", `tcsh -c 'cat /w/code/otherco/s'`},
		{"pwsh", "Bash", `pwsh -Command "Get-Content /w/code/otherco/s"`},
		{"bash --rcfile", "Bash", `bash --rcfile /dev/null -c 'cat /w/code/otherco/s'`},
		{"fish --command", "Bash", `fish --command 'cat /w/code/otherco/s'`},
		{"python -Ic", "Bash", `python3 -Ic "print(open('/w/code/otherco/secret.txt').read())"`},
		{"perl -le", "Bash", `perl -le 'open F,"/w/code/otherco/s"; print <F>'`},
		{"node -pe", "Bash", `node -pe "require('fs').readFileSync('/w/code/otherco/s')"`},
		{"awk getline", "Bash", `awk 'BEGIN{while((getline l < "/w/code/otherco/s")>0) print l}'`},
		{"awk computed", "Bash", `awk 'BEGIN{f="/w/code/" "otherco/s"; while((getline l < f)>0) print l}'`},
		{"sed r", "Bash", `sed '1r /w/code/otherco/s' README.md`},
		{"make --eval", "Bash", `make --eval='x: ; cat /w/code/otherco/s' x`},
		{"sqlite3 .shell", "Bash", `sqlite3 :memory: ".shell cat /w/code/otherco/s"`},
		{"trap", "Bash", `trap 'cat /w/code/otherco/s' EXIT`},
		{"alias", "Bash", "shopt -s expand_aliases\nalias x='cat $HOME/code/otherco/s'\nx"},
		{"git -c alias", "Bash", `git -c alias.x='!cat ~/code/otherco/s' x`},
		{"git config alias", "Bash", `git config alias.x '!sh -c "cd ../.. && cat otherco/s"' && git x`},
		{"git -c core.hooksPath", "Bash", `git -c core.hooksPath=/w/code/otherco/hooks commit`},
		{"docker -v", "Bash", `docker run -v /w/code/otherco:/w alpine cat /w/s`},
		{"source /dev/stdin", "Bash", `source /dev/stdin <<< 'cat /w/code/otherco/s'`},
		{"bash /dev/stdin", "Bash", `bash /dev/stdin <<< "cd ../.. && cat otherco/s"`},
		{"write then run", "Bash", `echo 'cd ../.. && cat otherco/s' > s.sh && sh s.sh`},
		{"cd in loop", "Bash", `for i in 1 2; do cd ..; done; cat otherco/s`},
		{"cd in function", "Bash", `up(){ cd ..; }; up; up; cat otherco/s`},
		{"skipped cd", "Bash", `true || cd sub; cd ../.. && cat otherco/s`},
		{"failed cd", "Bash", `cd nonexistent; cat ../../otherco/s`},
		{"ln then use", "Bash", `ln -s ../.. up && cat up/otherco/s`},
		{"apply_patch argv", "shell", shellArgv("apply_patch", "*** Begin Patch\n*** Add File: /w/code/otherco/x\n+x\n*** End Patch")},
		{"apply_patch heredoc", "shell", shellArgv("bash", "-lc", "apply_patch <<'EOF'\n*** Begin Patch\n*** Add File: ../../otherco/x\n+x\n*** End Patch\nEOF")},
		{"mcp move_file", "mcp__filesystem__move_file", `{"source":"/w/code/otherco/s","destination":"/tmp/x"}`},
		{"file uri", "mcp__ide__getDiagnostics", `{"uri":"file:///w/code/otherco/s"}`},
		{"mcp shell", "mcp__shell__run", `{"command":"cd ../.. && cat otherco/s"}`},
		{"codex exec_command", "exec_command", `{"cmd":"cat /w/code/otherco/s"}`},
		{"codex write_stdin", "write_stdin", `{"chars":"cd ../.. && cat otherco/s\n"}`},
		{"zsh cd substitution", "Bash", `cd myco otherco && cat secret.txt`},
		{"glob in variable", "Bash", `p='/w/code/othe*/s'; cat $p`},
		{"CDPATH", "Bash", `export CDPATH=x:/w/code; cd otherco && cat s`},
		{"nameref", "Bash", `declare -n r=HOME; cat $r/code/otherco/s`},
		{"sudo -D attached", "Bash", `sudo -D/w/code/otherco cat s`},
		{"env -C attached", "Bash", `env -C/w/code/otherco cat s`},
		{"time -o attached", "Bash", `/usr/bin/time -o/w/code/otherco/out ls`},
		{"git -c url insteadOf", "Bash", `git -c url.git@github.com:otherco/.insteadOf=x: clone x:secret`},
		{"git remote other host", "Bash", `git clone git@gitlab.example:otherco/secret.git`},
		{"gh api repositories", "Bash", `gh api /repositories/123456/contents`},
		{"tilde user in braces", "Bash", `ls {~root/a,x}`},
		{"interpreter script stdin", "Bash", `echo 'open("/w/code/otherco/s")' | python3`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.input
			if c.tool == "Bash" {
				_, in = bash(c.input)
			}
			got := extractTargets(c.tool, json.RawMessage(in), auditCwd, auditHome)
			if !got.Opaque && !reaches(got, auditOther) {
				t.Errorf("neither opaque nor reaching %s: %+v", auditOther, got)
			}
		})
	}
}

func TestTargetsAuditPrecise(t *testing.T) {
	run := func(cmd string) ToolTargets {
		_, in := bash(cmd)
		return extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome)
	}
	if got := run(`GH_REPO=otherco/secret gh issue list`); !slices.Contains(got.GHRepos, "otherco/secret") {
		t.Errorf("GH_REPO: repos %q", got.GHRepos)
	}
	if got := run(`git clone git@github-otherco:otherco/secret.git`); !slices.Contains(got.GHRepos, "otherco/secret") || got.Opaque {
		t.Errorf("ssh host alias: %+v", got)
	}
	if got := run(`git show HEAD:docs/x.md`); got.Opaque {
		t.Errorf("rev:path read as a remote: %q", got.OpaqueReasons)
	}
	if got := run(`[ -d x ] && echo y`); got.Opaque {
		t.Errorf("test command opaque: %q", got.OpaqueReasons)
	}
	if got := run(`git -c user.name=x commit -m "fix: a/b"`); got.Opaque {
		t.Errorf("safe git -c opaque: %q", got.OpaqueReasons)
	}
	if got := run(`git clone https://github.com/acme/x d`); got.Opaque {
		t.Errorf("plain clone opaque: %q", got.OpaqueReasons)
	}
	if got := run(`tar xf a.tar && cat l/x`); !got.Opaque {
		t.Error("path after extraction not opaque")
	}
	if got := run(`cd nonexistent; cat ../../otherco/s`); !slices.Contains(got.Paths, "/w/code/otherco/s") {
		t.Errorf("failed cd: paths %q", got.Paths)
	}
	if got := run(`ls {~root/a,x}`); slices.ContainsFunc(got.Paths, func(p string) bool { return strings.Contains(p, "root/a") && !strings.HasPrefix(p, auditCwd) }) {
		t.Errorf("~root expanded: %q", got.Paths)
	}
	if got := run(`p='/w/code/othe*/s'; cat $p`); !slices.Contains(got.Prefixes, "/w/code") {
		t.Errorf("glob variable prefix: %q", got.Prefixes)
	}
}

func TestTargetsBraceBomb(t *testing.T) {
	_, in := bash("cat " + strings.Repeat("{a,b}", 20))
	got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome)
	if !got.Opaque {
		t.Error("brace bomb not opaque")
	}
}

func TestTargetsGitVariables(t *testing.T) {
	_, in := bash(`GIT_DIR=/a/.git GIT_WORK_TREE=$X git log`)
	got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome)
	if !slices.Contains(got.GitDirs, "/a/.git") || !got.Opaque {
		t.Errorf("GIT_DIR: %+v", got)
	}
	_, in = bash(`GIT_SSH_COMMAND='ssh -i k' git fetch`)
	if got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome); !got.Opaque {
		t.Error("GIT_SSH_COMMAND not opaque")
	}
}

func TestTargetsTimeBudget(t *testing.T) {
	e := &extractor{home: auditHome, res: newPathResolver(), seen: map[string]bool{}, written: map[string]bool{}}
	e.add(kindPath, "/a")
	got := e.result()
	if !got.Opaque || len(got.Paths) != 0 {
		t.Errorf("past deadline: %+v", got)
	}
}

// Round 3 of the SP-013 security review: cost, false opaque results and
// the written-script check.
func TestTargetsAuditRound3Cost(t *testing.T) {
	step := `a="` + strings.Repeat("$a", 150) + `"; `
	_, in := bash(`a='x/y z/w x/y z/w '; ` + step + step + step)
	start := time.Now()
	got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome)
	if d := time.Since(start); d > 2*budget {
		t.Errorf("variable growth took %v", d)
	}
	if !got.Opaque {
		t.Error("variable growth not opaque")
	}

	content, _ := json.Marshal(map[string]string{"file_path": "/w/x.go",
		"content": strings.Repeat("import \"a/b/c\"\n", 2<<20/15)})
	start = time.Now()
	got = extractTargets("Write", content, auditCwd, auditHome)
	if d := time.Since(start); d > 2*budget {
		t.Errorf("2 MB Write took %v", d)
	}
	if !got.Opaque {
		t.Error("over-long Write content not opaque")
	}
}

func TestTargetsAuditRound3NotOpaque(t *testing.T) {
	cwd := realTemp(t)
	for _, cmd := range []string{
		`git switch main && go test ./...`,
		`git checkout -b feat && touch new.txt`,
		`git stash && echo hi > out.log`,
		`git commit -m "ln -s a b"`,
		`git checkout -b feat/x && git commit -m "touch docs/new/file.md"`,
		`python3 -m pip install -r requirements.txt`,
		`python3 -m pip install -e .`,
		`python3 manage.py runserver -p 8000`,
		`node build.js --print`,
		`python --version`,
		`ruby -v`,
		"git commit -m \"$(cat <<'EOF'\nfix: thing\n\nbody text\nEOF\n)\"",
	} {
		_, in := bash(cmd)
		if got := extractTargets("Bash", json.RawMessage(in), cwd, auditHome); got.Opaque {
			t.Errorf("%q opaque: %q", cmd, got.OpaqueReasons)
		}
	}
}

func TestTargetsAuditRound3CommitHeredocScanned(t *testing.T) {
	_, in := bash("git commit -m \"$(cat <<'EOF'\nsee /w/code/otherco/s\nEOF\n)\"")
	got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome)
	if !slices.Contains(got.Paths, "/w/code/otherco/s") {
		t.Errorf("heredoc body not searched: %q", got.Paths)
	}
	_, in = bash("git commit -m \"$(cat <<EOF\n$HOME\nEOF\n)\"")
	if got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome); !got.Opaque {
		t.Error("unquoted heredoc delimiter treated as literal")
	}
}

func TestTargetsAuditRound3WrittenScripts(t *testing.T) {
	root := realTemp(t)
	for _, f := range []string{"s.sh", "x.py"} {
		writeFile(t, filepath.Join(root, f), "echo old\n")
	}
	for _, cmd := range []string{
		`echo X > ./s.sh && bash s.sh`,
		`> s.sh && bash ./s.sh`,
		`tee ./s.sh < /dev/null && source s.sh`,
		`printf 'x' > x.py && python3 x.py`,
	} {
		_, in := bash(cmd)
		if got := extractTargets("Bash", json.RawMessage(in), root, auditHome); !got.Opaque {
			t.Errorf("%q not opaque", cmd)
		}
	}
}

func TestTargetsAuditRound3EmptyAssignments(t *testing.T) {
	for _, cmd := range []string{`HOME=; cd; cat s`, `GIT_DIR= git status`, `BASH_ENV= bash x`} {
		_, in := bash(cmd)
		got := extractTargets("Bash", json.RawMessage(in), auditCwd, auditHome)
		if !got.Opaque && !strings.HasPrefix(cmd, "GIT_DIR") {
			t.Errorf("%q not opaque", cmd)
		}
		for _, l := range [][]string{got.Paths, got.CdDest, got.GitDirs, got.Prefixes} {
			for _, p := range l {
				if !strings.HasPrefix(p, "/") {
					t.Errorf("%q: relative target %q", cmd, p)
				}
			}
		}
	}
}

func TestTargetsMakeFileFlag(t *testing.T) {
	root := realTemp(t)
	writeFile(t, filepath.Join(root, "other.mk"), "x:\n\tcat /mk/secret\n")
	_, in := bash("make -f other.mk x")
	got := extractTargets("Bash", json.RawMessage(in), root, auditHome)
	if !slices.Contains(got.Paths, "/mk/secret") {
		t.Errorf("make -f file not read: %q", got.Paths)
	}
}

package spaces

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// auditRound2 checks one case from the second security review of SP-013:
// it must name the other space (or its repo) or be opaque.
func auditRound2(t *testing.T, name string, r ToolTargets, dir, repo string) {
	t.Helper()
	hit := reaches(r, dir) || (repo != "" && slices.Contains(r.GHRepos, repo))
	status := "caught"
	if !hit && !r.Opaque {
		status = "BYPASS"
	} else if !hit {
		status = "opaque"
	}
	if status == "BYPASS" {
		b, _ := json.Marshal(r)
		t.Errorf("%s: neither opaque nor reaching %s: %s", name, dir, b)
	}
}

func TestTargetsAuditRound2Strings(t *testing.T) {
	cases := []struct{ name, tool, input, repo string }{
		{"arch wrapper", "Bash", `arch -arm64 sh -c 'cd ..; cd ..; cd otherco; cat s'`, ""},
		{"trap --", "Bash", `trap -- 'cd ..; cd ..; cd otherco; cat s' EXIT`, ""},
		{"env -P", "Bash", `env -P bin sh -c 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"sudo -T", "Bash", `sudo -T 10 sh -c 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"gtimeout", "Bash", `gtimeout 5 sh -c 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"uv run", "Bash", `uv run sh -c 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"script(1)", "Bash", `script -q /dev/null sh -c 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"ssh localhost", "Bash", `ssh localhost 'cd code/otherco; cat s'`, ""},
		{"tmux", "Bash", `tmux new -d 'cd ..;cd ..;cd otherco;cat s >out'`, ""},
		{"hyperfine", "Bash", `hyperfine 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"watch -x", "Bash", `watch -x sh -c 'cd ..; cd ..; cd otherco; cat s'`, ""},
		{"lastpipe", "Bash", "shopt -s lastpipe; true | cd ..; cd ../otherco; cat s", ""},
		{"zsh last pipe", "Bash", "true | cd ..; cd ../otherco; cat s", ""},
		{"printf -v", "Bash", `d=.; printf -v d %s ..; cd "$d/../otherco"; cat s`, ""},
		{"for var", "Bash", `d=.; for d in ..; do :; done; cat "$d/../otherco/s"`, ""},
		{"read var", "Bash", `d=.; read d <<< ..; cat "$d/../otherco/s"`, ""},
		{"unset var", "Bash", `d=.; unset d; cd $d; cd code/otherco; cat s`, ""},
		{"cdable_vars", "Bash", `shopt -s cdable_vars; up=../..; cd up; cat otherco/s`, ""},
		{"zsh cdpath", "Bash", `cdpath=(../..); cd otherco; cat s`, ""},
		{"zsh subscript", "Bash", `d=..x; cat $d[1,2]/../otherco/s`, ""},
		{"EDITOR git commit", "Bash", `EDITOR='cd ..; cd ..; cd otherco; cat s >&2; :' git commit --allow-empty`, ""},
		{"VISUAL", "Bash", `VISUAL='cd ..; cd ..; cd otherco; cat s >&2; :' git commit --allow-empty`, ""},
		{"GIT_PAGER", "Bash", `GIT_PAGER='cd ..; cd ..; cd otherco; cat s' git -p log`, ""},
		{"git clone -c", "Bash", `git clone -c core.sshCommand='cd ..;cd ..;cd otherco;cat s>&2; ssh' git@github.com:myco/proj x`, ""},
		{"git fetch upload-pack", "Bash", `git fetch --upload-pack='cd ..;cd ..;cd otherco;cat s>&2; git-upload-pack' .`, ""},
		{"git rebase -x", "Bash", `git rebase -x 'cd ..;cd ..;cd otherco;cat s' HEAD~1`, ""},
		{"git bisect run", "Bash", `git bisect run sh -c 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"git submodule foreach", "Bash", `git submodule foreach 'cd ..;cd ..;cd otherco;cat s'`, ""},
		{"make cmdline :=", "Bash", `make -f /dev/null 'X:=$(shell cd ..;cd ..;cd otherco;cat s>&2)'`, ""},
		{"make -f - heredoc", "Bash", "make -f - <<'EOF'\nall:\n\tcd .. && cd .. && cd otherco && cat s\nEOF", ""},
		{"deno eval", "Bash", `deno eval "Deno.chdir('..');Deno.chdir('..');Deno.chdir('otherco');console.log(Deno.readTextFileSync('s'))"`, ""},
		{"node --eval= file", "Bash", `node --eval='process.chdir("..");process.chdir("..");process.chdir("otherco");console.log(require("fs").readFileSync("s","utf8"))' x.js`, ""},
		{"julia -e", "Bash", `julia -e 'cd("..");cd("..");cd("otherco");print(read("s",String))'`, ""},
		{"php -R", "Bash", `echo | php -R 'chdir("..");chdir("..");chdir("otherco");echo file_get_contents("s");'`, ""},
		{"gsed e", "Bash", `gsed -n '1e cd ..;cd ..;cd otherco;cat s' README.md`, ""},
		{"gh alias", "Bash", `gh alias set z '!cd ..;cd ..;cd otherco;cat s' && gh z`, ""},
		{"tar -T here-string", "Bash", `tar -cf o.tar -T - <<< /w/code/otherco/s`, ""},
		{"gh api networks", "Bash", `gh api networks/otherco/secret/events`, "otherco/secret"},
		{"gh api user/repos", "Bash", `gh api user/repos`, "otherco/secret"},
		{"gh search", "Bash", `gh search code 'org:otherco password'`, "otherco/secret"},
		{"gh repo list", "Bash", `gh repo list otherco`, "otherco/secret"},
		{"mcp project abs", "mcp__serena__activate_project", `{"project":"/w/code/otherco"}`, ""},
		{"mcp args abs", "mcp__x__run", `{"args":["/w/code/otherco/s"]}`, ""},
		{"mcp location rel", "mcp__x__open", `{"location":"../../otherco/s"}`, ""},
		{"github mcp", "mcp__github__get_file_contents", `{"owner":"otherco","repo":"secret","path":"README.md"}`, "otherco/secret"},
		{"Read x:// rel", "Read", `{"file_path":"x://../../../otherco/s"}`, ""},
		{"Read x:// abs", "Read", `{"file_path":"/w/code/myco/proj/x://../../../otherco/s"}`, ""},
		{"apply_patch indent", "apply_patch", `{"input":"*** Begin Patch\n  *** Update File: ../../otherco/s\n@@\n-a\n+b\n*** End Patch"}`, ""},
		{"exec_command pwsh", "exec_command", `{"cmd":"Set-Location ..; Set-Location ..; Set-Location otherco; Get-Content s","shell":"pwsh"}`, ""},
	}
	for _, c := range cases {
		r := extractTargets(c.tool, json.RawMessage(toJSONInput(c.tool, c.input)), auditCwd, auditHome)
		auditRound2(t, c.name, r, auditOther, c.repo)
	}
}

func toJSONInput(tool, in string) string {
	if tool == "Bash" {
		b, _ := json.Marshal(map[string]string{"command": in})
		return string(b)
	}
	return in
}

func TestTargetsAuditRound2Files(t *testing.T) {
	root := realTemp(t)
	proj := filepath.Join(root, "code/myco/proj")
	other := filepath.Join(root, "code/otherco")
	mkdirs(t, proj+"/a/b/c", proj+"/x", other)
	writeFile(t, other+"/s", "secret")
	// NUL after the first line: bash still runs it.
	writeFile(t, proj+"/nul.sh", "#!/bin/sh\n# \x00\ncd ../..\ncd otherco\ncat s\n")
	writeFile(t, proj+"/c.sh", "#cd ../../otherco\n#cat s\n")
	if err := os.Symlink("../../../otherco", proj+"/a/b/c/l"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, cmd string }{
		{"NUL script bash", "bash nul.sh"},
		{"NUL script source", ". ./nul.sh"},
		{"NUL script ./", "./nul.sh"},
		{"sed -i then run", "sed -i '' 's/^#//' c.sh && bash c.sh"},
		{"cp then run", "cp c.sh d.sh && bash c.sh"},
		{"mv relative link", "mv a/b/c/l x/l && cat x/l/s"},
		{"cp -R relative link", "cp -R a/b/c/l x/l && cat x/l/s"},
	}
	for _, c := range cases {
		b, _ := json.Marshal(map[string]string{"command": c.cmd})
		r := extractTargets("Bash", b, proj, root)
		auditRound2(t, c.name, r, other, "")
	}
}

func TestTargetsAuditRound2Scripts(t *testing.T) {
	for _, c := range []struct{ name, cmd string }{
		{"PS4 env", `PS4='$(cd ..;cd ..;cd otherco;cat s >&2)' bash -xc true`},
		{"PS4 set -x", `PS4='$(cd ..;cd ..;cd otherco;cat s >&2)'; set -x; true`},
		{"LESSOPEN", `LESSOPEN='|cd ..;cd ..;cd otherco;cat s; : %s' less README.md`},
		{"HOME dotfiles", `HOME=. git status`},
	} {
		b, _ := json.Marshal(map[string]string{"command": c.cmd})
		auditRound2(t, c.name, extractTargets("Bash", b, auditCwd, auditHome), auditOther, "")
	}
	root := realTemp(t)
	proj := filepath.Join(root, "code/myco/proj")
	other := filepath.Join(root, "code/otherco")
	mkdirs(t, proj, other)
	writeFile(t, proj+"/c.sh", "echo hi\n")
	writeFile(t, proj+"/evil.txt", "cd ..\ncd ..\ncd otherco\ncat s\n")
	writeFile(t, proj+"/y.sh", "true\n")
	writeFile(t, proj+"/fan.sh", strings.Repeat(". ./y.sh\n", 6000))
	for _, c := range []struct{ name, cmd string }{
		{"cp over then run", "cp evil.txt c.sh && bash c.sh"},
		{"tee over then run", "tee c.sh < evil.txt >/dev/null && sh c.sh"},
		{"fan-out sources", "bash fan.sh"},
	} {
		b, _ := json.Marshal(map[string]string{"command": c.cmd})
		start := time.Now()
		r := extractTargets("Bash", b, proj, root)
		if d := time.Since(start); d > 2*budget {
			t.Errorf("%s took %v", c.name, d)
		}
		auditRound2(t, c.name, r, other, "")
	}
}

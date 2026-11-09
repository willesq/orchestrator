package spaces

import (
	"encoding/json"
	"mvdan.cc/sh/v3/expand"
	"slices"
	"testing"
)

const (
	testCwd  = "/scratch/project"
	testHome = "/h/u"
)

func bash(cmd string) (string, string) {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return "Bash", string(b)
}

func toolIn(tool, input string) (string, string) { return tool, input }

// targetCase lists targets that must be present in each category, targets
// that must not be in Paths, and whether the call is opaque. Shell
// commands also list their non-path words as paths under the cwd, so
// cases name what matters rather than every entry.
type targetCase struct {
	name     string
	tool     string
	input    string
	paths    []string
	notPaths []string
	cd       []string
	git      []string
	repos    []string
	prefixes []string
	opaque   bool
}

func tc(name string, call func() (string, string), c targetCase) targetCase {
	c.name = name
	c.tool, c.input = call()
	return c
}

func targetCases() []targetCase {
	return []targetCase{
		// cd and directory stack
		tc("cd relative", func() (string, string) { return bash("cd sub") }, targetCase{cd: []string{"/scratch/project/sub"}}),
		tc("cd absolute", func() (string, string) { return bash("cd /scratch/outside") }, targetCase{cd: []string{"/scratch/outside"}}),
		tc("cd home", func() (string, string) { return bash("cd") }, targetCase{cd: []string{"/h/u"}}),
		tc("cd tilde", func() (string, string) { return bash("cd ~/code/x") }, targetCase{cd: []string{"/h/u/code/x"}}),
		tc("cd dotdot", func() (string, string) { return bash("cd ../other") }, targetCase{cd: []string{"/scratch/other"}}),
		tc("cd then relative path", func() (string, string) { return bash("cd /scratch/outside && ls .") },
			targetCase{cd: []string{"/scratch/outside"}, paths: []string{"/scratch/outside"}}),
		tc("cd then cat relative", func() (string, string) { return bash("cd /a; cat b/c.txt") },
			targetCase{paths: []string{"/a/b/c.txt", "/scratch/project/b/c.txt"}}),
		tc("cd chain", func() (string, string) { return bash("cd /a && cd b && cat f") },
			targetCase{cd: []string{"/a", "/a/b"}, paths: []string{"/a/b/f"}}),
		tc("cd in subshell does not leak", func() (string, string) { return bash("(cd /a); cat f") },
			targetCase{cd: []string{"/a"}, paths: []string{"/scratch/project/f"}, notPaths: []string{"/a/f"}}),
		tc("cd in pipeline does not leak", func() (string, string) { return bash("cd /a | true; cat f") },
			targetCase{paths: []string{"/scratch/project/f"}, notPaths: []string{"/a/f"}}),
		tc("cd in block persists", func() (string, string) { return bash("{ cd /a; }; cat f") },
			targetCase{paths: []string{"/a/f"}}),
		tc("cd in if body", func() (string, string) { return bash("if true; then cd /a; fi; cat f") },
			targetCase{cd: []string{"/a"}, paths: []string{"/a/f"}}),
		tc("cd minus after cd", func() (string, string) { return bash("cd /a && cd /b && cd - && cat f") },
			targetCase{cd: []string{"/a", "/b"}, paths: []string{"/a/f"}}),
		tc("cd minus unknown", func() (string, string) { return bash("cd -") }, targetCase{opaque: true}),
		tc("cd -P flag", func() (string, string) { return bash("cd -P /a/b") }, targetCase{cd: []string{"/a/b"}}),
		tc("pushd and popd", func() (string, string) { return bash("pushd /a && pushd /b && popd && cat f") },
			targetCase{cd: []string{"/a", "/b"}, paths: []string{"/a/f"}}),
		tc("popd empty stack", func() (string, string) { return bash("popd") }, targetCase{opaque: true}),
		tc("pushd rotate", func() (string, string) { return bash("pushd +1") }, targetCase{opaque: true}),
		tc("cd quoted with space", func() (string, string) { return bash(`cd "/a b/c"`) }, targetCase{cd: []string{"/a b/c"}}),
		tc("cd escaped space", func() (string, string) { return bash(`cd /a\ b`) }, targetCase{cd: []string{"/a b"}}),

		// paths in arguments and redirections
		tc("absolute path", func() (string, string) { return bash("cat /scratch/outside/file.txt") },
			targetCase{paths: []string{"/scratch/outside/file.txt"}}),
		tc("tilde path", func() (string, string) { return bash("cat ~/.ssh/config") }, targetCase{paths: []string{"/h/u/.ssh/config"}}),
		tc("relative path", func() (string, string) { return bash("cat notes/a.md") },
			targetCase{paths: []string{"/scratch/project/notes/a.md"}}),
		tc("dotdot path", func() (string, string) { return bash("cat ../../etc/passwd") }, targetCase{paths: []string{"/etc/passwd"}}),
		tc("HOME variable", func() (string, string) { return bash("cat $HOME/x ${HOME}/y") },
			targetCase{paths: []string{"/h/u/x", "/h/u/y"}}),
		tc("redirect out", func() (string, string) { return bash("echo hi > /a/out.txt") }, targetCase{paths: []string{"/a/out.txt"}}),
		tc("redirect append and in", func() (string, string) { return bash("sort < /a/in >> /b/out") },
			targetCase{paths: []string{"/a/in", "/b/out"}}),
		tc("fd dup is not a path", func() (string, string) { return bash("ls 2>&1") },
			targetCase{notPaths: []string{"/scratch/project/1"}}),
		tc("heredoc delimiter is not a path", func() (string, string) { return bash("cat <<EOF\nhello\nEOF") },
			targetCase{notPaths: []string{"/scratch/project/EOF"}}),
		tc("heredoc body paths kept", func() (string, string) { return bash("cat <<EOF\nsee /a/b and ../x/y\nEOF") },
			targetCase{paths: []string{"/a/b", "/scratch/x/y"}}),
		tc("brace expansion", func() (string, string) { return bash("cat /a/{x,y}.md") },
			targetCase{paths: []string{"/a/x.md", "/a/y.md"}}),
		tc("glob gives prefix", func() (string, string) { return bash("cat ~/code/*/secrets") },
			targetCase{prefixes: []string{"/h/u/code"}}),
		tc("quoted glob is literal", func() (string, string) { return bash("cat '/a/*.txt'") },
			targetCase{paths: []string{"/a/*.txt"}}),
		tc("long option value", func() (string, string) { return bash("tool --config=/z/tool.conf") },
			targetCase{paths: []string{"/z/tool.conf"}}),
		tc("short option attached path", func() (string, string) { return bash("cc -I/opt/include x.c") },
			targetCase{paths: []string{"/opt/include", "/scratch/project/x.c"}}),
		tc("missing script path run", func() (string, string) { return bash("/z/bin/tool run") },
			targetCase{paths: []string{"/z/bin/tool"}, opaque: true}),
		tc("assignment then use", func() (string, string) { return bash("D=/a/b; cat $D/f") },
			targetCase{paths: []string{"/a/b/f"}}),
		tc("export then use", func() (string, string) { return bash("export D=/a; cd $D") }, targetCase{cd: []string{"/a"}}),
		tc("for loop list", func() (string, string) { return bash("for f in /a /b; do echo $f; done") },
			targetCase{paths: []string{"/a", "/b"}, opaque: true}),
		tc("test clause", func() (string, string) { return bash("[[ -f /a/x ]] && echo y") }, targetCase{paths: []string{"/a/x"}}),
		tc("command substitution walked", func() (string, string) { return bash("echo $(cat /a/x)") },
			targetCase{paths: []string{"/a/x"}, opaque: true}),
		tc("prefix wrapper sudo", func() (string, string) { return bash("sudo -u root cat /z/x") }, targetCase{paths: []string{"/z/x"}}),
		tc("env -C", func() (string, string) { return bash("env -C /a FOO=1 cat f") },
			targetCase{cd: []string{"/a"}, paths: []string{"/a/f"}}),
		tc("env --chdir=", func() (string, string) { return bash("env --chdir=/a ls") }, targetCase{cd: []string{"/a"}}),
		tc("timeout wrapper", func() (string, string) { return bash("timeout 5 git -C /a status") }, targetCase{git: []string{"/a"}}),
		tc("rtk wrapper", func() (string, string) { return bash("rtk git -C /scratch/project/sub log --oneline -1 .") },
			targetCase{git: []string{"/scratch/project/sub"}, paths: []string{"/scratch/project/sub"}}),

		// git and make
		tc("git -C", func() (string, string) { return bash("git -C /scratch/project/sub log --oneline -1 .") },
			targetCase{git: []string{"/scratch/project/sub"}, paths: []string{"/scratch/project/sub"},
				notPaths: []string{"/scratch/project/sub/log"}}),
		tc("git -C cumulative", func() (string, string) { return bash("git -C /a -C b status") },
			targetCase{git: []string{"/a", "/a/b"}}),
		tc("git --git-dir and --work-tree", func() (string, string) { return bash("git --git-dir=/a/.git --work-tree /a status") },
			targetCase{git: []string{"/a/.git", "/a"}}),
		tc("git -c skipped", func() (string, string) { return bash("git -c user.name=x -C /a commit") },
			targetCase{git: []string{"/a"}}),
		tc("git clone url", func() (string, string) { return bash("git clone git@github.com:Acme/Widgets.git") },
			targetCase{repos: []string{"acme/widgets"}}),
		tc("make -C", func() (string, string) { return bash("make -C /a test") }, targetCase{cd: []string{"/a"}}),
		tc("make --directory=", func() (string, string) { return bash("make --directory=../b") }, targetCase{cd: []string{"/scratch/b"}}),
		tc("make -Cdir", func() (string, string) { return bash("make -C/a") }, targetCase{cd: []string{"/a"}}),

		// gh
		tc("gh -R", func() (string, string) { return bash("gh pr list -R globex/api") }, targetCase{repos: []string{"globex/api"}}),
		tc("gh --repo=", func() (string, string) { return bash("gh issue list --repo=globex/api") }, targetCase{repos: []string{"globex/api"}}),
		tc("gh --repo host form", func() (string, string) { return bash("gh pr view 1 --repo github.com/Globex/API") },
			targetCase{repos: []string{"globex/api"}}),
		tc("gh api repos", func() (string, string) { return bash("gh api repos/acme/site/pulls?state=open") },
			targetCase{repos: []string{"acme/site"}}),
		tc("gh api leading slash", func() (string, string) { return bash("gh api -X GET /repos/acme/site/issues") },
			targetCase{repos: []string{"acme/site"}}),
		tc("gh api placeholder", func() (string, string) { return bash("gh api repos/{owner}/{repo}/pulls") }, targetCase{opaque: true}),
		tc("gh api graphql", func() (string, string) { return bash(`gh api graphql -f query='{viewer{login}}'`) },
			targetCase{opaque: true}),
		tc("gh repo clone", func() (string, string) { return bash("gh repo clone globex/api /a/dir") },
			targetCase{repos: []string{"globex/api"}, paths: []string{"/a/dir"}}),
		tc("gh pr url", func() (string, string) { return bash("gh pr view https://github.com/acme/x/pull/3") },
			targetCase{repos: []string{"acme/x"}}),
		tc("curl api url", func() (string, string) { return bash("curl https://api.github.com/repos/globex/api/pulls") },
			targetCase{repos: []string{"globex/api"}}),

		// nested shells and opaque forms
		tc("bash -c literal", func() (string, string) { return bash(`bash -c "cd /a && cat f"`) },
			targetCase{cd: []string{"/a"}, paths: []string{"/a/f"}}),
		tc("bash -lc literal", func() (string, string) { return bash(`bash -lc 'cat /a/x'`) }, targetCase{paths: []string{"/a/x"}}),
		tc("bash -c cd does not leak", func() (string, string) { return bash(`bash -c "cd /a"; cat f`) },
			targetCase{paths: []string{"/scratch/project/f"}, notPaths: []string{"/a/f"}}),
		tc("bash -c with variable", func() (string, string) { return bash(`bash -c "$CMD"`) }, targetCase{opaque: true}),
		tc("eval", func() (string, string) { return bash(`eval "cat /a/x"`) }, targetCase{paths: []string{"/a/x"}, opaque: true}),
		tc("base64 to shell", func() (string, string) { return bash("echo Y2F0IC9ldGMvcGFzc3dk | base64 -d | sh") },
			targetCase{opaque: true}),
		tc("base64 decode alone", func() (string, string) { return bash("base64 --decode x.b64") }, targetCase{opaque: true}),
		tc("base64 encode is fine", func() (string, string) { return bash("base64 /a/x") }, targetCase{paths: []string{"/a/x"}}),
		tc("python -c", func() (string, string) { return bash(`python3 -c "open('/a/x')"`) }, targetCase{opaque: true}),
		tc("python script", func() (string, string) { return bash("python3 /a/run.py") }, targetCase{paths: []string{"/a/run.py"}}),
		tc("xargs", func() (string, string) { return bash("ls | xargs cat") }, targetCase{opaque: true}),
		tc("unknown variable", func() (string, string) { return bash("cat /a/$NAME/f") },
			targetCase{prefixes: []string{"/a"}, opaque: true}),
		tc("cd unknown variable", func() (string, string) { return bash("cd $DIR") }, targetCase{opaque: true}),
		tc("tilde user", func() (string, string) { return bash("cat ~root/x") }, targetCase{opaque: true}),
		tc("command name variable", func() (string, string) { return bash("$EDITOR /a/x") },
			targetCase{paths: []string{"/a/x"}, opaque: true}),
		tc("unparseable keeps absolute paths", func() (string, string) { return bash("cat /a/x && (") },
			targetCase{prefixes: []string{"/a"}, opaque: true}),
		tc("heredoc substitution walked", func() (string, string) { return bash("cat <<EOF\n$(cat /a/x)\nEOF") },
			targetCase{paths: []string{"/a/x"}, opaque: true}),
		tc("herestring substitution walked", func() (string, string) { return bash("tr a b <<< \"$(cat /a/x)\"") },
			targetCase{paths: []string{"/a/x"}, opaque: true}),
		tc("process substitution walked", func() (string, string) { return bash("diff <(cat /a/x) /b/y") },
			targetCase{paths: []string{"/a/x", "/b/y"}, opaque: true}),
		tc("pushd swap", func() (string, string) { return bash("pushd /a && pushd /b && pushd && cat f") },
			targetCase{cd: []string{"/a", "/b"}, paths: []string{"/a/f"}}),
		tc("pushd no stack", func() (string, string) { return bash("pushd") }, targetCase{opaque: true}),
		tc("pushd -n", func() (string, string) { return bash("pushd -n /a") }, targetCase{opaque: true}),
		tc("pushd unknown", func() (string, string) { return bash("pushd $D") }, targetCase{opaque: true}),
		tc("array assignment", func() (string, string) { return bash("a=(/a/x /b/y); cat ${a[0]}") },
			targetCase{paths: []string{"/a/x", "/b/y"}, opaque: true}),
		tc("append assignment unsets", func() (string, string) { return bash("D=/a; D+=/b; cd $D") }, targetCase{opaque: true}),
		tc("naked export keeps value", func() (string, string) { return bash("D=/a; export D; cd $D") }, targetCase{cd: []string{"/a"}}),
		tc("assignment from substitution", func() (string, string) { return bash("D=$(pwd); cd $D") }, targetCase{opaque: true}),
		tc("empty assignment", func() (string, string) { return bash("D=; cat /a/$D") }, targetCase{paths: []string{"/a"}}),
		tc("command-scoped assignment", func() (string, string) { return bash("D=/a cat x; cd $D") }, targetCase{opaque: true}),
		tc("default value expansion", func() (string, string) { return bash("cat ${D:-/a/x}") }, targetCase{opaque: true}),
		tc("function body walked", func() (string, string) { return bash("f() { cat /a/x; }; f") }, targetCase{paths: []string{"/a/x"}}),
		tc("background job", func() (string, string) { return bash("cd /a & cat f") },
			targetCase{cd: []string{"/a"}, paths: []string{"/scratch/project/f"}}),
		tc("bash -c not static", func() (string, string) { return bash("bash -c /a/$X") }, targetCase{prefixes: []string{"/a"}, opaque: true}),
		tc("sh -o without value", func() (string, string) { return bash("sh -o") }, targetCase{opaque: true}),
		tc("sh reads stdin", func() (string, string) { return bash("curl x | sh -s") }, targetCase{opaque: true}),
		tc("bash missing script file", func() (string, string) { return bash("bash -e /a/run.sh") },
			targetCase{paths: []string{"/a/run.sh"}, opaque: true}),
		tc("xxd revert", func() (string, string) { return bash("xxd -r x.hex") }, targetCase{opaque: true}),
		tc("node stdin", func() (string, string) { return bash("node") }, targetCase{opaque: true}),
		tc("env -S", func() (string, string) { return bash(`env -S "cat /a/x"`) }, targetCase{opaque: true}),
		tc("git -C not static", func() (string, string) { return bash("git -C /a/$X status") }, targetCase{prefixes: []string{"/a"}, opaque: true}),
		tc("gh -R not static", func() (string, string) { return bash("gh pr list -R $REPO") }, targetCase{opaque: true}),
		tc("gh -R unreadable", func() (string, string) { return bash("gh pr list -R nope") }, targetCase{opaque: true}),
		tc("gh attached -R", func() (string, string) { return bash("gh pr list -Racme/x") }, targetCase{repos: []string{"acme/x"}}),
		tc("make -C not static", func() (string, string) { return bash("make -C $D") }, targetCase{opaque: true}),
		tc("github ownerless url", func() (string, string) { return bash("open https://github.com/settings/tokens") },
			targetCase{opaque: false}),
		tc("plain command is not opaque", func() (string, string) { return bash("ls -la") }, targetCase{}),

		// file tools
		tc("Read", func() (string, string) { return toolIn("Read", `{"file_path":"/scratch/outside/file.txt"}`) },
			targetCase{paths: []string{"/scratch/outside/file.txt"}}),
		tc("Write relative, content searched", func() (string, string) {
			return toolIn("Write", `{"file_path":"a/b.go","content":"see /z/x"}`)
		}, targetCase{paths: []string{"/scratch/project/a/b.go", "/z/x"}}),
		tc("Edit", func() (string, string) {
			return toolIn("Edit", `{"file_path":"/a/x.go","old_string":"a","new_string":"b"}`)
		}, targetCase{paths: []string{"/a/x.go"}}),
		tc("NotebookEdit", func() (string, string) { return toolIn("NotebookEdit", `{"notebook_path":"/a/n.ipynb"}`) },
			targetCase{paths: []string{"/a/n.ipynb"}}),
		tc("Glob absolute pattern", func() (string, string) { return toolIn("Glob", `{"pattern":"/other/**/*.go"}`) },
			targetCase{prefixes: []string{"/other"}}),
		tc("Glob with path", func() (string, string) { return toolIn("Glob", `{"pattern":"**/*.md","path":"/a"}`) },
			targetCase{paths: []string{"/a"}, prefixes: []string{"/a"}}),
		tc("Grep path", func() (string, string) { return toolIn("Grep", `{"pattern":"TODO","path":"~/code/x"}`) },
			targetCase{paths: []string{"/h/u/code/x"}}),
		tc("EnterWorktree", func() (string, string) { return toolIn("EnterWorktree", `{"name":"wt1"}`) },
			targetCase{cd: []string{"/scratch/project/.claude/worktrees/wt1"}}),
		tc("MCP relative_path", func() (string, string) {
			return toolIn("mcp__serena__find_symbol", `{"name_path":"X","relative_path":"internal/a.go"}`)
		}, targetCase{paths: []string{"/scratch/project/internal/a.go"}}),
		tc("WebFetch github url", func() (string, string) {
			return toolIn("WebFetch", `{"url":"https://github.com/globex/api/blob/main/x"}`)
		},
			targetCase{repos: []string{"globex/api"}}),
		tc("Agent has no targets", func() (string, string) {
			return toolIn("Agent", `{"description":"d","prompt":"look at /etc/x","subagent_type":"Explore"}`)
		}, targetCase{notPaths: []string{"/etc/x"}}),
		tc("apply_patch", func() (string, string) {
			return toolIn("apply_patch", `{"command":"*** Begin Patch\n*** Update File: /scratch/outside/file.txt\n@@\n outside\n+edited\n*** Add File: rel/new.txt\n+x\n*** End Patch"}`)
		}, targetCase{paths: []string{"/scratch/outside/file.txt", "/scratch/project/rel/new.txt"}}),
		tc("apply_patch move", func() (string, string) {
			return toolIn("apply_patch", `{"command":"*** Begin Patch\n*** Update File: a.txt\n*** Move to: /b/a.txt\n*** End Patch"}`)
		}, targetCase{paths: []string{"/scratch/project/a.txt", "/b/a.txt"}}),
		tc("shell workdir", func() (string, string) { return toolIn("shell", `{"command":"cat f","workdir":"/a"}`) },
			targetCase{cd: []string{"/a"}, paths: []string{"/a/f"}}),
		tc("shell argv bash -lc", func() (string, string) { return toolIn("shell", `{"command":["bash","-lc","cat /a/x"]}`) },
			targetCase{paths: []string{"/a/x"}}),
		tc("shell argv plain", func() (string, string) { return toolIn("shell", `{"command":["cat","/a/x"]}`) },
			targetCase{paths: []string{"/a/x"}}),
		tc("invalid json", func() (string, string) { return toolIn("Bash", `{not json`) }, targetCase{opaque: true}),
		tc("bash without command", func() (string, string) { return toolIn("Bash", `{}`) }, targetCase{opaque: true}),
	}
}

func TestTargetsTable(t *testing.T) {
	cases := targetCases()
	if len(cases) < 60 {
		t.Fatalf("table has %d cases, want at least 60", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTargets(c.tool, json.RawMessage(c.input), testCwd, testHome)
			check := func(kind string, have, want []string) {
				for _, w := range want {
					if !slices.Contains(have, w) {
						t.Errorf("%s missing %q; have %q", kind, w, have)
					}
				}
			}
			check("paths", got.Paths, c.paths)
			check("cd_dest", got.CdDest, c.cd)
			check("git_dirs", got.GitDirs, c.git)
			check("gh_repos", got.GHRepos, c.repos)
			check("prefixes", got.Prefixes, c.prefixes)
			for _, n := range c.notPaths {
				if slices.Contains(got.Paths, n) {
					t.Errorf("paths has %q, want it absent; have %q", n, got.Paths)
				}
			}
			if got.Opaque != c.opaque {
				t.Errorf("opaque = %v (%q), want %v", got.Opaque, got.OpaqueReasons, c.opaque)
			}
		})
	}
}

func TestTargetsOutputSortedAndUnique(t *testing.T) {
	tool, in := bash("cat /b /a /b; cd /a; cd /a")
	got := extractTargets(tool, json.RawMessage(in), testCwd, testHome)
	if !slices.Equal(got.Paths, []string{"/a", "/b"}) {
		t.Errorf("paths = %q", got.Paths)
	}
	if !slices.Equal(got.CdDest, []string{"/a"}) {
		t.Errorf("cd_dest = %q", got.CdDest)
	}
}

func TestTargetsRelativeCwdIsOpaque(t *testing.T) {
	tool, in := bash("ls")
	if got := extractTargets(tool, json.RawMessage(in), "relative", testHome); !got.Opaque {
		t.Error("relative cwd not flagged opaque")
	}
}

func TestTargetsUnknownHomeIsOpaque(t *testing.T) {
	tool, in := bash("cat ~/x")
	if got := extractTargets(tool, json.RawMessage(in), testCwd, ""); !got.Opaque {
		t.Error("unknown home not flagged opaque")
	}
}

func TestTargetsCapsTargetCount(t *testing.T) {
	cmd := "cat"
	for i := 0; i < maxTargets+10; i++ {
		cmd += " /a/" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
	}
	tool, in := bash(cmd)
	got := extractTargets(tool, json.RawMessage(in), testCwd, testHome)
	if !got.Opaque || len(got.Paths) > maxTargets {
		t.Errorf("opaque=%v paths=%d, want opaque and at most %d", got.Opaque, len(got.Paths), maxTargets)
	}
}

func TestTargetsNestingDepth(t *testing.T) {
	cmd := "cat /a/x"
	for i := 0; i < maxShellDepth+2; i++ {
		b, _ := json.Marshal(cmd)
		cmd = "bash -c " + string(b)
	}
	tool, in := bash(cmd)
	if got := extractTargets(tool, json.RawMessage(in), testCwd, testHome); !got.Opaque {
		t.Error("deep nesting not flagged opaque")
	}
}

// TestTargetsRecordedPayloads runs every recorded PreToolUse payload
// through Targets: each must yield its scenario's target.
func TestTargetsRecordedPayloads(t *testing.T) {
	want := map[string]func(ToolTargets) bool{
		"C02 cd outside project":     func(g ToolTargets) bool { return slices.Contains(g.CdDest, "/scratch/outside") },
		"C04 absolute path in bash":  func(g ToolTargets) bool { return slices.Contains(g.Paths, "/scratch/outside/file.txt") },
		"C05 git -C":                 func(g ToolTargets) bool { return slices.Contains(g.GitDirs, "/scratch/project/sub") },
		"X04 absolute path in shell": func(g ToolTargets) bool { return slices.Contains(g.Paths, "/scratch/outside/file.txt") },
		"X05 git -C":                 func(g ToolTargets) bool { return slices.Contains(g.GitDirs, "/scratch/project/sub") },
		"X08 apply_patch on outside path": func(g ToolTargets) bool {
			return slices.Contains(g.Paths, "/scratch/outside/file.txt")
		},
	}
	seen := map[string]bool{}
	for _, ev := range recordedToolEvents(t) {
		got := Targets(ev.Tool, ev.Input, ev.Cwd)
		if ok, has := want[ev.Scenario]; has && ok(got) {
			seen[ev.Scenario] = true
		}
	}
	for s := range want {
		if !seen[s] {
			t.Errorf("scenario %q: no PreToolUse event yielded its target", s)
		}
	}
}

func TestMapEnvEach(t *testing.T) {
	env := mapEnv{"A": "1", "B": "2"}
	n := 0
	env.Each(func(name string, v expand.Variable) bool {
		n++
		return false
	})
	if n != 1 {
		t.Errorf("Each visited %d after stop, want 1", n)
	}
	if env.Get("C").IsSet() || env.Get("A").Str != "1" {
		t.Error("Get wrong")
	}
}

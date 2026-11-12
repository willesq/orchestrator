package spaces

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// maxTargets bounds the targets one tool call may name before
	// extraction stops and the call is flagged opaque. It also bounds the
	// filesystem work the pre-tool guard does per call.
	maxTargets = 512
	// maxShellDepth bounds nested "bash -c" scripts and script files.
	maxShellDepth = 4
	// maxPathLen and maxComponents bound one path; a longer one is not
	// resolved and makes the call opaque.
	maxPathLen    = 4096
	maxComponents = 256
	// budget bounds the whole extraction. Past it the call is opaque.
	budget = 100 * time.Millisecond
	// maxScan bounds the text searched for paths in one string, and
	// maxVar one variable's value; past them the call is opaque.
	maxScan = 512 << 10
	maxVar  = 64 << 10
)

// ToolTargets is everything a tool call reaches, as far as its input shows
// (spaces plan SP-013). Every path is absolute and physical: symlinks are
// resolved, and a path inside a linked git worktree is listed both as it is
// and as the same path in the worktree's main checkout. Lists are sorted
// and hold no duplicates.
type ToolTargets struct {
	// Paths are files and directories the call reads, writes or runs,
	// including paths written inside its arguments and script files.
	Paths []string `json:"paths,omitempty"`
	// CdDest are directories the call moves into: cd, pushd, popd,
	// make -C, env -C, sudo -D, a shell tool's workdir and EnterWorktree.
	CdDest []string `json:"cd_dest,omitempty"`
	// GitDirs are directories git is pointed at: git -C, --git-dir,
	// --work-tree and GIT_DIR.
	GitDirs []string `json:"git_dirs,omitempty"`
	// GHRepos are GitHub repos as lowercase "owner/repo": gh -R/--repo,
	// GH_REPO, gh api repos/<o>/<r>, gh repo <cmd> <o>/<r>, git remotes
	// on a github host and github.com URLs.
	GHRepos []string `json:"gh_repos,omitempty"`
	// GHOrgs are GitHub accounts (orgs or users), lowercase, the call
	// reaches without naming one repo (SP-043): gh api orgs/<o> and
	// users/<o>, gh repo list <o>, gh search --owner and owner:/org:/user:
	// qualifiers, and github.com/<o> URLs.
	GHOrgs []string `json:"gh_orgs,omitempty"`
	// Prefixes are directories under which the call may reach any path:
	// the fixed part of a glob, or of a word whose value is only known up
	// to a variable or command substitution. "cat ~/code/*/x" reaches
	// every directory under ~/code.
	Prefixes []string `json:"prefixes,omitempty"`
	// Writes are the paths, and prefixes, the call writes (SP-042): a file
	// tool's file, a shell redirection's target, tee's and sed -i's files,
	// cp's and install's destination, mv's operands, rm's and touch's. They
	// are also listed above.
	Writes []string `json:"writes,omitempty"`
	// Opaque is set when part of the call cannot be analysed, so the lists
	// above may be incomplete. OpaqueReasons says which parts.
	Opaque        bool     `json:"opaque,omitempty"`
	OpaqueReasons []string `json:"opaque_reasons,omitempty"`
}

// Targets extracts the targets of one tool call from its hook payload:
// the tool name, its raw tool_input and the cwd the call runs in. It reads
// the filesystem only to resolve symlinks and worktrees and to read the
// shell scripts the call runs, never runs anything, and never fails: input
// it cannot analyse is flagged opaque.
func Targets(tool string, input json.RawMessage, cwd string) ToolTargets {
	home, _ := os.UserHomeDir()
	return extractTargets(tool, input, cwd, home)
}

func extractTargets(tool string, input json.RawMessage, cwd, home string) ToolTargets {
	e := &extractor{home: home, res: newPathResolver(), seen: map[string]bool{},
		written: map[string]bool{}, deadline: time.Now().Add(budget)}
	if !filepath.IsAbs(cwd) {
		e.opaque("cwd is not absolute")
		cwd = "/"
	}
	if !filepath.IsAbs(home) {
		e.opaque("home directory is unknown")
		e.home = "/"
	}
	e.tool(tool, input, cwd)
	return e.result()
}

type targetKind int

const (
	kindPath targetKind = iota
	kindCd
	kindGit
	kindPrefix
)

type extractor struct {
	home     string
	res      *pathResolver
	out      ToolTargets
	seen     map[string]bool
	count    int
	reasons  map[string]bool
	deadline time.Time
	// written holds paths the call writes through redirections, so a
	// script it writes and then runs is not read from the old file.
	written map[string]bool
	// mayCreate is set once the call has run something that can create
	// symlinks from content (ln, archive extraction, git checkout);
	// missing is set when a later path with a '/' did not exist, so it may
	// resolve through a link made by the call itself.
	mayCreate   bool
	missing     bool
	scriptBytes int
	// writing is set while the paths added are written by the call.
	writing int
	// spec is set while walking text that may or may not be shell code,
	// such as a string argument of an unknown program: its targets are
	// kept, but what cannot be read there does not make the call opaque.
	spec int
}

func (e *extractor) opaque(reason string) {
	if e.spec > 0 {
		return
	}
	e.forceOpaque(reason)
}

// forceOpaque flags the call even inside a guess, for limits that stop
// the analysis as a whole.
func (e *extractor) forceOpaque(reason string) {
	if e.reasons == nil {
		e.reasons = map[string]bool{}
	}
	e.reasons[reason] = true
}

func (e *extractor) expired() bool {
	if time.Now().After(e.deadline) {
		e.forceOpaque("analysis took too long")
		return true
	}
	return false
}

// add records an absolute, unresolved path as a target of kind k.
func (e *extractor) add(k targetKind, p string) {
	if p == "" || e.expired() {
		return
	}
	if len(p) > maxPathLen || strings.Count(p, "/") > maxComponents {
		e.opaque("path too long")
		return
	}
	if e.writingNow() && (k == kindPath || k == kindPrefix) {
		e.recordWrite(p)
	}
	key := string(rune('0'+k)) + p
	if e.seen[key] {
		return
	}
	if e.count >= maxTargets {
		e.opaque("too many targets")
		return
	}
	e.seen[key] = true
	e.count++
	if k == kindCd {
		// cd follows the logical path, as bash's default cd -L does.
		p = filepath.Clean(p)
	}
	r := e.res.resolve(p)
	if r.broken {
		e.opaque("too many symlinks")
	}
	// A missing path is suspect only below a directory that does not
	// exist yet, which a link the call creates could stand in for.
	if e.mayCreate && e.spec == 0 && r.missing && !strings.ContainsAny(p, " \t\n") && strings.Contains(strings.Trim(strings.TrimPrefix(r.path, r.existing), "/"), "/") {
		e.missing = true
	}
	list := e.list(k)
	*list = append(*list, r.path)
	if main, ok := e.res.mainCheckout(r.path, r.existing); ok {
		*list = append(*list, main)
	}
}

// creates records that the call runs something that can create symlinks.
// A guess (spec) is not a run.
func (e *extractor) creates() {
	if e.spec == 0 {
		e.mayCreate = true
	}
}

// fileKey names a file the same way however the call spelled it, for
// matching the files it writes against the scripts it runs.
func (e *extractor) fileKey(p string) string { return e.res.resolve(p).path }

func (e *extractor) list(k targetKind) *[]string {
	switch k {
	case kindCd:
		return &e.out.CdDest
	case kindGit:
		return &e.out.GitDirs
	case kindPrefix:
		return &e.out.Prefixes
	}
	return &e.out.Paths
}

func (e *extractor) addRepo(repo string) {
	if e.count >= maxTargets {
		e.opaque("too many targets")
		return
	}
	e.count++
	e.out.GHRepos = append(e.out.GHRepos, strings.ToLower(repo))
}

func (e *extractor) result() ToolTargets {
	if e.missing {
		e.opaque("a path may run through a link the call creates")
	}
	t := e.out
	for _, l := range []*[]string{&t.Paths, &t.CdDest, &t.GitDirs, &t.GHRepos, &t.GHOrgs, &t.Prefixes, &t.Writes} {
		*l = sortedUnique(*l)
	}
	for r := range e.reasons {
		t.OpaqueReasons = append(t.OpaqueReasons, r)
	}
	sort.Strings(t.OpaqueReasons)
	t.Opaque = len(t.OpaqueReasons) > 0
	return t
}

func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	sort.Strings(s)
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// abs joins a path from tool input onto base, expanding a leading ~. It
// concatenates rather than joins so ".." stays for resolve to apply
// physically.
func (e *extractor) abs(base, p string) string {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		return e.home + p[1:]
	case strings.HasPrefix(p, "/"):
		return p
	}
	return base + "/" + p
}

// absAll joins p onto each directory a command may run in.
func (e *extractor) absAll(at where, p string) []string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/") {
		return []string{e.abs("/", p)}
	}
	out := make([]string, len(at.dirs))
	for i, d := range at.dirs {
		out[i] = e.abs(d, p)
	}
	return out
}

func (e *extractor) moveAll(at where, p string) where {
	return where{dirs: e.absAll(at, p), unknown: at.unknown && !strings.HasPrefix(p, "/")}
}

// noFileTools take no paths: their text is a prompt or a query, and a
// subagent's own tool calls are guarded separately.
var noFileTools = flagSet("Agent", "Task", "TodoWrite", "ToolSearch", "AskUserQuestion", "SendMessage",
	"SubagentHandback", "WebSearch", "EnterPlanMode", "ExitPlanMode", "Skill", "ListAgents",
	"collaborationspawn_agent", "collaborationwait_agent")

// tool dispatches on the tool name. Shell tools are parsed; Glob and
// EnterWorktree have their own rules; every other tool, MCP tools
// included, is searched field by field.
func (e *extractor) tool(name string, input json.RawMessage, cwd string) {
	if noFileTools[name] {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		var s string
		if json.Unmarshal(input, &s) == nil && name == "apply_patch" {
			e.writing++
			e.patch(s, where{dirs: []string{cwd}})
			e.writing--
			return
		}
		e.opaque("tool input is not a JSON object")
		return
	}
	at := where{dirs: []string{cwd}}
	e.fileToolWrites(name, obj, at)
	if wd, ok := obj["workdir"].(string); ok && wd != "" {
		at = e.moveAll(at, wd)
		for _, d := range at.dirs {
			e.add(kindCd, d)
		}
	}
	switch name {
	case "Bash", "shell", "local_shell", "container.exec", "exec_command":
		if sh, ok := obj["shell"].(string); ok && sh != "" && !shells[filepath.Base(sh)] {
			e.opaque("shell in a language Targets cannot read")
		}
		cmd := obj["command"]
		if name == "exec_command" {
			cmd = obj["cmd"]
		}
		e.shellTool(cmd, at)
	case "write_stdin":
		e.opaque("input to a running shell")
		e.fields(obj, at, false)
	case "apply_patch":
		s, _ := obj["command"].(string)
		if s == "" {
			s, _ = obj["input"].(string)
		}
		e.writing++
		e.patch(s, at)
		e.writing--
	case "EnterWorktree":
		if n, ok := obj["name"].(string); ok && n != "" {
			e.add(kindCd, cwd+"/.claude/worktrees/"+n)
		}
	case "Glob":
		base := at
		if p, ok := obj["path"].(string); ok && p != "" {
			base = e.moveAll(at, p)
			for _, d := range base.dirs {
				e.add(kindPath, d)
			}
		}
		if p, ok := obj["pattern"].(string); ok && p != "" {
			for _, q := range e.absAll(base, p) {
				e.add(kindPrefix, globPrefix(q))
			}
		}
	default:
		for _, k := range []string{"command", "cmd", "script", "code"} {
			if s, ok := obj[k].(string); ok && s != "" {
				e.opaque("tool runs a command")
				e.shellTool(s, at)
			}
		}
		e.fields(obj, at, false)
	}
}

func (e *extractor) shellTool(cmd any, at where) {
	st := newShellState(at.dirs[0], e.home)
	switch c := cmd.(type) {
	case string:
		e.script(c, st, 0)
	case []any:
		argv := make([]string, 0, len(c))
		for _, a := range c {
			s, ok := a.(string)
			if !ok {
				e.opaque("command array holds a non-string")
				return
			}
			argv = append(argv, s)
		}
		e.argv(argv, st, 0)
	default:
		e.opaque("shell tool has no command")
	}
}

// pathKeys are the field names, matched in lower case by substring, that
// hold paths in file tools and MCP tools: file_path, notebook_path, path,
// relative_path, cwd, directory and the like. pathNames match exactly.
var (
	pathKeys  = []string{"path", "file", "dir", "folder", "cwd", "root"}
	pathNames = flagSet("source", "src", "dest", "destination", "target", "from", "to")
)

// fields walks a tool's input. A string under a path-like key is a path;
// every string is also searched for the paths, file:// URIs and GitHub
// URLs written in it, the whole string included.
func (e *extractor) fields(v any, at where, pathy bool) {
	switch v := v.(type) {
	case map[string]any:
		owner, _ := v["owner"].(string)
		repo, _ := v["repo"].(string)
		if owner != "" && repo != "" {
			// GitHub MCP tools name a repo as owner and repo fields.
			e.ghRepoArg(word{val: owner + "/" + repo})
		}
		for k, x := range v {
			lk := strings.ToLower(k)
			e.fields(x, at, containsAny(lk, pathKeys) || pathNames[lk])
		}
	case []any:
		for _, x := range v {
			e.fields(x, at, pathy)
		}
	case string:
		if p, ok := strings.CutPrefix(v, "file://"); ok && pathy {
			v = p
		}
		if pathy && v != "" {
			for _, p := range e.absAll(at, v) {
				e.add(kindPath, p)
			}
		}
		e.embedded(v, at, true, true)
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// patch extracts the files a Codex apply_patch names. Paths are relative
// to the call's cwd unless absolute.
func (e *extractor) patch(text string, at where) {
	if text == "" {
		e.opaque("apply_patch has no patch text")
		return
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimLeft(line, " \t")
		for _, p := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if rest, ok := strings.CutPrefix(line, p); ok {
				if f := strings.TrimSpace(rest); f != "" {
					for _, q := range e.absAll(at, f) {
						e.add(kindPath, q)
					}
				}
			}
		}
	}
}

// globPrefix returns the directory above the first glob metacharacter in
// p, which is every directory the glob may match under.
func globPrefix(p string) string {
	i := strings.IndexAny(p, "*?[{")
	if i < 0 {
		return p
	}
	return prefixDir(p[:i])
}

// prefixDir returns the directory a partial path ends in: "/a/b" may be
// completed to "/a/bc", so its directory is "/a"; "/a/b/" stays "/a/b/".
func prefixDir(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return filepath.Dir(p)
}

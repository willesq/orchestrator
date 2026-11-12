package spacesettings

import (
	"path/filepath"
	"slices"
	"strings"
)

// Fragment is orch's part of a settings file: values orch sets and list
// entries orch adds, each at a dotted key path ("sandbox.enabled").
type Fragment struct {
	Scalars map[string]any
	Lists   map[string][]string
}

// Empty reports whether the fragment sets nothing.
func (f Fragment) Empty() bool { return len(f.Scalars) == 0 && len(f.Lists) == 0 }

// Claude Code settings keys orch writes. SP-004 measured that a checkout's
// root settings.local.json applies wherever in the checkout a session
// starts, while its settings.json may not, so every key the deny depends on
// is written together (D-53).
const (
	keySandboxOn   = "sandbox.enabled"
	keyUnsandboxed = "sandbox.allowUnsandboxedCommands"
	keyDenyRead    = "sandbox.filesystem.denyRead"
	keyDenyWrite   = "sandbox.filesystem.denyWrite"
	keyAllowWrite  = "sandbox.filesystem.allowWrite"
	keyPermDeny    = "permissions.deny"
	// keyAllProjectMCP turns off starting every project MCP server.
	keyAllProjectMCP = "enableAllProjectMcpServers"
	envGHConfigDir   = "GH_CONFIG_DIR"
	envTmpDir        = "TMPDIR"
	// envClaudeTmpDir is the TMPDIR Claude Code gives sandboxed commands
	// and where it keeps its own temporary files; without it every space's
	// sandboxed commands share /tmp/claude.
	envClaudeTmpDir = "CLAUDE_CODE_TMPDIR"
)

// Env returns the environment a session in the rules' space runs with:
// GH_CONFIG_DIR, TMPDIR and CLAUDE_CODE_TMPDIR, each only when set.
func (r Rules) Env() map[string]string {
	env := map[string]string{}
	if r.GHConfigDir != "" {
		env[envGHConfigDir] = r.GHConfigDir
	}
	if r.TmpDir != "" {
		env[envTmpDir] = r.TmpDir
		env[envClaudeTmpDir] = r.TmpDir
	}
	return env
}

// ClaudeFragment is the Claude Code settings that carry r: the sandbox on
// with no unsandboxed fallback, every denied folder in the sandbox's
// denyRead and denyWrite and as Read(//...) and Edit(//...) permission
// denies (which also cover Grep, Glob, Write and the other file tools), the
// space's own TMPDIR writable, and its environment.
func ClaudeFragment(r Rules) Fragment {
	f := Fragment{
		// Reason for the MCP key: a project's .mcp.json, which the
		// session's file tools can write, would otherwise start servers
		// outside the sandbox when the user enabled every project server.
		Scalars: map[string]any{keySandboxOn: true, keyUnsandboxed: false, keyAllProjectMCP: false},
		Lists:   map[string][]string{},
	}
	if len(r.Deny) > 0 {
		f.Lists[keyDenyRead] = r.Deny
		f.Lists[keyDenyWrite] = r.Deny
		var rules []string
		for _, p := range r.Deny {
			if !RuleSafe(p) {
				// Reason: a glob character would make the rule name other
				// paths; the sandbox deny still holds for shell commands.
				continue
			}
			// "//" makes the path absolute; one "/" is relative to the
			// settings file (SP-004).
			abs := "/" + strings.TrimSuffix(p, "/")
			rules = append(rules, "Read("+abs+"/**)", "Edit("+abs+"/**)")
		}
		if len(rules) > 0 {
			f.Lists[keyPermDeny] = rules
		}
	}
	if r.TmpDir != "" {
		f.Lists[keyAllowWrite] = []string{r.TmpDir}
	}
	for k, v := range r.Env() {
		f.Scalars["env."+k] = v
	}
	return f
}

// CheckoutFragment is ClaudeFragment for a session in checkout, which also
// keeps the session from changing the settings that hold its rules: the
// sandbox denies shell writes to the checkout's .claude folder (so neither
// the files nor the folder can be replaced), and Edit rules deny the file
// tools its two settings files. The session's own file tools may still edit
// the rest of .claude (commands, agents) as project files.
func CheckoutFragment(r Rules, checkout string) Fragment {
	f := ClaudeFragment(r)
	dir := filepath.Join(checkout, claudeDir)
	f.Lists[keyDenyWrite] = append(slices.Clone(f.Lists[keyDenyWrite]), withReal([]string{dir})...)
	for _, name := range []string{localFile, "settings.json"} {
		p := filepath.Join(dir, name)
		if RuleSafe(p) {
			f.Lists[keyPermDeny] = append(f.Lists[keyPermDeny], "Edit(/"+p+")")
		}
	}
	return f
}

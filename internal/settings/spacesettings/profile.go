package spacesettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// envClaudeConfigDir is Claude Code's configuration folder variable.
const envClaudeConfigDir = "CLAUDE_CONFIG_DIR"

// ClaudeLaunch is how 'orch run' starts Claude Code for r's space (D-59).
// Args give the session frag through --settings, as a JSON string before
// the user's own arguments, so no file holds it; nil when frag is empty
// (the sandbox layer off). Env is r's environment plus CLAUDE_CONFIG_DIR
// only when the space names its own Claude Code folder: otherwise the
// session runs with the user's own configuration and login, which
// --settings adds to (Claude Code merges its lists and lets its values
// override the user's and the project's settings).
func ClaudeLaunch(r Rules, frag Fragment) (args []string, env map[string]string, err error) {
	env = r.Env()
	if r.ClaudeConfigDir != "" {
		env[envClaudeConfigDir] = r.ClaudeConfigDir
	}
	if frag.Empty() {
		return nil, env, nil
	}
	m, err := Merge(nil, false, frag, Owned{})
	if err != nil {
		return nil, nil, err
	}
	var b bytes.Buffer
	if err := json.Compact(&b, m.Data); err != nil {
		return nil, nil, err
	}
	return []string{"--settings", b.String()}, env, nil
}

// CodexProfile is the name of the space's Codex permissions profile.
func CodexProfile(space string) string { return "orch_" + strings.ReplaceAll(space, "-", "_") }

// CodexArgs are the config overrides that give a Codex session the rules:
// a permissions profile built on ':workspace' (which lets the shell write
// the workspace and $TMPDIR) that denies every denied folder. SP-004
// measured that Codex has no per-checkout path for this; a profile given
// with -c holds, and a checkout's own config cannot weaken it.
func CodexArgs(r Rules) []string {
	name := CodexProfile(r.Space)
	var fs []string
	for _, p := range r.Deny {
		fs = append(fs, tomlString(p)+"="+tomlString("deny"))
	}
	table := fmt.Sprintf("{extends=%s,filesystem={%s}}", tomlString(":workspace"), strings.Join(fs, ","))
	return []string{
		"-c", "default_permissions=" + tomlString(name),
		"-c", "permissions." + name + "=" + table,
	}
}

// tomlString quotes s as a TOML basic string. JSON's escapes are a subset
// of TOML's.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

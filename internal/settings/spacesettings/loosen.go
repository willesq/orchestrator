package spacesettings

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/willesq/orchestrator/internal/spaces"
)

// Claude Code merges sandbox lists from every settings file it loads, and an
// allowRead or allowWrite entry re-opens the sandbox's denies below it
// (measured with Claude Code 2.1.309: an allowRead in a checkout's own
// .claude/settings.json, or beside the deny in settings.local.json, let a
// sandboxed cat read a folder orch's settings.local.json denied). orch
// cannot change a repository's settings.json and keeps the user's own
// entries, so it reports such entries rather than letting them pass
// silently (RC-215). Only the checkout's own files are read: the user's
// ~/.claude settings, managed settings and other --settings files merge the
// same way but are the user's to trust.

// loosenKeys are the sandbox lists whose entries re-open a deny.
var loosenKeys = []string{keyAllowRead, keyAllowWrite}

// hookEnvKeys are environment variables that move where orch-hook finds the
// guard's files and orchd. Measured with Claude Code 2.1.309 (D-67 re-audit):
// a project settings.json's env reaches the hooks, over the environment
// Claude Code started with, and a settings.local.json's wins over it.
var hookEnvKeys = []string{envOrchData, "ORCH_CONFIG_DIR", "ORCH_SOCKET"}

// Loosening lists the entries of a checkout's .claude/settings.json and
// settings.local.json that would re-open a folder in deny, or turn off or
// redirect the hooks that refuse calls there (disableAllHooks, or an env
// entry of hookEnvKeys), each as a sentence naming the file, key and entry.
// Entries orch wrote (owned) are not counted. A file, or a .claude folder,
// that exists but cannot be checked (a link, not a regular file, too
// large, not JSON) is named too: Claude Code may still load it, so it must
// not pass silently.
func Loosening(checkout string, deny []string, owned Owned) []string {
	dir, err := claudeDirFD(checkout, false)
	if err != nil {
		return []string{fmt.Sprintf("%s: orch cannot check whether its settings re-open denied folders: %v", claudeDir, err)}
	}
	if dir < 0 {
		return nil
	}
	defer unix.Close(dir)
	var out []string
	for _, name := range []string{"settings.json", localFile} {
		rel := filepath.Join(claudeDir, name)
		root, err := settingsRoot(dir, name)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: orch cannot check whether it re-opens denied folders: %v", rel, err))
			continue
		}
		mine := Owned{}
		if name == localFile {
			mine = owned
		}
		for _, key := range loosenKeys {
			v, _ := lookup(root, key).([]any)
			for _, e := range v {
				s, ok := e.(string)
				if !ok || slices.Contains(mine.Lists[key], s) {
					continue
				}
				if d, ok := reopens(s, checkout, deny); ok {
					out = append(out, fmt.Sprintf("%s: %s entry %q re-opens %s, which orch's sandbox rules deny", rel, key, s, d))
				}
			}
		}
		if v, ok := lookup(root, keyNoHooks).(bool); ok && v {
			out = append(out, fmt.Sprintf("%s: %s true turns off orch's tool guard", rel, keyNoHooks))
		}
		for _, k := range hookEnvKeys {
			key := "env." + k
			if raw := lookup(root, key); raw != nil {
				v, str := raw.(string)
				// Reason: orch's own pin counts only while it holds the
				// value orch wrote; any other value, of any type, counts.
				if pr, orchs := mine.Scalars[key]; !orchs || !str || !isString(pr.Set, v) {
					out = append(out, fmt.Sprintf("%s: %s moves where orch's tool guard reads its files", rel, key))
				}
			}
		}
	}
	return out
}

// settingsRoot reads file name in the open .claude folder dir as a JSON
// object; nil when the file is missing.
func settingsRoot(dir int, name string) (map[string]any, error) {
	b, ok, err := readNamed(dir, name)
	if err != nil || !ok {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return root, nil
}

// lookup returns the value at a dotted key path in root, nil when absent.
func lookup(root map[string]any, key string) any {
	var cur any = root
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// reopens reports whether entry, a settings path, holds or lies in a denied
// folder, and which. A pattern counts from the folder before its first
// pattern character, so "/*" holds everything.
func reopens(entry, checkout string, deny []string) (string, bool) {
	p := entry
	if i := strings.IndexAny(p, globMeta); i >= 0 {
		p = filepath.Dir(p[:i] + "x")
	}
	var bases []string
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		// Reason: a settings env can change HOME for the hook, while
		// Claude Code may resolve "~" with the home it started with; both
		// are checked.
		for _, home := range homes() {
			bases = append(bases, filepath.Join(home, strings.TrimPrefix(p, "~")))
		}
	case strings.HasPrefix(p, "/"):
		bases = []string{"/" + strings.TrimLeft(p, "/")}
	default:
		bases = []string{filepath.Join(checkout, p)}
	}
	var forms []string
	for _, b := range bases {
		b = filepath.Clean(b)
		forms = append(forms, b)
		if r, ok := realPath(b); ok && r != b {
			forms = append(forms, r)
		}
	}
	for _, d := range deny {
		for _, f := range forms {
			if spaces.RootHolds(f, d) || spaces.RootHolds(d, f) {
				return d, true
			}
		}
	}
	return "", false
}

// homes are the user's home folders: $HOME and the account's own.
func homes() []string {
	var out []string
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		out = append(out, h)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" && !slices.Contains(out, u.HomeDir) {
		out = append(out, u.HomeDir)
	}
	return out
}

// isString reports whether raw, a JSON value orch wrote, is the string v.
func isString(raw json.RawMessage, v string) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == v
}

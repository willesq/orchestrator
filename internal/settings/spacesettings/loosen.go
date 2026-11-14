package spacesettings

import (
	"encoding/json"
	"fmt"
	"os"
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

// Loosening lists the entries of a checkout's .claude/settings.json and
// settings.local.json that would re-open a folder in deny, each as a
// sentence naming the file, key and entry. Entries orch wrote (owned) are
// not counted. A file, or a .claude folder, that exists but cannot be
// checked (a link, not a regular file, too large, not JSON) is named too:
// Claude Code may still load it, so it must not pass silently.
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
		lists, err := sandboxLists(dir, name)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: orch cannot check whether it re-opens denied folders: %v", rel, err))
			continue
		}
		for _, key := range loosenKeys {
			for _, e := range lists[key] {
				if name == localFile && slices.Contains(owned.Lists[key], e) {
					continue
				}
				if d, ok := reopens(e, checkout, deny); ok {
					out = append(out, fmt.Sprintf("%s: %s entry %q re-opens %s, which orch's sandbox rules deny", rel, key, e, d))
				}
			}
		}
	}
	return out
}

// sandboxLists reads the loosening lists of file name in the open .claude
// folder dir; none when the file is missing.
func sandboxLists(dir int, name string) (map[string][]string, error) {
	b, ok, err := readNamed(dir, name)
	if err != nil || !ok {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	out := map[string][]string{}
	for _, key := range loosenKeys {
		v, _ := lookup(root, key).([]any)
		for _, e := range v {
			if s, ok := e.(string); ok {
				out[key] = append(out[key], s)
			}
		}
	}
	return out, nil
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
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	case strings.HasPrefix(p, "/"):
		p = "/" + strings.TrimLeft(p, "/")
	default:
		p = filepath.Join(checkout, p)
	}
	p = filepath.Clean(p)
	forms := []string{p}
	if r, ok := realPath(p); ok && r != p {
		forms = append(forms, r)
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

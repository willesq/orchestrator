// Package fakeclaude simulates the `claude plugin` commands safety-net runs,
// by editing the same plugin JSON files the real CLI writes. Tests use it so
// no test ever runs the real claude CLI or touches a real ~/.claude.
package fakeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/willesq/orchestrator/internal/safetynet"
)

// Runner is a safetynet.Runner over a fake Claude config directory.
type Runner struct {
	ClaudeDir string
	// Version and SHA are what an install or update records; empty means the pin.
	Version, SHA string
	// Fail makes any command whose joined args start with the key fail.
	Fail map[string]error

	mu    sync.Mutex
	calls [][]string
}

// New returns a runner over claudeDir.
func New(claudeDir string) *Runner { return &Runner{ClaudeDir: claudeDir} }

// Calls returns every command run so far, each as "plugin install ...".
func (r *Runner) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

// Run implements safetynet.Runner.
func (r *Runner) Run(_ context.Context, args []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	joined := strings.Join(args, " ")
	for prefix, err := range r.Fail {
		if strings.HasPrefix(joined, prefix) {
			return "simulated failure", err
		}
	}
	if len(args) < 3 || args[0] != "plugin" {
		return "", fmt.Errorf("unexpected command %q", joined)
	}
	switch {
	case args[1] == "marketplace" && args[2] == "add" && len(args) > 3:
		return "added", r.edit("known_marketplaces.json", func(m map[string]any) {
			m[safetynet.MarketplaceName] = map[string]any{
				"source":          map[string]any{"source": "directory", "path": args[3]},
				"installLocation": args[3],
			}
		})
	case args[1] == "marketplace" && args[2] == "remove" && len(args) > 3:
		name := args[3]
		if err := r.edit("known_marketplaces.json", func(m map[string]any) { delete(m, name) }); err != nil {
			return "", err
		}
		return "removed", r.editPlugins(func(p map[string]any) {
			for id := range p {
				if strings.HasSuffix(id, "@"+name) {
					delete(p, id)
				}
			}
		})
	case args[1] == "marketplace" && args[2] == "update":
		return "updated", nil
	case args[1] == "install" || args[1] == "update":
		id := args[2]
		version, sha := r.Version, r.SHA
		if version == "" {
			version = safetynet.Pinned.Version
		}
		if sha == "" {
			sha = safetynet.Pinned.SHA
		}
		if err := r.editPlugins(func(p map[string]any) {
			p[id] = []any{map[string]any{"scope": "user", "version": version, "gitCommitSha": sha,
				"installPath": filepath.Join(r.ClaudeDir, "plugins", "cache", id)}}
		}); err != nil {
			return "", err
		}
		return "installed", r.setEnabled(id, true)
	case args[1] == "uninstall":
		id := args[2]
		return "uninstalled", r.editPlugins(func(p map[string]any) { delete(p, id) })
	case args[1] == "enable":
		return "enabled", r.setEnabled(args[2], true)
	case args[1] == "disable":
		return "disabled", r.setEnabled(args[2], false)
	}
	return "", fmt.Errorf("unexpected command %q", joined)
}

func (r *Runner) setEnabled(id string, on bool) error {
	return r.editFile(filepath.Join(r.ClaudeDir, "settings.json"), func(m map[string]any) {
		e, _ := m["enabledPlugins"].(map[string]any)
		if e == nil {
			e = map[string]any{}
		}
		e[id] = on
		m["enabledPlugins"] = e
	})
}

func (r *Runner) editPlugins(fn func(map[string]any)) error {
	return r.edit("installed_plugins.json", func(m map[string]any) {
		p, _ := m["plugins"].(map[string]any)
		if p == nil {
			p = map[string]any{}
		}
		fn(p)
		m["version"] = 2
		m["plugins"] = p
	})
}

func (r *Runner) edit(name string, fn func(map[string]any)) error {
	return r.editFile(filepath.Join(r.ClaudeDir, "plugins", name), fn)
}

func (r *Runner) editFile(path string, fn func(map[string]any)) error {
	m := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	fn(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

package install

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/willesq/orchestrator/internal/safetynet"
	"github.com/willesq/orchestrator/internal/settings"
)

// safetyNetRunner builds the runner the plugin steps use. Tests replace it so
// no test ever runs the real claude CLI.
var safetyNetRunner = func(loc safetynet.Locations) safetynet.Runner {
	return safetynet.ExecRunner{Loc: loc}
}

// SetSafetyNetRunner replaces the runner the Claude Code plugin steps use and
// returns a function restoring the previous one. It exists for tests in other
// packages, which must never reach the real claude CLI.
func SetSafetyNetRunner(f func(safetynet.Locations) safetynet.Runner) (restore func()) {
	prev := safetyNetRunner
	safetyNetRunner = f
	return func() { safetyNetRunner = prev }
}

// SafetyNetOptions tune a safety-net plan.
type SafetyNetOptions struct {
	// RulesOnly limits the plan to the guard rule files (Repair rules).
	RulesOnly bool
	// Plugin tunes the plugin commands.
	Plugin safetynet.PlanOptions
	// Runner overrides the runner; nil uses the default.
	Runner safetynet.Runner
}

// SafetyNetPlan is the safety-net part of the Claude Code install on its own,
// for orchd's install action: the marketplace manifest, the orch-guards
// rulebook, the rule.json merge, then the plugin commands.
func SafetyNetPlan(loc safetynet.Locations, opts SafetyNetOptions) Plan {
	p := Plan{Target: "safety-net", Edits: safetyNetRuleEdits(loc)}
	if opts.RulesOnly {
		return p
	}
	p.Edits = append([]Edit{safetyNetManifestEdit(loc)}, p.Edits...)
	p.Steps = func() []Step { return safetyNetSteps(loc, opts.Plugin, opts.Runner, false) }
	return p
}

// safetyNetEdits are the files the Claude Code install writes for safety-net.
func safetyNetEdits(home string) []Edit {
	loc := safetynet.Locate(home)
	return append([]Edit{safetyNetManifestEdit(loc)}, safetyNetRuleEdits(loc)...)
}

// safetyNetManifestEdit writes the orch-owned marketplace manifest.
func safetyNetManifestEdit(loc safetynet.Locations) Edit {
	return Edit{
		Path: loc.ManifestPath(),
		Compute: func([]byte) ([]byte, string, error) {
			b, err := safetynet.Manifest(safetynet.Pinned)
			if err != nil {
				return nil, "", err
			}
			return b, fmt.Sprintf("safety-net: orch-owned marketplace %q pinning %s v%s at %s",
				safetynet.MarketplaceName, safetynet.PluginName, safetynet.Pinned.Version, safetynet.Pinned.SHA[:12]), nil
		},
	}
}

// safetyNetRuleEdits write the orch-guards rulebook, then enable it in
// rule.json. The rulebook goes first so rule.json never names a missing file.
func safetyNetRuleEdits(loc safetynet.Locations) []Edit {
	return []Edit{
		{
			Path: loc.RulebookPath(safetynet.RulebookName),
			Compute: func([]byte) ([]byte, string, error) {
				return safetynet.Rulebook(), "safety-net: orch-owned " + safetynet.RulebookName +
					" rulebook (shared-policy guards; overwritten on upgrade)", nil
			},
			Normalize: safetynet.NormalizeJSON,
		},
		{
			Path:      loc.RuleConfigPath(),
			Compute:   safetynet.MergeRuleConfig,
			Normalize: safetynet.NormalizeJSON,
		},
	}
}

// Steps implements stepper: the plugin commands Claude Code still needs.
//
// Reason: without the claude CLI the steps are skipped with a note rather
// than failing, so `orch install --target all` on a machine without Claude
// Code still wires the other harnesses. `orch status` keeps reporting them.
func (ClaudeCode) Steps(home string) []Step {
	return safetyNetSteps(safetynet.Locate(home), safetynet.PlanOptions{}, nil, true)
}

// safetyNetSteps plans the plugin commands from the current state.
func safetyNetSteps(loc safetynet.Locations, opts safetynet.PlanOptions, r safetynet.Runner, skipWithoutClaude bool) []Step {
	if r == nil {
		r = safetyNetRunner(loc)
	}
	st := safetynet.Detect(loc)
	var steps []Step
	installs := false
	for _, c := range safetynet.PlanCommands(st, loc, opts) {
		if c.Args[1] == "install" || c.Args[1] == "update" {
			installs = true
		}
		steps = append(steps, Step{
			Summary: "safety-net: " + c.Summary + "\n  " + c.String(),
			Run: func(ctx context.Context) (string, error) {
				out, err := r.Run(ctx, c.Args)
				if skipWithoutClaude && errors.Is(err, safetynet.ErrNoClaude) {
					return "skipped: " + err.Error() + "; install Claude Code and re-run orch install --apply", nil
				}
				return out, err
			},
		})
	}
	if installs {
		steps = append(steps, Step{
			Summary: "safety-net: verify the installed commit is the pinned " + safetynet.Pinned.SHA[:12],
			Run:     func(context.Context) (string, error) { return verifyPin(loc, skipWithoutClaude) },
		})
	}
	return steps
}

// verifyPin checks what Claude Code recorded after an install or update.
//
// Reason: the pin holds only if Claude Code honors the marketplace's sha; a
// change in how it resolves sources would install the branch head instead.
// A mismatch fails the install loudly rather than uninstalling, since an
// unpinned copy of the same plugin still guards better than none.
func verifyPin(loc safetynet.Locations, skipMissing bool) (string, error) {
	st := safetynet.Detect(loc)
	switch {
	case st.Installed == nil && skipMissing:
		return "skipped: " + safetynet.PluginID + " is not installed", nil
	case st.Installed == nil:
		return "", fmt.Errorf("%s is not installed after the install step", safetynet.PluginID)
	case st.Installed.SHA == "":
		return "", fmt.Errorf("Claude Code recorded no commit for %s, so the pin cannot be verified", safetynet.PluginID)
	case st.Installed.SHA != safetynet.Pinned.SHA:
		return "", fmt.Errorf("Claude Code installed commit %s, not the pinned %s; check the marketplace at %s",
			st.Installed.SHA, safetynet.Pinned.SHA, loc.MarketplaceDir)
	}
	return "verified " + safetynet.PluginID + " at " + st.Installed.SHA, nil
}

// credentialReadDeny keeps the Read tool off files that hold credentials.
// cc-safety-net already blocks shell access to them; these cover the tools.
var credentialReadDeny = []string{"Read(~/.config/gh/**)", "Read(~/.netrc)", "Read(~/.docker/config.json)"}

// applyCredentialDeny appends the credential deny rules, skipping any present.
// Deny rules only narrow what an agent may do, and never prompt.
func applyCredentialDeny(root map[string]any) (string, error) {
	return applyCredentialDenyWith(root, settings.Toggles{})
}

// applyCredentialDenyWith is applyCredentialDeny leaving out any deny rule
// the user turned off in orch.
func applyCredentialDenyWith(root map[string]any, t settings.Toggles) (string, error) {
	perms, err := permissionsObject(root)
	if err != nil {
		return "", err
	}
	deny, err := ruleList(perms, "deny")
	if err != nil {
		return "", err
	}
	var added, off []string
	for _, rule := range credentialReadDeny {
		if containsRule(deny, rule) {
			continue
		}
		if t.Disabled(ProviderClaudeCode, KindPermission, PermissionIdentity("deny", rule)) {
			off = append(off, rule)
			continue
		}
		deny = append(deny, rule)
		added = append(added, rule)
	}
	note := ""
	if len(off) > 0 {
		note = "; left out " + strings.Join(off, ", ") + " — turned off in orch"
	}
	if len(added) == 0 {
		if len(off) > 0 {
			return "permissions.deny: credential-file Read rules in place" + note, nil
		}
		return "permissions.deny: credential-file Read rules already present", nil
	}
	perms["deny"] = deny
	root["permissions"] = perms
	return "permissions.deny: added " + strings.Join(added, ", ") + " (keeps file tools off credential files)" + note, nil
}

func containsRule(rules []any, want string) bool {
	for _, r := range rules {
		if s, ok := r.(string); ok && s == want {
			return true
		}
	}
	return false
}

// applyAttribution sets attribution.commit to "" so Claude Code adds no
// co-author trailer, per the shared policy, keeping other attribution keys.
func applyAttribution(root map[string]any) (string, error) {
	var attr map[string]any
	switch v := root["attribution"].(type) {
	case nil:
		attr = map[string]any{}
	case map[string]any:
		attr = v
	default:
		return "", fmt.Errorf("settings.json: attribution is %T, not an object — "+
			"refusing to overwrite it; fix or remove it and re-run", v)
	}
	if s, ok := attr["commit"].(string); ok && s == "" {
		return "attribution.commit: already empty (no co-author trailer)", nil
	}
	attr["commit"] = ""
	root["attribution"] = attr
	return `attribution.commit: set to "" so commits carry no coding-agent co-author trailer`, nil
}

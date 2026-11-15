package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// paneParity is testdata/spaces_pane_parity.json: each Spaces pane action
// and its CLI equivalents (SP-055).
type paneParity struct {
	Actions []struct {
		Method string   `json:"method"`
		Pane   string   `json:"pane"`
		CLI    []string `json:"cli"`
		RPCs   []string `json:"rpcs"`
		Access string   `json:"access"`
	} `json:"actions"`
}

func loadPaneParity(t *testing.T) paneParity {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "spaces_pane_parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p paneParity
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

var (
	spacesProtocol = regexp.MustCompile(`(?ms)^public protocol (Spaces\w*API)\b[^{]*\{(.*?)^\}`)
	protocolFunc   = regexp.MustCompile(`(?m)^\s*func (\w+)\(`)
)

// paneMethods reads the Spaces pane's orchd calls from the app's source:
// every method of every OrchKit protocol named Spaces...API, by protocol.
func paneMethods(t *testing.T, dir string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.swift"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Swift sources in %s: %v", dir, err)
	}
	methods := map[string][]string{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range spacesProtocol.FindAllStringSubmatch(string(src), -1) {
			for _, m := range protocolFunc.FindAllStringSubmatch(p[2], -1) {
				methods[m[1]] = append(methods[m[1]], p[1])
			}
		}
	}
	return methods
}

// resolveUsage finds the command a usage line names, walking subcommands
// from the root, and the --flags it uses.
func resolveUsage(root *cobra.Command, usage string) (*cobra.Command, []string, bool) {
	words := strings.Fields(usage)
	if len(words) == 0 || words[0] != "orch" {
		return nil, nil, false
	}
	cmd := root
	i := 1
	for ; i < len(words); i++ {
		next := subcommand(cmd, words[i])
		if next == nil {
			break
		}
		cmd = next
	}
	var flags []string
	for _, w := range words[i:] {
		w = strings.Trim(w, "[]")
		if strings.HasPrefix(w, "--") {
			flags = append(flags, strings.TrimPrefix(w, "--"))
		}
	}
	return cmd, flags, cmd != root
}

func subcommand(c *cobra.Command, name string) *cobra.Command {
	for _, sub := range c.Commands() {
		if sub.Name() == name || sub.HasAlias(name) {
			return sub
		}
	}
	return nil
}

// TestSpacesPaneParity is SP-055.1.a1: every Spaces pane action, read from
// the app's Spaces...API protocols, has an entry in the parity table, and
// each entry's commands and flags exist in orch's command tree.
func TestSpacesPaneParity(t *testing.T) {
	p := loadPaneParity(t)
	methods := paneMethods(t, filepath.Join("..", "..", "macos", "Sources", "OrchKit"))
	// Reason: a pattern that stopped matching would find nothing and pass.
	for _, known := range []string{"applySpaces", "saveSpacesDraft", "setSpaceTerms", "explainPlacement"} {
		if methods[known] == nil {
			t.Fatalf("the protocol scan missed %s; found %v", known, methods)
		}
	}
	for _, problem := range checkPaneParity(NewRootCmd(nil), p, methods) {
		t.Error(problem)
	}
}

// checkPaneParity reports each method with no entry, each entry naming no
// method, and each command or flag that does not exist.
func checkPaneParity(root *cobra.Command, p paneParity, methods map[string][]string) []string {
	var problems []string
	errorf := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	listed := map[string]bool{}
	for _, a := range p.Actions {
		if listed[a.Method] {
			errorf("%s is listed twice", a.Method)
		}
		listed[a.Method] = true
		if methods[a.Method] == nil {
			errorf("%s is in the parity table but no Spaces...API protocol has it", a.Method)
		}
		if a.Pane == "" || len(a.RPCs) == 0 {
			errorf("%s: name the pane action and the RPCs it calls", a.Method)
		}
		switch a.Access {
		case "anyone", "operator", "orch-app":
		default:
			errorf("%s: access %q is not anyone, operator or orch-app", a.Method, a.Access)
		}
		if len(a.CLI) == 0 {
			errorf("%s (%s) has no CLI equivalent", a.Method, a.Pane)
		}
		for _, usage := range a.CLI {
			cmd, flags, ok := resolveUsage(root, usage)
			if !ok || !cmd.Runnable() {
				errorf("%s: %q names no orch command", a.Method, usage)
				continue
			}
			for _, f := range flags {
				if cmd.Flags().Lookup(f) == nil && cmd.InheritedFlags().Lookup(f) == nil {
					errorf("%s: %q: %s has no --%s flag", a.Method, usage, cmd.CommandPath(), f)
				}
			}
		}
	}
	var missing []string
	for m, protocols := range methods {
		if !listed[m] {
			missing = append(missing, m+" ("+strings.Join(protocols, ", ")+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		errorf("Spaces pane action %s has no CLI entry in testdata/spaces_pane_parity.json", m)
	}
	return problems
}

// TestSpacesPaneParityCatches shows the parity check fails on a pane
// action with no entry, a stale entry, and a command or flag that does
// not exist.
func TestSpacesPaneParityCatches(t *testing.T) {
	dir := t.TempDir()
	src := "public protocol SpacesNewAPI: Sendable {\n    func listSpaces() async throws -> X\n    func brandNew() async throws\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "New.swift"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	methods := paneMethods(t, dir)
	if strings.Join(methods["brandNew"], "") != "SpacesNewAPI" || methods["listSpaces"] == nil {
		t.Fatalf("scan = %v", methods)
	}
	var p paneParity
	if err := json.Unmarshal([]byte(`{"actions": [
		{"method": "listSpaces", "pane": "Overview", "rpcs": ["ListSpaces"], "access": "operator", "cli": ["orch spaces list --nope", "orch spaces nothing"]},
		{"method": "gone", "pane": "Old", "rpcs": ["X"], "access": "maybe", "cli": []}
	]}`), &p); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(checkPaneParity(NewRootCmd(nil), p, methods), "\n")
	for _, want := range []string{"brandNew (SpacesNewAPI) has no CLI entry", "no --nope flag", `"orch spaces nothing" names no orch command`,
		"gone is in the parity table but no", `access "maybe"`, "gone (Old) has no CLI equivalent"} {
		if !strings.Contains(got, want) {
			t.Errorf("the check did not report %q:\n%s", want, got)
		}
	}
}

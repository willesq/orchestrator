package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

const settingsOperatorOnly = "queue tasks cannot change Claude Code, cc-safety-net or instruction-module settings"

func newSettingsCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Turn Claude Code hooks, permissions, plugins, cc-safety-net rules and instruction modules on or off",
		Long: `orch keeps the hooks, permission rules and plugins in ~/.claude/settings.json,
the rules of each cc-safety-net rulebook, and orch's instruction modules (the
sections of your agents' startup instructions), as items you can turn on or off.
A change is written to the file at once and remembered in this machine's
orch.db, so orch install does not undo it. Edits made to the files elsewhere
are picked up within a few seconds. The app's Claude tab adds and removes
hooks and permission rules too.`,
	}
	cmd.AddCommand(
		newSettingsListCmd(cf),
		operatorOnlyMsg(settingsOperatorOnly, newSettingsToggleCmd(cf, true)),
		operatorOnlyMsg(settingsOperatorOnly, newSettingsToggleCmd(cf, false)),
	)
	return cmd
}

func newSettingsListCmd(cf clientFactory) *cobra.Command {
	var provider, kind string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the managed settings and whether each is on",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				resp, err := client.ListSettings(ctx, &orchdv1.ListSettingsRequest{Provider: provider, Kind: kind})
				if err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(c.OutOrStdout()).Encode(resp.GetItems())
				}
				printSettings(c.OutOrStdout(), resp)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "only this provider (claude-code, cc-safety-net, orch-instructions)")
	cmd.Flags().StringVar(&kind, "kind", "", "only this kind (hook, permission, plugin, rule, module)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func newSettingsToggleCmd(cf clientFactory, enable bool) *cobra.Command {
	verb, short := "disable", "Turn a setting off (removing it from its file, or recording it as off)"
	if enable {
		verb, short = "enable", "Turn a setting on again, restoring it exactly"
	}
	var provider string
	var yes bool
	cmd := &cobra.Command{
		Use:   verb + " <kind> <id>",
		Short: short,
		Long:  short + ".\nThe id is the ID column of orch settings list.",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			kind, id := args[0], args[1]
			return withOrchd(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				p, item, err := settingsProvider(ctx, client, provider, kind, id)
				if err != nil {
					return err
				}
				if !enable && !yes && sensitiveModule(item) {
					return fmt.Errorf("%s %q carries your agents' security policy; turning it off removes it from every session on this machine — re-run with --yes to confirm", kind, id)
				}
				it, err := client.SetSettingEnabled(ctx, &orchdv1.SetSettingEnabledRequest{
					Provider: p, Kind: kind, Identity: id, Enabled: enable,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "%s %s %s: %s\n", settingState(it), it.GetKind(),
					oneLine(it.GetIdentity(), 200), oneLine(it.GetSummary(), 200))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "the item's provider, when its kind and id are not enough")
	if !enable {
		cmd.Flags().BoolVar(&yes, "yes", false, "confirm turning off a security instruction module")
	}
	return cmd
}

// sensitiveModule reports whether an item is an instruction module marked
// sensitive (the agents' security policy).
func sensitiveModule(it *orchdv1.SettingItem) bool {
	if it == nil || it.GetKind() != "module" {
		return false
	}
	var p struct {
		Sensitive bool `json:"sensitive"`
	}
	return json.Unmarshal([]byte(it.GetPayloadJson()), &p) == nil && p.Sensitive
}

// settingsProvider finds the provider holding kind and id, unless one is
// given, and the item itself.
func settingsProvider(ctx context.Context, client orchdv1.OrchdClient, provider, kind, id string) (string, *orchdv1.SettingItem, error) {
	resp, err := client.ListSettings(ctx, &orchdv1.ListSettingsRequest{Provider: provider, Kind: kind})
	if err != nil {
		return "", nil, err
	}
	var found []*orchdv1.SettingItem
	for _, it := range resp.GetItems() {
		if it.GetIdentity() == id {
			found = append(found, it)
		}
	}
	switch {
	case len(found) == 1:
		return found[0].GetProvider(), found[0], nil
	case provider != "":
		// The daemon reports an unknown item with its own message.
		return provider, nil, nil
	case len(found) == 0:
		return "", nil, fmt.Errorf("no %s %q — see orch settings list", kind, id)
	default:
		names := make([]string, 0, len(found))
		for _, it := range found {
			names = append(names, it.GetProvider())
		}
		return "", nil, fmt.Errorf("%s %q exists in %s; pass --provider", kind, id, strings.Join(names, " and "))
	}
}

// fileList names a provider's files, capped so a provider that reads many
// (instruction modules read every module file) keeps a one-line header.
func fileList(files []string) string {
	const shown = 3
	if len(files) <= shown {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(files[:shown], ", "), len(files)-shown)
}

// settingState is the short state shown for an item.
func settingState(it *orchdv1.SettingItem) string {
	switch it.GetStatus() {
	case "active":
		return "on"
	case "disabled":
		return "off"
	default:
		return it.GetStatus()
	}
}

func printSettings(w io.Writer, resp *orchdv1.ListSettingsResponse) {
	if len(resp.GetItems()) == 0 && len(resp.GetProviders()) == 0 {
		fmt.Fprintln(w, "no settings providers")
		return
	}
	for _, p := range resp.GetProviders() {
		fmt.Fprintf(w, "%s (%s)\n", p.GetTitle(), fileList(p.GetFiles()))
		if p.GetError() != "" {
			fmt.Fprintf(w, "  error: %s\n", oneLine(p.GetError(), 300))
		}
		for _, k := range p.GetKinds() {
			var rows []*orchdv1.SettingItem
			for _, it := range resp.GetItems() {
				if it.GetProvider() == p.GetName() && it.GetKind() == k.GetName() {
					rows = append(rows, it)
				}
			}
			if len(rows) == 0 {
				continue
			}
			fmt.Fprintf(w, "  %s\n", k.GetTitle())
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintf(tw, "    STATE\tORIGIN\t%s\tID\tSUMMARY\n", strings.ToUpper(k.GetGroupTitle()))
			for _, it := range rows {
				fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n", settingState(it), it.GetOrigin(), oneLine(it.GetGroup(), 40),
					oneLine(it.GetIdentity(), 60), oneLine(it.GetSummary(), 80))
			}
			_ = tw.Flush()
		}
	}
}

// oneLine flattens and shortens text from a settings file for a table
// cell. Control and format characters (bidi overrides, zero-width marks)
// become "?", since the text is the file's, not orch's, and must neither
// drive the terminal nor display as something other than what runs.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' || r == 0x7f || r >= 0x80 && r < 0xa0 || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

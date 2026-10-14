package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/models"
)

// modelAdapters builds the discovery adapters; a seam for tests.
var modelAdapters = func() []models.Adapter {
	bin := ""
	if v, err := config.Load(); err == nil {
		if s, err := config.Unmarshal(v); err == nil {
			bin = s.Queue.Worker.ClaudeBin
		}
	}
	// Resolved as the worker resolves it, so the CLI and orchd agree on whether
	// Claude Code is available.
	home, _ := os.UserHomeDir()
	return models.Adapters(agentrun.FindClaude(bin, home))
}

func openModelRegistry() (*models.Registry, func(), error) {
	conn, err := db.Open()
	if err != nil {
		return nil, nil, err
	}
	return models.NewRegistry(conn), func() { _ = conn.Close() }, nil
}

// registryModelCmds are the `orch model` subcommands for the coding-agent model
// registry, alongside `fetch` for the embedding model.
func registryModelCmds() []*cobra.Command {
	return []*cobra.Command{
		newModelRefreshCmd(), newModelListCmd(),
		operatorOnly(newModelStateCmd("enable", models.StateEnabled)), operatorOnly(newModelStateCmd("disable", models.StateDisabled)),
		operatorOnly(newModelTierCmd()),
	}
}

func newModelRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Discover coding-agent models from each CLI's local cache (never enables one)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			reg, closeFn, err := openModelRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			before, err := reg.Models(c.Context(), models.Filter{})
			if err != nil {
				return err
			}
			known := make(map[string]bool, len(before))
			for _, m := range before {
				known[m.Ref()] = true
			}
			refreshErr := models.Refresh(c.Context(), reg, home, modelAdapters())
			after, err := reg.Models(c.Context(), models.Filter{})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			var added int
			for _, m := range after {
				if !known[m.Ref()] {
					added++
					fmt.Fprintf(out, "new: %s (%s)\n", m.Ref(), m.State)
				}
			}
			fmt.Fprintf(out, "%d models, %d new\n", len(after), added)
			if added > 0 {
				fmt.Fprintln(out, "new models stay inert until `orch model enable <provider>/<slug>`")
			}
			return refreshErr
		},
	}
}

func newModelListCmd() *cobra.Command {
	var f models.Filter
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered coding-agent models with state, tier, efforts and provenance",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openModelRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			ms, err := reg.Models(c.Context(), f)
			if err != nil {
				return err
			}
			if len(ms) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no models; run `orch model refresh`")
				return nil
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "MODEL\tSTATE\tTIER\tEFFORT\tSOURCE\tVERIFIED")
			for _, m := range ms {
				state := m.State
				if m.Visibility == "hide" {
					state += " (hidden)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Ref(), state, dash(m.Tier),
					efforts(m), m.Source, age(m.LastVerified))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&f.Provider, "provider", "", "only this provider")
	cmd.Flags().StringVar(&f.State, "state", "", "only this state (discovered, enabled, disabled)")
	return cmd
}

func newModelStateCmd(verb, state string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <provider>/<slug>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a coding-agent model for routing",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			provider, slug, err := models.ParseRef(args[0])
			if err != nil {
				return err
			}
			reg, closeFn, err := openModelRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.SetModelState(c.Context(), provider, slug, state); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s/%s %s\n", provider, slug, state)
			return nil
		},
	}
}

func newModelTierCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tier <provider>/<slug> <frontier|balanced|fast>",
		Short: "Set a model's capability tier (discovery never overwrites it)",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			provider, slug, err := models.ParseRef(args[0])
			if err != nil {
				return err
			}
			reg, closeFn, err := openModelRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.SetTier(c.Context(), provider, slug, args[1]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s/%s tier %s\n", provider, slug, args[1])
			return nil
		},
	}
}

func newProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage coding-agent providers (claude-code, codex) for model routing",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List providers and their state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			reg, closeFn, err := openModelRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			ps, err := reg.Providers(c.Context())
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no providers; run `orch model refresh`")
				return nil
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROVIDER\tSTATE\tREFRESHED\tNOTE")
			for _, p := range ps {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.State, age(p.LastRefreshed), p.StateReason)
			}
			return w.Flush()
		},
	}
	cmd.AddCommand(list, operatorOnly(newProviderStateCmd("enable", models.StateEnabled)),
		operatorOnly(newProviderStateCmd("disable", models.StateDisabled)))
	return cmd
}

func newProviderStateCmd(verb, state string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <provider>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a provider for routing",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, closeFn, err := openModelRegistry()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.SetProviderState(c.Context(), args[0], state); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %s\n", args[0], state)
			return nil
		},
	}
}

func efforts(m models.Model) string {
	if len(m.SupportedEfforts) == 0 {
		return "-"
	}
	parts := make([]string, len(m.SupportedEfforts))
	for i, e := range m.SupportedEfforts {
		parts[i] = e
		if e == m.DefaultEffort {
			parts[i] = e + "*"
		}
	}
	return strings.Join(parts, ",")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Since(time.Unix(unix, 0)).Round(time.Minute).String() + " ago"
}

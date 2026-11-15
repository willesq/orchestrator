package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// `orch spaces on|off|state`: the memory spaces switch (spaces.enabled), off
// by default. orchd changes it only for the operator, confirmed and on its
// own, and applies it at once.

func newSpacesOnCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Turn memory spaces on (operator only)",
		Long: `Turns memory spaces on. orchd first moves every space's memory back into its
own collection, then enforces the applied policy (if any): the tool guard,
outbound terms, sandbox rules, session pins, the command log and memory
filtering. 'orch status' shows the move. Run 'orch install --apply' after, so
Claude Code and Codex hand their tool calls to orchd.

Refused inside an agent session (Claude Code, Codex) and inside tasks orchd
runs; while a policy is applied, only Orch.app may change it.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return setSpacesSwitch(cf, c.OutOrStdout(), true)
		},
	}
}

func newSpacesOffCmd(cf clientFactory) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "off --confirm",
		Short: "Turn memory spaces off, as before spaces (operator only)",
		Long: config.SpacesSwitchHelp + `

Turning spaces off stops every enforcement layer at once and moves every
space's memory, quarantine included, back into one store in the background
('orch status' shows it). Quarantined content becomes readable again. The
policy, its revisions, retags, pins and the command log are kept, inert, for
when spaces are turned on again. Run 'orch install --apply' after, to remove
the tool hooks.

Refused inside an agent session (Claude Code, Codex) and inside tasks orchd
runs; while a policy is applied, only Orch.app may change it.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !confirm {
				return errors.New("turning memory spaces off makes quarantined content readable again; add --confirm to proceed")
			}
			return setSpacesSwitch(cf, c.OutOrStdout(), false)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm turning spaces off (quarantined content becomes readable again)")
	return cmd
}

// setSpacesSwitch asks orchd to switch spaces and says what follows.
func setSpacesSwitch(cf clientFactory, out io.Writer, on bool) error {
	return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
		value := "false"
		if on {
			value = "true"
		}
		if _, err := client.SetConfig(ctx, &orchdv1.SetConfigRequest{
			Values: map[string]string{config.KeySpacesEnabled: value}, ConfirmSensitive: true,
		}); err != nil {
			return err
		}
		if on {
			fmt.Fprintln(out, "memory spaces are turning on: orchd moves memory back into its spaces, then enforces (see 'orch status')")
		} else {
			fmt.Fprintln(out, "memory spaces are off: nothing is enforced and memory is moving back into one store (see 'orch status')")
		}
		fmt.Fprintln(out, "run 'orch install --apply' so Claude Code and Codex match")
		return nil
	})
}

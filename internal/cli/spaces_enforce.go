package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newSpacesEnforceCmd(cf clientFactory) *cobra.Command {
	var forDur time.Duration
	cmd := &cobra.Command{
		Use:   "enforce [<layer> <state>]",
		Short: "Show or switch the enforcement layers (switching is operator only)",
		Long: `Each layer of spaces enforcement has a switch, for isolating behavior when
debugging:

  memory    on | off                                memory filtering
  guard     block-other-jobs | strict | log | off   the tool guard
  terms     on | off                                outbound terms
  sandbox   on | off                                generated sandbox rules
  hooks     on | off                                the tool hooks (pre-tool-use,
                                                    cwd-changed) in Claude Code

The defaults are memory on, guard block-other-jobs, terms on, sandbox on and
hooks on. With no arguments, prints every layer's state.

With --for (e.g. 15m or 1h, at most 24h) the switch reverts by itself to the
state before it. Every change is logged with its time and caller and emits an
event. While a layer is off (or the guard only logs), orch status and every
session's start digest say so. The command log keeps recording tool calls
whatever the switches say.

Switching is refused inside an agent session (Claude Code, Codex) and inside
tasks orchd runs: run it in your own terminal or use Orch.app.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return fmt.Errorf("give a layer and a state, e.g. 'orch spaces enforce guard off --for 15m', or nothing to list them")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				if forDur != 0 {
					return fmt.Errorf("--for needs a layer and a state")
				}
				return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
					rep, err := client.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
					if err != nil {
						return err
					}
					return printSwitches(c.OutOrStdout(), rep)
				})
			}
			if forDur < 0 || (forDur > 0 && forDur < time.Second) {
				return fmt.Errorf("--for must be at least 1s, e.g. 15m")
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				rep, err := client.SetEnforcement(ctx, &orchdv1.SetEnforcementRequest{
					Layer: args[0], State: args[1], ForSeconds: int64(forDur / time.Second),
				})
				if err != nil {
					return err
				}
				out := c.OutOrStdout()
				sw := rep.GetCurrent()
				if !rep.GetChanged() {
					fmt.Fprintf(out, "%s is already %s\n", sw.GetLayer(), sw.GetState())
				} else {
					fmt.Fprintf(out, "%s is now %s%s\n", sw.GetLayer(), sw.GetState(), untilText(sw))
				}
				for _, n := range rep.GetNotes() {
					fmt.Fprintf(out, "note: %s\n", n)
				}
				return nil
			})
		},
	}
	cmd.Flags().DurationVar(&forDur, "for", 0, "revert the switch by itself after this long (e.g. 15m, 1h; at most 24h)")
	return cmd
}

// printSwitches lists every layer's switch.
func printSwitches(out io.Writer, rep *orchdv1.EnforcementState) error {
	if e := rep.GetError(); e != "" {
		fmt.Fprintf(out, "warning: %s\n", e)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LAYER\tSTATE\tDEFAULT\tSINCE\tDETAIL")
	for _, sw := range rep.GetSwitches() {
		since := "-"
		if at := sw.GetSetAt(); at > 0 {
			since = time.Unix(at, 0).Local().Format("2006-01-02 15:04")
		}
		detail := sw.GetTitle()
		if sw.GetRelaxed() {
			detail += "; NOT ENFORCING"
		}
		if p := sw.GetProblem(); p != "" {
			detail += ": " + p
		}
		if u := untilText(sw); u != "" {
			detail += ";" + u
		}
		if by := sw.GetSetBy(); by != "" {
			detail += "; set by " + by
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", cell(sw.GetLayer()), cell(sw.GetState()), cell(sw.GetDefaultState()),
			since, cell(detail))
	}
	return w.Flush()
}

// untilText says when a timed switch reverts, and to what.
func untilText(sw *orchdv1.EnforcementSwitch) string {
	if sw.GetUntil() == 0 {
		return ""
	}
	return fmt.Sprintf(" until %s, then %s",
		time.Unix(sw.GetUntil(), 0).Local().Format("15:04 Jan 2"), strings.TrimSpace(sw.GetRevertsTo()))
}

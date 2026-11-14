package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newSpacesSandboxCmd shows or sets the sandbox setting (spaces D-61).
func newSpacesSandboxCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "sandbox [auto|on|off]",
		Short: "Show or set whether orch writes sandbox rules (setting is operator only)",
		Long: `The sandbox setting is your lasting choice of whether orch writes its OS
sandbox rules into checkouts' .claude/settings.local.json and gives them to
'orch run':

  auto   (default) only for sessions another isolated space's folders
         are denied to; a policy with one isolated space and nothing else
         to keep out writes none
  on     for every space's sessions while a policy is applied
  off    never, by choice

With the rules a space using your default gh login may read that login's
folder inside the sandbox, a space naming its own gh_config_dir reads only
that profile, and gh may reach github.com. Without them, a space naming its
own gh profile still gets GH_CONFIG_DIR in its checkouts; the tool guard,
memory filtering and outbound terms are not affected.

orch status and the app show the setting. Unlike the sandbox switch in
'orch spaces enforce' (for debugging, optionally timed), it does not make
every session's start digest warn. Each change is logged with its time and
caller.

Setting it is refused inside an agent session (Claude Code, Codex) and inside
tasks orchd runs: run it in your own terminal.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				if len(args) == 0 {
					st, err := client.GetEnforcement(ctx, &orchdv1.GetEnforcementRequest{})
					if err != nil {
						return err
					}
					printSandboxSetting(out, st.GetSandboxSetting())
					return nil
				}
				rep, err := client.SetSandboxSetting(ctx, &orchdv1.SetSandboxSettingRequest{Mode: args[0]})
				if err != nil {
					return err
				}
				cur := rep.GetCurrent()
				if rep.GetChanged() {
					fmt.Fprintf(out, "the sandbox setting is now %s\n", cur.GetMode())
				} else {
					fmt.Fprintf(out, "the sandbox setting is already %s\n", cur.GetMode())
				}
				if d := cur.GetDetail(); d != "" {
					fmt.Fprintf(out, "%s\n", d)
				}
				return nil
			})
		},
	}
}

// printSandboxSetting prints the setting in one line, then any problem.
func printSandboxSetting(out io.Writer, s *orchdv1.SandboxSetting) {
	if s == nil {
		fmt.Fprintln(out, "sandbox setting: unknown (orchd did not say)")
		return
	}
	line := fmt.Sprintf("sandbox setting: %s", s.GetMode())
	if s.GetMode() == s.GetDefaultMode() {
		line += " (default)"
	}
	if at := s.GetSetAt(); at > 0 {
		line += "; set " + time.Unix(at, 0).Local().Format("2006-01-02 15:04")
		if by := s.GetSetBy(); by != "" {
			line += " by " + by
		}
	}
	fmt.Fprintln(out, line)
	if d := s.GetDetail(); d != "" {
		fmt.Fprintf(out, "  %s\n", d)
	}
	if p := s.GetProblem(); p != "" {
		fmt.Fprintf(out, "warning: %s\n", p)
	}
}

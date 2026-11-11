package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// newSessionsCmd shows the agent sessions orchd knows (spaces SP-017).
func newSessionsCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Show agent sessions and whether they still run",
	}
	cmd.AddCommand(newSessionsListCmd(cf))
	return cmd
}

func newSessionsListCmd(cf clientFactory) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pinned sessions and running harness processes",
		Long: `Lists the claude and codex sessions orchd pinned at SessionStart, with the
space and directory each is pinned to. orchd reads liveness from the kernel:
a session is active while its harness process runs and a hook event came in
the last 30 minutes, idle while it runs with none for longer, and ended once
SessionEnd came or the process is gone. Running claude, codex or gemini
processes with no pin are listed as unpinned, with the tool calls orchd logged
for that process; read them with 'orch spaces log process:<pid>'. With --all,
the logs of unpinned processes that have since exited are listed as ended.

In a terminal outside any agent session every session is shown; inside a
session only that session's row is.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				rep, err := client.ListSessions(ctx, &orchdv1.ListSessionsRequest{All: all})
				if err != nil {
					return err
				}
				return printSessions(c.OutOrStdout(), rep.GetSessions())
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include ended sessions")
	return cmd
}

func printSessions(out io.Writer, rows []*orchdv1.SessionInfo) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "no sessions")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tSESSION\tPID\tSPACE\tDIRECTORY\tSTATE\tLAST ACTIVITY\tEVENTS\tREFUSALS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			cell(r.GetHarness()), cell(r.GetSessionId()), r.GetHarnessPid(), cell(r.GetSpace()),
			cell(r.GetFirstCwd()), cell(r.GetState()), unixCell(r.GetLastActivity()),
			countCell(r, r.GetToolEvents()), countCell(r, r.GetRefusals()))
	}
	return w.Flush()
}

// cell is a table cell: "-" for empty, and terminal-safe on one line, since a
// directory name can hold any character.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(printableText(s))
}

func unixCell(sec int64) string {
	if sec == 0 {
		return "-"
	}
	return time.Unix(sec, 0).Local().Format("2006-01-02 15:04")
}

// countCell is a count, or "-" for an unpinned process that has logged
// nothing.
func countCell(r *orchdv1.SessionInfo, n int64) string {
	if r.GetSessionId() == "" && r.GetToolEvents() == 0 && r.GetLastActivity() == 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}

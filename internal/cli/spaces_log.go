package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newSpacesLogCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Check the command log of sessions' tool calls",
		Long: `orchd records every tool call and directory change the pre-tool-use and
cwd-changed hooks report, per session or queued task, in an owner-only file
apart from orch.db. The log is never indexed into RAG.`,
	}
	cmd.AddCommand(newSpacesLogVerifyCmd(cf))
	return cmd
}

func newSpacesLogVerifyCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <session id | task:<task id>>",
		Short: "Check that a session's command log was not changed",
		Long: `Recomputes the session's hash chain and reports every record that was
modified, deleted or moved since orchd wrote it, and every chain head
anchored at SessionEnd that no longer matches. Exits non-zero when the
chain is broken. Operator only: run it in a terminal outside any agent
session.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				rep, err := client.VerifyToolLog(ctx, &orchdv1.VerifyToolLogRequest{Owner: args[0]})
				if err != nil {
					return err
				}
				return printToolLogReport(c.OutOrStdout(), args[0], rep)
			})
		},
	}
}

func printToolLogReport(out io.Writer, owner string, rep *orchdv1.VerifyToolLogResponse) error {
	if rep.GetRecords() == 0 && len(rep.GetProblems()) == 0 {
		fmt.Fprintf(out, "%s: no records\n", owner)
		return nil
	}
	fmt.Fprintf(out, "%s: %d record(s), %d chain head(s) anchored at SessionEnd\n", owner, rep.GetRecords(), rep.GetAnchors())
	if rep.GetGaps() > 0 {
		fmt.Fprintf(out, "%d gap record(s): %d event(s) were dropped before orchd could store them\n",
			rep.GetGaps(), rep.GetDropped())
	}
	ps := rep.GetProblems()
	if len(ps) == 0 {
		fmt.Fprintln(out, "chain intact")
		return nil
	}
	fmt.Fprintf(out, "chain broken: %d problem(s)\n", len(ps))
	for _, p := range ps {
		fmt.Fprintf(out, "  record %d: %s\n", p.GetSeq(), p.GetDetail())
	}
	return fmt.Errorf("the command log of %s was changed after orchd wrote it", owner)
}

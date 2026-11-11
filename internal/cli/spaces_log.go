package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newSpacesLogCmd(cf clientFactory) *cobra.Command {
	var after int64
	var limit int
	cmd := &cobra.Command{
		Use:   "log [<session id | task:<task id> | process:<pid>>]",
		Short: "Show or check the command log of sessions' tool calls",
		Long: `orchd records every tool call and directory change the pre-tool-use and
cwd-changed hooks report, per session or queued task, in an owner-only file
apart from orch.db. The log is never indexed into RAG.

With a session ID (or session:<id> for one named like a subcommand),
task:<task id> or process:<pid> (a harness process that held no pin; the
latest such process with that PID, or process:<pid>-<start> for one), prints
that log's records in order. In a terminal outside any
agent session any log can be read; inside a session only that session's own,
which is also what it prints with no argument, and one read at a time: a second
run while one is reading is refused.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			owner := ""
			if len(args) == 1 {
				owner = args[0]
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				return printToolEvents(ctx, c.OutOrStdout(), client, owner, after, limit)
			})
		},
	}
	cmd.Flags().Int64Var(&after, "after", 0, "show records after this sequence number")
	cmd.Flags().IntVar(&limit, "limit", 0, "show at most this many records (0 for all)")
	cmd.AddCommand(newSpacesLogVerifyCmd(cf), newSpacesLogSummaryCmd(cf))
	return cmd
}

// printToolEvents prints owner's records after seq after, a page at a time,
// at most limit of them when limit is above 0.
func printToolEvents(ctx context.Context, out io.Writer, client orchdv1.OrchdClient, owner string, after int64, limit int) error {
	var w *tabwriter.Writer
	shown := 0
	for {
		page := int32(0)
		if limit > 0 {
			page = int32(min(limit-shown, 1000))
		}
		rep, err := client.ListToolEvents(ctx, &orchdv1.ListToolEventsRequest{Owner: owner, AfterSeq: after, Limit: page})
		if err != nil {
			return err
		}
		if w == nil {
			if n := rep.GetPruned(); n > 0 {
				fmt.Fprintf(out, "%s: records up to %d were deleted by retention, except refusals\n", cell(rep.GetOwner()), n)
			}
			if len(rep.GetEvents()) == 0 {
				fmt.Fprintf(out, "%s: no records\n", cell(rep.GetOwner()))
				return nil
			}
			w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SEQ\tTIME\tEVENT\tTOOL\tDECISION\tDETAIL")
		}
		for _, e := range rep.GetEvents() {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", e.GetSeq(),
				time.Unix(0, e.GetAt()).Local().Format("2006-01-02 15:04:05"),
				cell(eventName(e)), cell(e.GetTool()), cell(e.GetDecision()), cell(eventDetail(e)))
			after = e.GetSeq()
			shown++
		}
		if !rep.GetMore() || len(rep.GetEvents()) == 0 || (limit > 0 && shown >= limit) {
			return w.Flush()
		}
	}
}

// eventName is a record's event, or its kind for a gap or end record.
func eventName(e *orchdv1.ToolEvent) string {
	if e.GetKind() != "event" {
		return e.GetKind()
	}
	return e.GetEvent()
}

// eventDetail is what a record says: a call's command, a move's
// directories, or how many events a gap lost.
func eventDetail(e *orchdv1.ToolEvent) string {
	switch {
	case e.GetKind() == "gap":
		return fmt.Sprintf("%d event(s) dropped before orchd could store them", e.GetDropped())
	case e.GetEvent() == "cwd-changed":
		return e.GetOldCwd() + " -> " + e.GetNewCwd()
	}
	return e.GetCommand()
}

func newSpacesLogSummaryCmd(cf clientFactory) *cobra.Command {
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Sum the command log per space: tool calls and directory moves",
		Long: `For each space, over the last week by default: the sessions with events,
their tool calls, their directory moves, the moves into a directory a session
would be pinned to another space for under the policy applied now, and the
calls the tool guard refused. Sessions count in the space they are pinned to;
queued tasks, and sessions pinned with no policy applied, show as "-".
Operator only: run it in a terminal outside any agent session.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if since <= 0 {
				return fmt.Errorf("--since must be positive, e.g. 168h")
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				rep, err := client.SummarizeToolLog(ctx, &orchdv1.SummarizeToolLogRequest{Since: time.Now().Add(-since).Unix()})
				if err != nil {
					return err
				}
				return printToolLogSummary(c.OutOrStdout(), rep)
			})
		},
	}
	cmd.Flags().DurationVar(&since, "since", 7*24*time.Hour, "how far back to sum")
	return cmd
}

func printToolLogSummary(out io.Writer, rep *orchdv1.ToolLogSummary) error {
	fmt.Fprintf(out, "command log from %s to %s\n", unixCell(rep.GetSince()), unixCell(rep.GetUntil()))
	if len(rep.GetSpaces()) == 0 {
		_, err := fmt.Fprintln(out, "no tool events")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SPACE\tSESSIONS\tTOOL CALLS\tDIRECTORY MOVES\tINTO ANOTHER SPACE\tREFUSALS")
	for _, r := range rep.GetSpaces() {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\n", cell(r.GetSpace()), r.GetSessions(), r.GetToolCalls(),
			r.GetDirectoryMoves(), r.GetCrossSpaceMoves(), r.GetRefusals())
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if n := rep.GetUnplacedMoves(); n > 0 {
		fmt.Fprintf(out, "%d directory move(s) were not checked for a space change: too many distinct directories, or the time limit ran out\n", n)
	}
	return nil
}

func newSpacesLogVerifyCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <session id | task:<task id> | process:<pid>>",
		Short: "Check that a session's command log was not changed",
		Long: `Recomputes the session's hash chain and reports every record that was
modified, deleted or moved since orchd wrote it, and every chain head
anchored at its end that no longer matches, either in the command log or in
the copy of each anchor orchd keeps in orch.db. Exits non-zero when the
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
	if rep.GetRecords() == 0 && len(rep.GetProblems()) == 0 && rep.GetPruned() == 0 {
		fmt.Fprintf(out, "%s: no records\n", owner)
		return nil
	}
	fmt.Fprintf(out, "%s: %d record(s), %d chain head(s) anchored at SessionEnd\n", owner, rep.GetRecords(), rep.GetAnchors())
	if n := rep.GetPruned(); n > 0 {
		// Reason: the cut is recorded in the same file, so orchd's log of
		// each cut is the record to compare it with.
		fmt.Fprintf(out, "records up to %d: deleted by retention except refusals (orchd's log names each cut)\n", n)
	}
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

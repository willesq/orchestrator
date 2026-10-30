package cli

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/taskfmt"
)

func newTasksCmd(cf clientFactory) *cobra.Command {
	var (
		statusFilter string
		agentFilter  string
		limit        int
		stats        bool
	)
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "List dispatched agent tasks from the ledger",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if stats {
				// Aggregate over as much of the ledger as one read returns.
				limit = maxStatsTasks
			}
			resp, err := client.ListTasks(ctx, &orchdv1.ListTasksRequest{
				Status: statusFilter,
				Agent:  agentFilter,
				Limit:  clampInt32(limit),
			})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			tasks := resp.GetTasks()
			if len(tasks) == 0 {
				fmt.Fprintln(out, "no tasks recorded")
				return nil
			}
			if stats {
				fmt.Fprint(out, taskfmt.StatsTable(taskfmt.Stats(tasks)))
				if len(tasks) == maxStatsTasks {
					fmt.Fprintf(out, "(based on the %d most recent tasks)\n", maxStatsTasks)
				}
				return nil
			}
			fmt.Fprintf(out, "%-40s %-12s %-10s %s\n", "ID", "STATUS", "AGENT", "UPDATED")
			for _, t := range tasks {
				status := t.GetStatus()
				if tag := taskfmt.RunTag(t); tag != "" {
					status += " " + tag
				}
				fmt.Fprintf(out, "%-40s %-12s %-10s %s\n",
					t.GetId(), status, t.GetAgent(), t.GetUpdatedAt())
				if b := firstLine(t.GetBody()); b != "" {
					fmt.Fprintf(out, "    %s\n", b)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter: pending | in_progress | complete | failed | blocked | interrupted | cancelled")
	cmd.Flags().StringVar(&agentFilter, "agent", "", "filter by target agent")
	cmd.Flags().IntVar(&limit, "limit", 20, "max tasks to show")
	cmd.Flags().BoolVar(&stats, "stats", false, "summarize outcomes per agent: success rate, run time and cost")
	return cmd
}

// maxStatsTasks matches the daemon's cap on one task listing.
const maxStatsTasks = 500

func newTaskCmd(cf clientFactory) *cobra.Command {
	var rate string
	var retry, cancelTask bool
	cmd := &cobra.Command{
		Use:   "task <id>",
		Short: "Show one dispatched agent task, or rate, retry or cancel it",
		Long: `Show one dispatched agent task. For a pending or in-progress task it also
shows whether anything is running it: running (with its process and how long),
waiting for a worker, or stalled, plus its session and the command to open it.

--retry runs a task again; the session is resumed where it left off when one
exists. --cancel stops a pending or in-progress task.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if n := btoi(rate != "") + btoi(retry) + btoi(cancelTask); n > 1 {
				return fmt.Errorf("use only one of --rate, --retry and --cancel")
			}
			// A rating is evidence routing acts on, so a queued session must not
			// grade its own work.
			if id := os.Getenv(agentrun.TaskIDEnv); rate != "" && id != "" {
				return fmt.Errorf("queue tasks cannot rate tasks (running as %s)", id)
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if rate != "" {
				if _, err := client.RateTask(ctx, &orchdv1.RateTaskRequest{Id: args[0], Rating: rate}); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "rated %s %s\n", args[0], rate)
				return nil
			}
			if retry {
				resp, err := client.RetryTask(ctx, &orchdv1.RetryTaskRequest{Id: args[0]})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "retrying %s as run %s; its session resumes where it left off when there is one\n",
					args[0], resp.GetRunId())
				return nil
			}
			if cancelTask {
				resp, err := client.CancelTask(ctx, &orchdv1.CancelTaskRequest{Id: args[0]})
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "cancelled %s: %s\n", args[0], resp.GetOutcome())
				return nil
			}
			resp, err := client.GetTask(ctx, &orchdv1.GetTaskRequest{Id: args[0]})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if !resp.GetFound() {
				fmt.Fprintf(out, "no task with id %s\n", args[0])
				return nil
			}
			t := resp.GetTask()
			fmt.Fprintf(out, "id:      %s\nstatus:  %s\nagent:   %s\nupdated: %s\n%s%s\nbody:\n%s\n\nresult:\n%s\n",
				t.GetId(), t.GetStatus(), t.GetAgent(), t.GetUpdatedAt(), taskfmt.Run(t, time.Now()), taskfmt.Outcome(t),
				t.GetBody(), orNotYet(t.GetResult()))
			return nil
		},
	}
	cmd.Flags().StringVar(&rate, "rate", "", "record your verdict on a finished task: good, rework or bad")
	cmd.Flags().BoolVar(&retry, "retry", false, "run the task again, resuming its session when there is one")
	cmd.Flags().BoolVar(&cancelTask, "cancel", false, "stop a pending or in-progress task")
	return cmd
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// firstLine trims a body to one readable line for list output.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func orNotYet(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none yet)"
	}
	return s
}

// clampInt32 narrows a flag value without wrapping. A silently wrapped limit is
// worse than a capped one: on some paths 0 means "no limit".
func clampInt32(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

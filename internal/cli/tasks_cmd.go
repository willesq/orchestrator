package cli

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newTasksCmd(cf clientFactory) *cobra.Command {
	var (
		statusFilter string
		agentFilter  string
		limit        int
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
			fmt.Fprintf(out, "%-40s %-12s %-10s %s\n", "ID", "STATUS", "AGENT", "UPDATED")
			for _, t := range tasks {
				fmt.Fprintf(out, "%-40s %-12s %-10s %s\n",
					t.GetId(), t.GetStatus(), t.GetAgent(), t.GetUpdatedAt())
				if b := firstLine(t.GetBody()); b != "" {
					fmt.Fprintf(out, "    %s\n", b)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter: pending | in_progress | complete | failed")
	cmd.Flags().StringVar(&agentFilter, "agent", "", "filter by target agent")
	cmd.Flags().IntVar(&limit, "limit", 20, "max tasks to show")
	return cmd
}

func newTaskCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "task <id>",
		Short: "Show one dispatched agent task",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
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
			fmt.Fprintf(out, "id:      %s\nstatus:  %s\nagent:   %s\nupdated: %s\n\nbody:\n%s\n\nresult:\n%s\n",
				t.GetId(), t.GetStatus(), t.GetAgent(), t.GetUpdatedAt(),
				t.GetBody(), orNotYet(t.GetResult()))
			return nil
		},
	}
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

package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/logs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// logsPollInterval is how often `orch logs --follow` asks for new lines. A
// variable so tests need not wait.
var logsPollInterval = time.Second

// maxFollowLines matches the most orchd returns per call, so a burst between
// polls is not cut to the default page.
const maxFollowLines = 5000

func newLogsCmd(cf clientFactory) *cobra.Command {
	var (
		source string
		lines  int
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show orchd's recent log lines, from every source or one",
		Long: `Show the log lines orchd keeps in memory: its own, each supervised
subsystem's (sources-scheduler, pr-babysitter, workflow-worker, ...), and the
qdrant engine's. --source narrows to one; --follow keeps printing new lines.

orchd keeps the most recent 5000 lines; older ones are in the service log file.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			return tailLogs(c.Context(), client, c.OutOrStdout(), source, lines, follow)
		},
	}
	cmd.Flags().StringVarP(&source, "source", "s", "", "only this source, such as orchd, qdrant or pr-babysitter")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "how many recent lines to show first")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines until interrupted")
	return cmd
}

func tailLogs(ctx context.Context, client orchdv1.OrchdClient, out io.Writer, source string, lines int, follow bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req := &orchdv1.TailLogsRequest{Source: source, Limit: clampInt32(lines)}
	for {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := client.TailLogs(callCtx, req)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// After the first read, a gap between what was shown and the oldest line
		// still kept means lines rolled out of memory between polls.
		if req.AfterSeq > 0 && resp.GetOldestSeq() > req.AfterSeq+1 {
			fmt.Fprintf(out, "... %d lines dropped\n", resp.GetOldestSeq()-req.AfterSeq-1)
		}
		for _, e := range resp.GetEntries() {
			fmt.Fprintf(out, "%s %-18s %s\n",
				time.UnixMilli(e.GetAtUnixMs()).Format("2006-01-02 15:04:05"),
				logs.Clean(e.GetSource()), logs.Clean(e.GetMessage()))
		}
		if !follow {
			return nil
		}
		if resp.GetLastSeq() > req.AfterSeq {
			req.AfterSeq = resp.GetLastSeq()
		}
		// Follow everything new, not just the last screenful.
		req.Limit = maxFollowLines
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logsPollInterval):
		}
	}
}

package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
)

// newRAGBackfillCmd dates sessions stored before sessions were dated, by
// handing each one whose transcript is still on disk to orchd to store again.
func newRAGBackfillCmd(cf clientFactory) *cobra.Command {
	var apply bool
	cmd := operatorOnlyMsg("queue tasks cannot backfill the RAG store", &cobra.Command{
		Use:   "backfill-sessions",
		Short: "Store undated sessions again with their dates; plans unless --apply",
		Long: "Sessions stored before sessions were dated show \"date unknown\" and are left out of\n" +
			"date-filtered searches. This hands each one whose transcript file is still on disk to orchd,\n" +
			"which stores it again in the background with every turn dated. Sessions whose transcript is\n" +
			"gone stay undated. Without --apply it only reports what it would do.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
			defer cancel()
			plan, err := planBackfill(ctx, client, paths.SessionsDir())
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "%d session(s) in memory: %d to store again with dates, %d already dated, %d without a transcript on disk\n",
				plan.total, len(plan.todo), plan.dated, plan.missing)
			if !apply || len(plan.todo) == 0 {
				if len(plan.todo) > 0 {
					fmt.Fprintln(out, "plan only; run with --apply to store them again")
				}
				return nil
			}
			dir := handoff.Dir()
			for _, s := range plan.todo {
				if err := handoff.PutSession(dir, s); err != nil {
					return fmt.Errorf("hand off %s: %w", s.ID, err)
				}
			}
			fmt.Fprintf(out, "handed %d session(s) to orchd, which stores them in the background\n", len(plan.todo))
			return nil
		},
	})
	cmd.Flags().BoolVar(&apply, "apply", false, "hand the sessions to orchd (without it, only plan)")
	return cmd
}

type backfillPlan struct {
	todo                  []handoff.Session
	total, dated, missing int
}

// planBackfill finds the sessions in memory that carry no dates, and which of
// them still have a transcript to date them from.
func planBackfill(ctx context.Context, client orchdv1.OrchdClient, sessionsDir string) (backfillPlan, error) {
	resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{UriPrefix: "session://"})
	if err != nil {
		return backfillPlan{}, err
	}
	type found struct {
		tags  []string
		dated bool
	}
	sessions := map[string]*found{}
	for _, src := range resp.GetSources() {
		id, kind, ok := strings.Cut(strings.TrimPrefix(src.GetSourceUri(), "session://"), "/")
		if !ok || !session.ValidID(id) {
			continue
		}
		f := sessions[id]
		if f == nil {
			f = &found{}
			sessions[id] = f
		}
		// The record's tags are kept, so a session stays ranked with its project.
		if kind == "record" || f.tags == nil {
			f.tags = src.GetTags()
		}
		if kind == "transcript" && (src.GetStartedAt() > 0 || src.GetEndedAt() > 0) {
			f.dated = true
		}
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plan := backfillPlan{total: len(ids)}
	for _, id := range ids {
		f := sessions[id]
		if f.dated {
			plan.dated++
			continue
		}
		rec, err := session.ReadRecord(sessionsDir, id)
		if err != nil || rec.TranscriptPath == "" {
			plan.missing++
			continue
		}
		if info, err := os.Stat(rec.TranscriptPath); err != nil || !info.Mode().IsRegular() {
			plan.missing++
			continue
		}
		plan.todo = append(plan.todo, handoff.Session{ID: id, TranscriptPath: rec.TranscriptPath, Tags: f.tags, Record: true})
	}
	return plan, nil
}

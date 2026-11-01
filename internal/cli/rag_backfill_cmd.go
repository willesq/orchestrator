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
	var apply, all bool
	cmd := operatorOnlyMsg("queue tasks cannot backfill the RAG store", &cobra.Command{
		Use:   "backfill-sessions",
		Short: "Store undated sessions again with their dates; plans unless --apply",
		Long: "Sessions stored before sessions were dated show \"date unknown\" and are left out of\n" +
			"date-filtered searches. This hands each one whose transcript file is still on disk to orchd,\n" +
			"which stores it again in the background with every turn dated. Sessions whose transcript is\n" +
			"gone stay undated. Without --apply it only reports what it would do.\n\n" +
			"--all stores every session's transcript again whose file is on disk, not only undated ones,\n" +
			"so one stored before harness context was left out loses it. Records and summaries are kept.\n" +
			"It is safe to repeat: a transcript that would come out the same is not written again. A\n" +
			"session whose file is gone keeps what it has, and prompt context still skips its harness text.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
			defer cancel()
			plan, err := planBackfill(ctx, client, paths.SessionsDir(), all)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if all {
				fmt.Fprintf(out, "%d session(s) in memory: %d transcript(s) to store again, %d without a transcript on disk\n",
					plan.total, len(plan.todo), plan.missing)
			} else {
				fmt.Fprintf(out, "%d session(s) in memory: %d to store again with dates, %d already dated, %d without a transcript on disk\n",
					plan.total, len(plan.todo), plan.dated, plan.missing)
			}
			if !apply || len(plan.todo) == 0 {
				if len(plan.todo) > 0 {
					fmt.Fprintln(out, "plan only; run with --apply to store them again")
				}
				return nil
			}
			dir := handoff.Dir()
			// An entry already waiting asks for at least this, and may also ask for a
			// record and summary a replacement would lose.
			waiting := map[string]bool{}
			if pending, err := handoff.Sessions(dir, time.Now().Add(time.Hour)); err == nil {
				for _, s := range pending {
					waiting[s.ID] = true
				}
			}
			handed := 0
			for _, s := range plan.todo {
				if waiting[s.ID] {
					continue
				}
				if err := handoff.PutSession(dir, s); err != nil {
					return fmt.Errorf("hand off %s: %w", s.ID, err)
				}
				handed++
			}
			fmt.Fprintf(out, "handed %d session(s) to orchd, which stores them in the background\n", handed)
			return nil
		},
	})
	cmd.Flags().BoolVar(&apply, "apply", false, "hand the sessions to orchd (without it, only plan)")
	cmd.Flags().BoolVar(&all, "all", false, "every transcript still on disk, not only undated sessions; leaves records alone")
	return cmd
}

type backfillPlan struct {
	todo                  []handoff.Session
	total, dated, missing int
}

// planBackfill finds the sessions in memory that carry no dates, or with all
// every session, and which of them still have a transcript to store again from.
func planBackfill(ctx context.Context, client orchdv1.OrchdClient, sessionsDir string, all bool) (backfillPlan, error) {
	resp, err := client.ListSources(ctx, &orchdv1.ListSourcesRequest{UriPrefix: "session://"})
	if err != nil {
		return backfillPlan{}, err
	}
	type found struct {
		tags          []string
		dated, stored bool
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
		if kind == "transcript" {
			f.stored = true
			f.dated = src.GetStartedAt() > 0 || src.GetEndedAt() > 0
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
		if f.dated && !all {
			plan.dated++
			continue
		}
		// A transcript someone forgot on purpose is not brought back.
		if all && !f.stored {
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
		plan.todo = append(plan.todo, handoff.Session{ID: id, TranscriptPath: rec.TranscriptPath, Tags: f.tags, Record: !all})
	}
	return plan, nil
}

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
)

// BackfillSessions stores sessions again from their transcripts on disk. It is
// an RPC so that orchd itself refuses a queued task (see callers.go): it can
// replace what every later session retrieves, and a client's check of
// ORCH_TASK_ID is one unset variable away from nothing.
func (s *server) BackfillSessions(ctx context.Context, req *orchdv1.BackfillSessionsRequest) (*orchdv1.BackfillSessionsResponse, error) {
	sources, _, _, err := s.rag.ListSources(ctx, rag.ListSourcesOpts{URIPrefix: "session://"})
	if err != nil {
		return nil, err
	}
	plan := planBackfill(sources, paths.SessionsDir(), req.GetAll(), req.GetSummaries())
	resp := &orchdv1.BackfillSessionsResponse{
		Total: count32(plan.total), Todo: count32(len(plan.todo)), Dated: count32(plan.dated), Missing: count32(plan.missing),
	}
	if !req.GetApply() {
		return resp, nil
	}
	dir := handoff.Dir()
	// An entry already waiting may ask for a record and summary that a
	// replacement would lose, and covers the transcript anyway.
	pending, err := handoff.Sessions(dir, time.Now().Add(time.Hour))
	if err != nil {
		return nil, err
	}
	waiting := map[string]bool{}
	for _, p := range pending {
		waiting[p.ID] = true
	}
	for _, sess := range plan.todo {
		if err := ctx.Err(); err != nil {
			return resp, err
		}
		if waiting[sess.ID] {
			resp.Waiting++
			continue
		}
		if err := handoff.PutSession(dir, sess); err != nil {
			return resp, err
		}
		resp.Handed++
	}
	return resp, nil
}

type backfillPlan struct {
	todo                  []handoff.Session
	total, dated, missing int
}

// planBackfill finds the sessions in memory that carry no dates, or with all
// every session whose transcript is stored, and which of them still have a
// transcript to store again from. A transcript someone forgot on purpose stays
// forgotten under all.
func planBackfill(sources []rag.SourceSummary, sessionsDir string, all, summaries bool) backfillPlan {
	type found struct {
		tags          []string
		dated, stored bool
	}
	sessions := map[string]*found{}
	for _, src := range sources {
		id, kind, ok := strings.Cut(strings.TrimPrefix(src.SourceURI, "session://"), "/")
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
			f.tags = src.Tags
		}
		if kind == "transcript" {
			f.stored = true
			f.dated = !src.StartedAt.IsZero() || !src.EndedAt.IsZero()
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
		if all && !f.stored {
			continue
		}
		rec, err := session.ReadRecord(sessionsDir, id)
		if err != nil || !ownTranscript(rec, id) {
			plan.missing++
			continue
		}
		plan.todo = append(plan.todo, handoff.Session{
			ID: id, TranscriptPath: rec.TranscriptPath, Tags: f.tags, Record: !all, Summarize: all && summaries,
		})
	}
	return plan
}

// ownTranscript reports that a record names a regular file that is this session's
// transcript, as harnesses name them. Records are files any process running as
// the user can write, so this only keeps a stray or swapped record from replacing
// one session with another's text; it is no boundary against such a process.
func ownTranscript(rec *session.Record, id string) bool {
	if rec.SessionID != id || filepath.Base(rec.TranscriptPath) != id+".jsonl" {
		return false
	}
	info, err := os.Lstat(rec.TranscriptPath)
	return err == nil && info.Mode().IsRegular()
}

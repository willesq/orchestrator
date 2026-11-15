package daemon

import (
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// A retag (SP-071, D-14, INV-15) moves one source to the space the operator
// names and keeps it there: orchd records a sticky override in orch.db
// (spaces_retags), then rag.Service.Retag moves the stored chunks with their
// vectors. Later writes of the source honor the override (withRetag). See
// docs/internals/memory.md "Retag".

// retagger is the part of the RAG service a retag runs on.
type retagger interface {
	Retag(ctx context.Context, opts rag.RetagOpts) (rag.RetagReport, error)
}

// maxRetagURI bounds a retagged source URI; orchd's writers make far shorter
// ones.
const maxRetagURI = 4096

// retagRequest is a retag the operator asked for.
type retagRequest struct {
	uri, space, expectFrom string
	confirm                bool
	by                     string
}

// errRetagUnconfirmed refuses a move into public or out of an isolated
// space that was not confirmed.
var errRetagUnconfirmed = errors.New("moving a source into a public space, out of an isolated one or out of quarantine needs confirmation (--confirm)")

// errRetagNotFrom refuses a retag whose source is not where the caller
// expected it.
var errRetagNotFrom = errors.New("the source is not stored where the request expected it; nothing was changed")

// retag checks req against the applied policy, records the override and
// moves the source. It shares the migration lock, so it never races a
// migration, a keyword re-index or a relabel.
func (s *server) retag(ctx context.Context, req retagRequest) (rag.RetagReport, error) {
	rt, ok := s.migrator.(retagger)
	if !ok || s.spaces == nil {
		return rag.RetagReport{}, grpcstatus.Error(codes.FailedPrecondition, "retag is unavailable")
	}
	switch {
	case req.uri == "":
		return rag.RetagReport{}, grpcstatus.Error(codes.InvalidArgument, "name the source_uri to retag")
	case len(req.uri) > maxRetagURI:
		return rag.RetagReport{}, grpcstatus.Error(codes.InvalidArgument, "the source_uri is too long")
	}
	res, rev, err := s.spaces.CurrentRevision()
	switch {
	case errors.Is(err, spaces.ErrBroken):
		return rag.RetagReport{}, grpcstatus.Error(codes.FailedPrecondition, "the applied spaces policy does not verify; apply a revision again, then retag")
	case err != nil:
		log.Printf("orchd: retag: reading the applied spaces policy: %v", err)
		return rag.RetagReport{}, grpcstatus.Error(codes.Unavailable, "the applied spaces policy could not be read; see orchd's log")
	case rev == 0 || !res.Configured():
		return rag.RetagReport{}, grpcstatus.Error(codes.FailedPrecondition, "no spaces policy is applied, so there is no space to retag to")
	}
	if _, known := res.Space(req.space); !known && req.space != spaces.Quarantine {
		return rag.RetagReport{}, grpcstatus.Errorf(codes.InvalidArgument, "%q is not a space of the applied policy", req.space)
	}
	if !migrating.TryLock() {
		return rag.RetagReport{}, grpcstatus.Error(codes.FailedPrecondition, "a hybrid migration, backup drop, keyword re-index, relabel or retag is already running")
	}
	defer migrating.Unlock()
	check := func(from []string) error {
		// Reason: applying a policy does not take the migration lock, so
		// the kinds checked here must be the ones the move is stamped with.
		if _, now, err := s.spaces.CurrentRevision(); err != nil || now != rev {
			return grpcstatus.Error(codes.Aborted, "the applied spaces policy changed; run the retag again")
		}
		if req.expectFrom != "" && !(slices.Contains(from, req.expectFrom) && onlyIn(from, req.expectFrom, req.space)) {
			return grpcstatus.Error(codes.FailedPrecondition, errRetagNotFrom.Error())
		}
		if !req.confirm && needsConfirm(res, req.space, from) {
			return grpcstatus.Error(codes.FailedPrecondition, errRetagUnconfirmed.Error())
		}
		// Reason: the override is stored before anything moves, so a
		// write that comes after the move, or an exit mid-move, cannot
		// place the source back where it was; a store that cannot record
		// it refuses the retag.
		if err := s.spaces.SaveRetag(ctx, req.uri, req.space, from, req.confirm, req.by); err != nil {
			log.Printf("orchd: retag: recording the override: %v", err)
			return grpcstatus.Error(codes.Unavailable, "the retag could not be recorded; nothing was moved")
		}
		return nil
	}
	rep, err := rt.Retag(ctx, rag.RetagOpts{SourceURI: req.uri, Space: req.space, Rev: rev, Check: check})
	if err != nil {
		return rep, retagStatus(err)
	}
	if err := s.spaces.FinishRetag(ctx, req.uri, req.space); err != nil {
		// The move is done; the row stays 'moving' and the next start
		// runs it again, which only stamps.
		log.Printf("orchd: retag: marking the move done: %v", err)
	}
	log.Printf("orchd: %s retagged a source to %s (%d chunk(s), was in %s)", req.by, req.space, rep.Chunks, strings.Join(rep.From, ", "))
	return rep, nil
}

// onlyIn reports whether every space in from is a or b.
func onlyIn(from []string, a, b string) bool {
	for _, f := range from {
		if f != a && f != b {
			return false
		}
	}
	return true
}

// needsConfirm reports whether moving a source held in from to space
// needs the operator's confirmation: into a public space, which every
// session reads; out of an isolated one, whose job the content belonged
// to; or out of quarantine or the unplaced base into a space, since content
// of unknown origin may be another job's.
func needsConfirm(res *spaces.Resolved, space string, from []string) bool {
	if space == spaces.Quarantine {
		return slices.ContainsFunc(from, func(f string) bool {
			sp, ok := res.Space(f)
			return ok && sp.Kind == spaces.Isolated
		})
	}
	if sp, ok := res.Space(space); !ok || sp.Kind == spaces.Public {
		return true
	}
	for _, f := range from {
		if f == space {
			continue
		}
		if f == spaces.Quarantine || f == rag.Unplaced {
			return true
		}
		if sp, ok := res.Space(f); ok && sp.Kind == spaces.Isolated {
			return true
		}
	}
	return false
}

// retagStatus turns a retag's error into what the caller gets: refusals
// keep their status; the store's errors are already generic.
func retagStatus(err error) error {
	if _, ok := grpcstatus.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, rag.ErrRetagNotStored):
		return grpcstatus.Error(codes.NotFound, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return grpcstatus.FromContextError(err).Err()
	}
	return grpcstatus.Error(codes.Aborted, err.Error()+"; run the retag again to finish it")
}

// RetagSource is the RPC: operator only, since it moves a source between
// spaces and its answer says which spaces held it.
func (s *server) RetagSource(ctx context.Context, req *orchdv1.RetagSourceRequest) (*orchdv1.RetagSourceResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_RetagSource_FullMethodName, "retag a source"); err != nil {
		return nil, err
	}
	rep, err := s.retag(ctx, retagRequest{uri: req.GetSourceUri(), space: req.GetSpace(), expectFrom: req.GetExpectFrom(),
		confirm: req.GetConfirm(), by: callerLabel(ctx)})
	if err != nil {
		return nil, err
	}
	return &orchdv1.RetagSourceResponse{From: rep.From, Chunks: int64(rep.Chunks), Moved: rep.Moved}, nil
}

// resumeRetagsDelay lets orchd's start settle (Qdrant, the first requests)
// before interrupted retags are finished.
const resumeRetagsDelay = 30 * time.Second

// resumeRetags finishes, after a delay, every retag whose move an exit
// interrupted. It retries while the migration lock is busy.
func (s *server) resumeRetags(ctx context.Context, delay time.Duration) {
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.resumePendingRetags(ctx) {
			return
		}
		t.Reset(delay)
	}
}

// resumePendingRetags runs each pending retag again under the current
// policy. A space the policy no longer has sends the source to quarantine,
// as its override now does; a move that now needs confirmation the
// operator did not give is left pending for the operator to run again. It
// reports whether it is done; false means the migration lock was busy.
func (s *server) resumePendingRetags(ctx context.Context) bool {
	rt, ok := s.migrator.(retagger)
	if !ok || s.spaces == nil {
		return true
	}
	res, rev, err := s.spaces.CurrentRevision()
	if err != nil || rev == 0 || !res.Configured() {
		// Without a policy that verifies there is nothing to stamp them
		// with; they resume after the operator applies one and orchd
		// starts again.
		return true
	}
	if !migrating.TryLock() {
		return false
	}
	defer migrating.Unlock()
	// Read under the lock: a retag the operator runs meanwhile holds it, so
	// a row it replaced is never moved to its old target.
	pending, err := s.spaces.PendingRetags(ctx)
	if err != nil {
		log.Printf("orchd: retag: listing interrupted retags: %v", err)
		return true
	}
	for _, r := range pending {
		to := r.Space
		if _, known := res.Space(to); !known {
			to = spaces.Quarantine
		}
		check := func(from []string) error {
			if !r.Confirmed && needsConfirm(res, to, from) {
				return errRetagUnconfirmed
			}
			return nil
		}
		_, err := rt.Retag(ctx, rag.RetagOpts{SourceURI: r.SourceURI, Space: to, Rev: rev, Check: check})
		switch {
		case errors.Is(err, errRetagUnconfirmed):
			log.Printf("orchd: retag: an interrupted retag to %s now needs confirmation; run it again with --confirm", r.Space)
			continue
		case err != nil && !errors.Is(err, rag.ErrRetagNotStored):
			log.Printf("orchd: retag: resuming a retag to %s: %v", r.Space, err)
			continue
		}
		if err := s.spaces.FinishRetag(ctx, r.SourceURI, r.Space); err != nil {
			log.Printf("orchd: retag: marking a resumed move done: %v", err)
		}
	}
	return true
}

// withRetag applies the operator's sticky retag of m's source, if any, to
// p. orchd's own writers (docs sync, sources refresh, hand-offs, summaries)
// always honor it, and so does a relabel. A client's write honors it only
// when the operator wrote it or the writer's own space is the retag's
// target (RC-850): otherwise a session of one job could write its text under
// a retagged URI into another job's space. Such a write is placed by its
// writer, and mayReplace then refuses it.
func (w *writePlacer) withRetag(ctx context.Context, res *spaces.Resolved, m *rag.Meta, p spaces.Placement) (spaces.Placement, error) {
	if m.SourceURI == "" {
		return p, nil
	}
	over, err := w.spaces.RetagOf(ctx, m.SourceURI)
	if err != nil {
		log.Printf("orchd: reading a source's retag: %v", err)
		return p, errRetagUnreadable
	}
	if over == "" || !w.honorsRetag(m, p, over) {
		return p, nil
	}
	return spaces.ApplyOverride(p, res, over), nil
}

// errRetagUnreadable refuses a write whose source's retag could not be
// read: placing it without the retag could move the source back.
var errRetagUnreadable = grpcstatus.Error(codes.Unavailable, "the source's placement could not be read; see orchd's log, then write it again")

// honorsRetag reports whether m's write, placed at p without the retag,
// follows a retag to over:
//   - the docs sync and the sources refresh always do: they place by what
//     GitHub, a checkout or a bound prefix says, never by the writer;
//   - a relabel does for content stored where the retag put it (the stored
//     space is over), and otherwise follows the rules below by its writer;
//   - a session's records and an edited file's hand-off do only when they
//     were placed (not quarantined, in conflict or placed only by a
//     checkout's remote): a write placed by a session ID, a file or a .git
//     any process can make must not borrow the retag's space;
//   - a client's note does only from the operator or from a session whose
//     space is over (RC-850 option (a)): otherwise a session of one job
//     could write its text under a retagged URI into another job's space.
func (w *writePlacer) honorsRetag(m *rag.Meta, p spaces.Placement, over string) bool {
	switch {
	case rag.IsSyncWriter(m.Writer):
		// A chat feed is placed by orchd's tenant binding, as a sync by
		// what it verified, so an operator's retag holds for it too.
		return true
	case w.relabel && m.Space == over:
		return true
	case m.Writer == rag.WriterClient:
		return m.Origin.Caller == string(callerOperator) || (m.Origin.WriterSpace != "" && m.Origin.WriterSpace == over)
	}
	// Reason: a file placed only by a checkout's remote, or claimed by two
	// spaces, is placed by a .git any agent can write (remoteSteered).
	return p.Outcome == spaces.Placed && !remoteSteered(m, p)
}

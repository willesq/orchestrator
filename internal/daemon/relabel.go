package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
)

// A relabel (SP-027, INV-08) places stored content again under the applied
// policy and moves it to its space's collection: content stored before
// placement existed, before collections were split (SP-024), or under an
// older revision. Content that cannot be placed goes to quarantine (INV-03).
// See docs/internals/memory.md "Relabel".

// relabeler is the part of the RAG service a relabel runs on.
type relabeler interface {
	Relabel(ctx context.Context, opts rag.RelabelOpts) (rag.RelabelReport, error)
}

// relabel brings every stored chunk up to the applied policy; without apply
// it only places and counts. It shares the migration lock, so it never races
// a migration's copy or a keyword re-index.
func (s *server) relabel(ctx context.Context, apply bool) (rag.RelabelReport, error) {
	rl, ok := s.migrator.(relabeler)
	if !ok || s.spaces == nil {
		return rag.RelabelReport{}, errors.New("relabel is unavailable")
	}
	_, rev, err := s.spaces.CurrentRevision()
	switch {
	case errors.Is(err, spaces.ErrBroken):
		// Reason: with no policy that verifies everything would be placed
		// in quarantine; content stays stale (unreadable) until the
		// operator applies a revision again.
		return rag.RelabelReport{}, errors.New("the applied spaces policy does not verify; apply a revision again, then relabel")
	case err != nil:
		log.Printf("orchd: relabel: reading the applied spaces policy: %v", err)
		return rag.RelabelReport{}, errors.New("the applied spaces policy could not be read; see orchd's log")
	case rev == 0:
		return rag.RelabelReport{}, errors.New("no spaces policy is applied, so nothing needs relabelling")
	}
	if !migrating.TryLock() {
		return rag.RelabelReport{}, errors.New("a hybrid migration, backup drop, keyword re-index or relabel is already running")
	}
	defer migrating.Unlock()
	p := relabelPlacer{w: &writePlacer{spaces: s.spaces, pins: s.pins, repos: s.repos, relabel: true, files: s.files}, rev: rev}
	return rl.Relabel(ctx, rag.RelabelOpts{Rev: rev, Place: p.place, Apply: apply})
}

// relabelPlacer places a stored source again for a relabel under revision
// rev.
type relabelPlacer struct {
	w   *writePlacer
	rev int64
}

// place is a rag.PlaceFunc. A source is placed from the origin stored with
// it or, for content stored before origins were recorded (RC-124), from what
// its writer and URI say (storedOrigin). Content with neither, and content
// two spaces claim equally, is quarantined: a relabel never leaves content
// readable where it cannot be placed (INV-03). Any other failure leaves the
// source stale, so the next run tries again.
func (p relabelPlacer) place(ctx context.Context, m *rag.Meta) error {
	if _, rev, err := p.w.spaces.CurrentRevision(); err != nil || rev != p.rev {
		return fmt.Errorf("the applied policy changed during the relabel (revision %d); run it again", rev)
	}
	if m.Origin.System == "" {
		m.Origin = storedOrigin(ctx, p.w.repos, *m)
		if m.Origin.System == rag.OriginGitHub && m.Origin.Visibility == "" {
			// Reason: the reconstructed origin is stored, so a lookup that
			// failed or ran out of budget would fix a degraded placement
			// for good; the source stays stale for the next run instead.
			return errors.New("the repo's visibility could not be looked up")
		}
	}
	if m.Origin.System == "" {
		m.Space = spaces.Quarantine
		return nil
	}
	err := p.w.place(ctx, m)
	if errors.Is(err, errWriteConflict) {
		m.Space = spaces.Quarantine
		return nil
	}
	return err
}

// storedOrigin reconstructs the origin of content stored before writers
// recorded one, from the writer orchd stored with it and the URI form that
// writer uses. Reason: the writer is set by orchd alone, so only a URI an
// orchd writer made is read; a client's note naming another space's URI, or
// a chunk stored before writers were recorded, gets no origin and is
// quarantined.
func storedOrigin(ctx context.Context, repos *repoLookups, m rag.Meta) rag.Origin {
	uri := m.SourceURI
	switch m.Writer {
	case rag.WriterDocs:
		if owner, name, _, ok := splitRepoURI(uri); ok {
			// The repo's visibility is asked of GitHub again: it was not
			// stored. Unknown visibility is never treated as public.
			r := repos.lookup(ctx, owner, name)
			return rag.Origin{System: rag.OriginGitHub, RepoOwner: r.Owner, RepoName: r.Name, Visibility: string(r.Visibility)}
		}
	case rag.WriterSources:
		if path, ok := strings.CutPrefix(uri, "checkout://"); ok && sources.CheckoutURI(path) == uri && filepath.IsAbs(path) {
			return rag.Origin{System: rag.OriginFS, Path: path}
		}
		if isWebURL(uri) {
			return rag.Origin{System: rag.OriginWeb, URL: uri}
		}
	case rag.WriterFile:
		path := ""
		if p, ok := strings.CutPrefix(uri, "file://"); ok && filepath.IsAbs(p) && filepath.Clean(p) == p {
			path = p
		}
		if rel, ok := strings.CutPrefix(uri, "claude://"); ok {
			if home, err := os.UserHomeDir(); err == nil {
				p := filepath.Join(home, ".claude", rel)
				if strings.HasPrefix(p, filepath.Join(home, ".claude")+string(filepath.Separator)) {
					path = p
				}
			}
		}
		// Reason: the hand-off named the file by the path it was edited at
		// but placed what that path resolved to then. A path that resolves
		// elsewhere now (a link) or is gone cannot say where it was placed,
		// and following a link today would let whoever re-points it choose
		// the space.
		if resolved, err := filepath.EvalSymlinks(path); path != "" && err == nil && resolved == path {
			return rag.Origin{System: rag.OriginFS, Path: path}
		}
	case rag.WriterSession:
		// A session's own record or transcript goes where the session is
		// pinned (the session table), under the check a live write gets:
		// the transcript its record names must be where its harness keeps
		// that session's transcript (sessionOrigin). Otherwise, or with no
		// pin, it is quarantined.
		if rest, ok := strings.CutPrefix(uri, "session://"); ok {
			if id, kind, ok := strings.Cut(rest, "/"); ok && session.ValidID(id) && (kind == "record" || kind == "transcript") {
				rec, err := session.ReadRecord(paths.SessionsDir(), id)
				if err != nil {
					return rag.Origin{System: rag.OriginSession}
				}
				return sessionOrigin(id, rec.Cwd, rec.TranscriptPath)
			}
		}
	case rag.WriterClient:
		// A client's note is placed by its writer's space, which was not
		// recorded: it is quarantined until the operator retags it.
		return rag.Origin{System: rag.OriginClient}
	}
	return rag.Origin{}
}

// Reason (SP-027): a relabel moves every space's content and its report
// names spaces; it is also refused to interactive harness sessions.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_Relabel_FullMethodName, "relabel the RAG store",
	spaceDecision{spaceOperator, "relabels the RAG store; its report counts chunks per space"}))

// Relabel is the RPC: operator only, since the report names spaces and the
// run moves every space's content. A run that fails part-way still reports
// what it did.
func (s *server) Relabel(ctx context.Context, req *orchdv1.RelabelRequest) (*orchdv1.RelabelResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_Relabel_FullMethodName, "relabel the RAG store"); err != nil {
		return nil, err
	}
	rep, err := s.relabel(ctx, req.GetApply())
	if err != nil && rep.Rev == 0 {
		// Refused before anything was read.
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	resp := &orchdv1.RelabelResponse{Revision: rep.Rev, Total: int64(rep.Total), Stale: int64(rep.Stale),
		Changed: int64(rep.Changed), Moved: int64(rep.Moved), Failed: int64(rep.Failed), Applied: rep.Applied}
	for _, t := range rep.Transitions {
		resp.Transitions = append(resp.Transitions, &orchdv1.RelabelTransition{From: t.From, To: t.To, Chunks: int64(t.Chunks)})
	}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp, nil
}
